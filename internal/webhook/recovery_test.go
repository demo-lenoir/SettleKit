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

func recoveryDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("SETTLEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SETTLEKIT_TEST_DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, id, err := payments.NewID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "recovery_" + strings.ReplaceAll(id, "-", "")
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		admin.Close()
	})
	if err = store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(id))
	_, err = store.CreateIntent(ctx, pool, store.Idempotency{Principal: "test", Method: "POST", Route: "/v1/payment-intents", Key: id, Hash: h}, store.Intent{ID: id, MerchantPrincipal: "test", EscrowID: h[:], ChainID: 11155111, EscrowContract: bytes.Repeat([]byte{1}, 20), Payer: bytes.Repeat([]byte{2}, 20), Payee: bytes.Repeat([]byte{3}, 20), Token: bytes.Repeat([]byte{4}, 20), Amount: "1000000", Status: "AWAITING_CHAIN", ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second)}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err = pool.QueryRow(ctx, `SELECT id::text FROM outbox_events WHERE intent_id=$1`, id).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	return pool, eventID
}

func TestOutboxCrashBoundaries(t *testing.T) {
	for _, attempt := range []int{1, 10} {
		for _, boundary := range []string{"claimed-before-send", "sent-before-ack", "ack-canceled"} {
			t.Run(strconv.Itoa(attempt)+"/"+boundary, func(t *testing.T) {
				pool, eventID := recoveryDB(t)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				secret := bytes.Repeat([]byte{7}, 32)
				var received []string
				var bodies [][]byte
				var mu sync.Mutex
				sendCtx, cancelSend := context.WithCancel(ctx)
				defer cancelSend()
				receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
					stamp, _ := strconv.ParseInt(r.Header.Get("X-SettleKit-Timestamp"), 10, 64)
					if !Verify(secret, stamp, r.Header.Get("X-SettleKit-Event-Id"), b, r.Header.Get("X-SettleKit-Signature"), time.Now()) {
						t.Error("invalid signature")
					}
					mu.Lock()
					received = append(received, r.Header.Get("X-SettleKit-Event-Id"))
					bodies = append(bodies, b)
					mu.Unlock()
					if boundary == "ack-canceled" {
						cancelSend()
					}
					w.WriteHeader(204)
				}))
				defer receiver.Close()
				worker, err := New(pool, receiver.URL, secret)
				if err != nil {
					t.Fatal(err)
				}
				if boundary == "ack-canceled" {
					if _, err = pool.Exec(ctx, `UPDATE outbox_events SET attempts=$2 WHERE id=$1`, eventID, attempt-1); err != nil {
						t.Fatal(err)
					}
					if _, err = worker.RunOnce(sendCtx); err == nil {
						t.Fatal("canceled acknowledgement accepted")
					}
				} else {
					if _, err = pool.Exec(ctx, `UPDATE outbox_events SET attempts=$2,lease_owner='dead-process',lease_until=now()+interval '30 seconds' WHERE id=$1`, eventID, attempt); err != nil {
						t.Fatal(err)
					}
					if boundary == "sent-before-ack" {
						var body []byte
						if err = pool.QueryRow(ctx, `SELECT body FROM outbox_events WHERE id=$1`, eventID).Scan(&body); err != nil {
							t.Fatal(err)
						}
						stamp := time.Now().Unix()
						req, _ := http.NewRequestWithContext(ctx, "POST", receiver.URL, bytes.NewReader(body))
						req.Header.Set("X-SettleKit-Event-Id", eventID)
						req.Header.Set("X-SettleKit-Timestamp", strconv.FormatInt(stamp, 10))
						req.Header.Set("X-SettleKit-Signature", Sign(secret, stamp, eventID, body))
						client := &http.Client{Timeout: time.Second}
						res, err := client.Do(req)
						if err != nil {
							t.Fatal(err)
						}
						res.Body.Close()
					}
				}
				restarted, err := New(pool, receiver.URL, secret)
				if err != nil {
					t.Fatal(err)
				}
				if worked, err := restarted.RunOnce(ctx); err != nil || worked {
					t.Fatalf("active lease stolen: %v %v", worked, err)
				}
				if _, err = pool.Exec(ctx, `UPDATE outbox_events SET lease_until=now()-interval '1 second' WHERE id=$1`, eventID); err != nil {
					t.Fatal(err)
				}
				if worked, err := restarted.RunOnce(ctx); err != nil || !worked {
					t.Fatalf("abandoned event not recovered: %v %v", worked, err)
				}
				var dead, delivered bool
				var attempts int
				if err = pool.QueryRow(ctx, `SELECT attempts,dead_lettered_at IS NOT NULL,delivered_at IS NOT NULL FROM outbox_events WHERE id=$1`, eventID).Scan(&attempts, &dead, &delivered); err != nil {
					t.Fatal(err)
				}
				if attempt == 10 {
					if !dead || delivered || attempts != 10 {
						t.Fatalf("exhausted lease not dead-lettered: %d %v %v", attempts, dead, delivered)
					}
					if err = store.RequeueDeadLetter(ctx, pool, eventID); err != nil {
						t.Fatal(err)
					}
					if worked, err := restarted.RunOnce(ctx); err != nil || !worked {
						t.Fatalf("manual replay: %v %v", worked, err)
					}
				} else if !delivered || dead || attempts != 2 {
					t.Fatalf("retry: %d %v %v", attempts, dead, delivered)
				}
				if err = store.RequeueDeadLetter(ctx, pool, eventID); !errors.Is(err, store.ErrOutboxNotDead) {
					t.Fatalf("delivered event replayed: %v", err)
				}
				mu.Lock()
				defer mu.Unlock()
				for i, id := range received {
					if id != eventID || !bytes.Equal(bodies[i], bodies[0]) {
						t.Fatal("retry changed identity/body")
					}
				}
			})
		}
	}
}

func TestOutboxCanceledBeforeClaim(t *testing.T) {
	pool, id := recoveryDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker, err := New(pool, "http://127.0.0.1:1", bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.RunOnce(ctx); err == nil {
		t.Fatal("cancellation ignored")
	}
	check, cancelCheck := context.WithTimeout(context.Background(), time.Second)
	defer cancelCheck()
	var attempts int
	if err = pool.QueryRow(check, `SELECT attempts FROM outbox_events WHERE id=$1`, id).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("claim mutated: %d %v", attempts, err)
	}
}

func TestConcurrentOutboxWorkersClaimOnce(t *testing.T) {
	pool, _ := recoveryDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, finish := make(chan struct{}), make(chan struct{})
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-finish:
		case <-r.Context().Done():
		}
		w.WriteHeader(204)
	}))
	defer func() { close(finish); receiver.Close() }()
	a, _ := New(pool, receiver.URL, bytes.Repeat([]byte{1}, 32))
	b, _ := New(pool, receiver.URL, bytes.Repeat([]byte{1}, 32))
	done := make(chan error, 1)
	go func() { _, err := a.RunOnce(ctx); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if worked, err := b.RunOnce(ctx); err != nil || worked {
		t.Fatalf("second worker stole live lease: %v %v", worked, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not cancel")
	}
}
