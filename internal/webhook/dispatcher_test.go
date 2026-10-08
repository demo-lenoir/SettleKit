package webhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"settlekit/internal/payments"
	"settlekit/internal/store"
)

func TestSignatureBindsTimestampEventAndRawBody(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	stamp := int64(1700000000)
	body := []byte(`{"status":"CONFIRMED"}`)
	signature := Sign(secret, stamp, "event-one", body)
	if !Verify(secret, stamp, "event-one", body, signature, time.Unix(stamp, 0)) {
		t.Fatal("valid signature rejected")
	}
	if Verify(secret, stamp, "event-two", body, signature, time.Unix(stamp, 0)) {
		t.Fatal("event ID can be changed")
	}
	if Verify(secret, stamp, "event-one", append([]byte(nil), body...), signature, time.Unix(stamp+301, 0)) {
		t.Fatal("stale signature accepted")
	}
	if Verify(secret, stamp, "event-one", []byte(`{"status":"REFUNDED"}`), signature, time.Unix(stamp, 0)) {
		t.Fatal("body can be changed")
	}
}

func TestOutboxRetryAfterWorkerRestart(t *testing.T) {
	dsn := os.Getenv("SETTLEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SETTLEKIT_TEST_DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	adminPool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	_, schemaID, err := payments.NewID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "sk_webhook_" + strings.ReplaceAll(schemaID, "-", "")
	if _, err := adminPool.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(cleanupCtx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	})
	if err := store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	_, intentID, err := payments.NewID()
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := payments.NewID()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(`{"request":1}`))
	escrowHash := sha256.Sum256([]byte(intentID))
	_, err = store.CreateIntent(ctx, pool, store.Idempotency{Principal: "merchant", Method: "POST", Route: "/v1/payment-intents", Key: key, Hash: hash},
		store.Intent{ID: intentID, MerchantPrincipal: "merchant", EscrowID: escrowHash[:], ChainID: time.Now().UnixNano() & 0x7fffffffffffffff,
			EscrowContract: bytes.Repeat([]byte{1}, 20), Payer: bytes.Repeat([]byte{2}, 20), Payee: bytes.Repeat([]byte{3}, 20), Token: bytes.Repeat([]byte{4}, 20), Amount: "1000000", Status: "AWAITING_CHAIN", ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second)}, []byte(`{"id":"created"}`))
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("0123456789abcdef0123456789abcdef")
	var mu sync.Mutex
	var eventIDs []string
	var bodies [][]byte
	invalidSignature := false
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		mu.Lock()
		defer mu.Unlock()
		id := r.Header.Get("X-SettleKit-Event-Id")
		unix, err := strconv.ParseInt(r.Header.Get("X-SettleKit-Timestamp"), 10, 64)
		if err != nil || !Verify(secret, unix, id, body, r.Header.Get("X-SettleKit-Signature"), time.Now()) {
			invalidSignature = true
		}
		eventIDs = append(eventIDs, id)
		bodies = append(bodies, append([]byte(nil), body...))
		if len(eventIDs) == 1 {
			w.WriteHeader(503)
		} else if len(eventIDs) == 3 {
			w.WriteHeader(400)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer receiver.Close()
	first, err := New(pool, receiver.URL, secret)
	if err != nil {
		t.Fatal(err)
	}
	if sent, err := first.RunOnce(ctx); err != nil || !sent {
		t.Fatalf("first delivery sent=%t err=%v", sent, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at=now()-interval '1 second' WHERE intent_id=$1`, intentID); err != nil {
		t.Fatal(err)
	}
	second, err := New(pool, receiver.URL, secret)
	if err != nil {
		t.Fatal(err)
	}
	if sent, err := second.RunOnce(ctx); err != nil || !sent {
		t.Fatalf("restart delivery sent=%t err=%v", sent, err)
	}
	mu.Lock()
	if invalidSignature || len(eventIDs) != 2 || eventIDs[0] != eventIDs[1] || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("unstable/rejected delivery IDs=%v invalid=%t", eventIDs, invalidSignature)
	}
	mu.Unlock()
	var attempts int
	var delivered bool
	if err := pool.QueryRow(ctx, `SELECT attempts,delivered_at IS NOT NULL FROM outbox_events WHERE intent_id=$1`, intentID).Scan(&attempts, &delivered); err != nil || attempts != 2 || !delivered {
		t.Fatalf("attempts=%d delivered=%t err=%v", attempts, delivered, err)
	}
	_, secondIntent, err := payments.NewID()
	if err != nil {
		t.Fatal(err)
	}
	_, secondKey, err := payments.NewID()
	if err != nil {
		t.Fatal(err)
	}
	secondEscrow := sha256.Sum256([]byte(secondIntent))
	_, err = store.CreateIntent(ctx, pool, store.Idempotency{Principal: "merchant", Method: "POST", Route: "/v1/payment-intents", Key: secondKey, Hash: hash},
		store.Intent{ID: secondIntent, MerchantPrincipal: "merchant", EscrowID: secondEscrow[:], ChainID: time.Now().UnixNano() & 0x7fffffffffffffff,
			EscrowContract: bytes.Repeat([]byte{1}, 20), Payer: bytes.Repeat([]byte{2}, 20), Payee: bytes.Repeat([]byte{3}, 20), Token: bytes.Repeat([]byte{4}, 20), Amount: "1000000", Status: "AWAITING_CHAIN", ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second)}, []byte(`{"id":"created"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sent, err := second.RunOnce(ctx); err != nil || !sent {
		t.Fatalf("dead-letter attempt sent=%t err=%v", sent, err)
	}
	var deadEvent string
	var dead bool
	if err := pool.QueryRow(ctx, `SELECT id::text,dead_lettered_at IS NOT NULL FROM outbox_events WHERE intent_id=$1`, secondIntent).Scan(&deadEvent, &dead); err != nil || !dead {
		t.Fatalf("dead-letter state=%t err=%v", dead, err)
	}
	if err := store.RequeueDeadLetter(ctx, pool, deadEvent); err != nil {
		t.Fatal(err)
	}
	if sent, err := second.RunOnce(ctx); err != nil || !sent {
		t.Fatalf("manual replay sent=%t err=%v", sent, err)
	}
	if err := store.RequeueDeadLetter(ctx, pool, deadEvent); !errors.Is(err, store.ErrOutboxNotDead) {
		t.Fatalf("delivered event requeued: %v", err)
	}
	mu.Lock()
	if len(eventIDs) != 4 || eventIDs[2] != eventIDs[3] || !bytes.Equal(bodies[2], bodies[3]) {
		t.Fatalf("manual replay changed event identity: %v", eventIDs)
	}
	mu.Unlock()
}
