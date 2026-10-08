package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationsAndMoneyConstraints(t *testing.T) {
	dsn := os.Getenv("SETTLEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SETTLEKIT_TEST_DATABASE_URL is required for PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()
	if err := ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("ApplyMigrations: %v", err)
	}
	if err := ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("ApplyMigrations second run: %v", err)
	}
	var versions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil || versions != 2 {
		t.Fatalf("migration ledger: count=%d err=%v", versions, err)
	}

	escrowID, payer, payee, token := randomBytes(t, 32), repeated(0x02, 20), repeated(0x03, 20), repeated(0x04, 20)
	chainID := time.Now().UnixNano()
	expiry := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	insert := `INSERT INTO payment_intents
		(id, merchant_principal, escrow_id, chain_id, escrow_contract, payer, payee, token, amount, status, expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
	args := []any{randomUUID(t), "merchant", escrowID, chainID, repeated(0x09, 20), payer, payee, token, "1000000", "AWAITING_CHAIN", expiry}
	if _, err := pool.Exec(ctx, insert, args...); err != nil {
		t.Fatalf("insert valid intent: %v", err)
	}
	duplicate := append([]any(nil), args...)
	duplicate[0] = randomUUID(t)
	expectSQLState(t, execError(pool.Exec(ctx, insert, duplicate...)), "23505")

	invalidAmount := append([]any(nil), args...)
	invalidAmount[0] = randomUUID(t)
	invalidAmount[2] = randomBytes(t, 32)
	invalidAmount[8] = "-1"
	expectSQLState(t, execError(pool.Exec(ctx, insert, invalidAmount...)), "23514")

	tooLarge := append([]any(nil), invalidAmount...)
	tooLarge[0] = randomUUID(t)
	tooLarge[2] = randomBytes(t, 32)
	tooLarge[8] = "340282366920938463463374607431768211456"
	expectSQLState(t, execError(pool.Exec(ctx, insert, tooLarge...)), "23514")

	badAddress := append([]any(nil), invalidAmount...)
	badAddress[0] = randomUUID(t)
	badAddress[2] = randomBytes(t, 32)
	badAddress[5] = repeated(0x02, 19)
	badAddress[8] = "100"
	expectSQLState(t, execError(pool.Exec(ctx, insert, badAddress...)), "23514")

	hashA, hashB, parent := randomBytes(t, 32), randomBytes(t, 32), randomBytes(t, 32)
	if _, err := pool.Exec(ctx, `INSERT INTO chain_blocks(chain_id, block_hash, block_number, parent_hash, canonical) VALUES($1,$2,$3,$4,true)`, chainID, hashA, 1, parent); err != nil {
		t.Fatalf("insert block: %v", err)
	}
	expectSQLState(t, execError(pool.Exec(ctx, `INSERT INTO chain_blocks(chain_id, block_hash, block_number, parent_hash, canonical) VALUES($1,$2,$3,$4,true)`, chainID, hashB, 1, parent)), "23505")
	if _, err := pool.Exec(ctx, `INSERT INTO chain_blocks(chain_id, block_hash, block_number, parent_hash, canonical) VALUES($1,$2,$3,$4,false)`, chainID, hashB, 1, parent); err != nil {
		t.Fatalf("insert noncanonical fork block: %v", err)
	}

	logInsert := `INSERT INTO chain_logs(chain_id, contract_address, block_hash, tx_hash, log_index, escrow_id, event_kind, payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,'{}'::jsonb)`
	logArgs := []any{chainID, randomBytes(t, 20), hashA, randomBytes(t, 32), 0, escrowID, "EscrowFunded"}
	if _, err := pool.Exec(ctx, logInsert, logArgs...); err != nil {
		t.Fatalf("insert log: %v", err)
	}
	expectSQLState(t, execError(pool.Exec(ctx, logInsert, logArgs...)), "23505")

	idemInsert := `INSERT INTO idempotency_records(principal, method, route, key, request_hash, response_status, response_body)
		VALUES('merchant','POST','/v1/payment-intents',$1,$2,201,$3)`
	idemKey := fmt.Sprintf("request-key-%x", randomBytes(t, 8))
	if _, err := pool.Exec(ctx, idemInsert, idemKey, repeated(0x31, 32), []byte(`{"id":"x"}`)); err != nil {
		t.Fatalf("insert idempotency: %v", err)
	}
	expectSQLState(t, execError(pool.Exec(ctx, idemInsert, idemKey, repeated(0x32, 32), []byte(`{"id":"y"}`))), "23505")
}

func repeated(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	out := make([]byte, n)
	if _, err := rand.Read(out); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return out
}

func randomUUID(t *testing.T) string {
	t.Helper()
	raw := randomBytes(t, 16)
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}

func execError(_ pgconn.CommandTag, err error) error { return err }

func expectSQLState(t *testing.T, err error, state string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != state {
		t.Fatalf("expected SQLSTATE %s, got %v", state, err)
	}
}
