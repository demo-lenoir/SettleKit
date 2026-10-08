package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"settlekit/internal/payments"
	"settlekit/internal/telemetry"
)

const deliveryTimeout = 5 * time.Second

type Dispatcher struct {
	pool     *pgxpool.Pool
	client   *http.Client
	url      string
	secret   []byte
	owner    string
	observer func(result, eventID, traceID string)
}

func (d *Dispatcher) SetObserver(observer func(result, eventID, traceID string)) {
	d.observer = observer
}

func New(pool *pgxpool.Pool, rawURL string, secret []byte) (*Dispatcher, error) {
	parsed, err := url.Parse(rawURL)
	if pool == nil || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || len(secret) < 32 {
		return nil, errors.New("invalid webhook dispatcher configuration")
	}
	if parsed.Scheme == "http" && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		return nil, errors.New("nonlocal webhook URL must use HTTPS")
	}
	_, owner, err := payments.NewID()
	if err != nil {
		return nil, fmt.Errorf("new webhook worker ID: %w", err)
	}
	client := &http.Client{Timeout: deliveryTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Dispatcher{pool: pool, client: client, url: rawURL, secret: append([]byte(nil), secret...), owner: owner}, nil
}

func Sign(secret []byte, timestamp int64, eventID string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, strconv.FormatInt(timestamp, 10))
	_, _ = io.WriteString(mac, ".")
	_, _ = io.WriteString(mac, eventID)
	_, _ = io.WriteString(mac, ".")
	_, _ = mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func Verify(secret []byte, timestamp int64, eventID string, body []byte, signature string, now time.Time) bool {
	if timestamp < now.Add(-5*time.Minute).Unix() || timestamp > now.Add(5*time.Minute).Unix() {
		return false
	}
	expected := Sign(secret, timestamp, eventID, body)
	return hmac.Equal([]byte(expected), []byte(signature))
}

type claimed struct {
	id        string
	body      []byte
	attempts  int
	exhausted bool
}

func (d *Dispatcher) RunOnce(ctx context.Context) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var event claimed
	err := d.pool.QueryRow(bounded, `UPDATE outbox_events SET
		lease_owner=CASE WHEN attempts=10 THEN NULL ELSE $1 END,
		lease_until=CASE WHEN attempts=10 THEN NULL ELSE now()+interval '30 seconds' END,
		dead_lettered_at=CASE WHEN attempts=10 THEN now() ELSE NULL END,
		attempts=LEAST(attempts+1,10)
		WHERE id=(SELECT id FROM outbox_events WHERE delivered_at IS NULL AND dead_lettered_at IS NULL
		AND next_attempt_at<=now() AND (lease_until IS NULL OR lease_until<now())
		ORDER BY next_attempt_at,id LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id::text,body,attempts,dead_lettered_at IS NOT NULL`, d.owner).Scan(&event.id, &event.body, &event.attempts, &event.exhausted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim webhook event: %w", err)
	}
	if event.exhausted {
		if d.observer != nil {
			traceID, _ := telemetry.NewTrace()
			d.observer("dead", event.id, traceID)
		}
		return true, nil
	}
	stamp := time.Now().Unix()
	traceID, spanID := telemetry.NewTrace()
	req, err := http.NewRequestWithContext(bounded, http.MethodPost, d.url, bytes.NewReader(event.body))
	if err != nil {
		return true, fmt.Errorf("construct webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SettleKit-Event-Id", event.id)
	req.Header.Set("X-SettleKit-Timestamp", strconv.FormatInt(stamp, 10))
	req.Header.Set("X-SettleKit-Signature", Sign(d.secret, stamp, event.id, event.body))
	req.Header.Set("traceparent", telemetry.Traceparent(traceID, spanID))
	response, deliveryErr := d.client.Do(req)
	status := 0
	if deliveryErr == nil {
		status = response.StatusCode
		_, _ = io.CopyN(io.Discard, response.Body, 1024)
		_ = response.Body.Close()
	}
	ackCtx, ackCancel := context.WithTimeout(ctx, deliveryTimeout)
	defer ackCancel()
	var command string
	var args []any
	result := "retry"
	switch {
	case deliveryErr == nil && status >= 200 && status < 300:
		result = "delivered"
		command = `UPDATE outbox_events SET delivered_at=now(),lease_owner=NULL,lease_until=NULL WHERE id=$1 AND lease_owner=$2`
		args = []any{event.id, d.owner}
	case (deliveryErr == nil && status >= 400 && status < 500 && status != 408 && status != 429) || event.attempts >= 10:
		result = "dead"
		command = `UPDATE outbox_events SET dead_lettered_at=now(),lease_owner=NULL,lease_until=NULL WHERE id=$1 AND lease_owner=$2`
		args = []any{event.id, d.owner}
	default:
		backoff := time.Duration(1<<min(event.attempts, 10)) * time.Second
		var jitter [1]byte
		if _, err := rand.Read(jitter[:]); err != nil {
			return true, fmt.Errorf("webhook retry jitter: %w", err)
		}
		next := time.Now().Add(backoff + time.Duration(jitter[0])*time.Millisecond)
		command = `UPDATE outbox_events SET next_attempt_at=$3,lease_owner=NULL,lease_until=NULL WHERE id=$1 AND lease_owner=$2`
		args = []any{event.id, d.owner, next}
	}
	if _, err := d.pool.Exec(ackCtx, command, args...); err != nil {
		return true, fmt.Errorf("finish webhook delivery; retry outcome unknown: %w", err)
	}
	if d.observer != nil {
		d.observer(result, event.id, traceID)
	}
	return true, nil
}
