package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type httpKey struct{ route, method, class string }

type Metrics struct {
	mu               sync.Mutex
	http             map[httpKey]uint64
	pollSuccess      atomic.Uint64
	pollFailure      atomic.Uint64
	webhookDelivered atomic.Uint64
	webhookRetry     atomic.Uint64
	webhookDead      atomic.Uint64
	webhookError     atomic.Uint64
	reorg            atomic.Uint64
	checkpoint       atomic.Int64
	lag              atomic.Int64
	ready            atomic.Int64
}

func (m *Metrics) HTTP(route, method string, status int) {
	class := "other"
	if status >= 200 && status < 300 {
		class = "2xx"
	} else if status >= 400 && status < 500 {
		class = "4xx"
	} else if status >= 500 && status < 600 {
		class = "5xx"
	}
	if method != "GET" && method != "POST" {
		method = "OTHER"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.http == nil {
		m.http = make(map[httpKey]uint64)
	}
	m.http[httpKey{route, method, class}]++
}
func (m *Metrics) Poll(ok bool) {
	if ok {
		m.pollSuccess.Add(1)
	} else {
		m.pollFailure.Add(1)
	}
}
func (m *Metrics) Webhook(result string) {
	switch result {
	case "delivered":
		m.webhookDelivered.Add(1)
	case "retry":
		m.webhookRetry.Add(1)
	case "dead":
		m.webhookDead.Add(1)
	default:
		m.webhookError.Add(1)
	}
}
func (m *Metrics) Reorg() { m.reorg.Add(1) }
func (m *Metrics) Progress(checkpoint, lag uint64) {
	m.checkpoint.Store(int64(checkpoint))
	m.lag.Store(int64(lag))
}
func (m *Metrics) Ready(ok bool) {
	if ok {
		m.ready.Store(1)
	} else {
		m.ready.Store(0)
	}
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE settlekit_rpc_polls_total counter\nsettlekit_rpc_polls_total{result=\"success\"} %d\nsettlekit_rpc_polls_total{result=\"failure\"} %d\n", m.pollSuccess.Load(), m.pollFailure.Load())
	fmt.Fprintf(w, "# TYPE settlekit_chain_checkpoint gauge\nsettlekit_chain_checkpoint %d\n# TYPE settlekit_chain_lag_blocks gauge\nsettlekit_chain_lag_blocks %d\n", m.checkpoint.Load(), m.lag.Load())
	fmt.Fprintf(w, "# TYPE settlekit_reorg_total counter\nsettlekit_reorg_total %d\n", m.reorg.Load())
	fmt.Fprintf(w, "# TYPE settlekit_webhook_attempts_total counter\nsettlekit_webhook_attempts_total{result=\"delivered\"} %d\nsettlekit_webhook_attempts_total{result=\"retry\"} %d\nsettlekit_webhook_attempts_total{result=\"dead\"} %d\nsettlekit_webhook_attempts_total{result=\"error\"} %d\n", m.webhookDelivered.Load(), m.webhookRetry.Load(), m.webhookDead.Load(), m.webhookError.Load())
	fmt.Fprintf(w, "# TYPE settlekit_ready gauge\nsettlekit_ready %d\n", m.ready.Load())
	m.mu.Lock()
	keys := make([]httpKey, 0, len(m.http))
	for key := range m.http {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].route+keys[i].method+keys[i].class < keys[j].route+keys[j].method+keys[j].class
	})
	values := make([]uint64, len(keys))
	for i, key := range keys {
		values[i] = m.http[key]
	}
	m.mu.Unlock()
	fmt.Fprint(w, "# TYPE settlekit_http_requests_total counter\n")
	for i, key := range keys {
		fmt.Fprintf(w, "settlekit_http_requests_total{route=%q,method=%q,class=%q} %d\n", key.route, key.method, key.class, values[i])
	}
}

func NewTrace() (traceID, spanID string) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strings.Repeat("0", 32), strings.Repeat("0", 16)
	}
	return hex.EncodeToString(raw[:16]), hex.EncodeToString(raw[16:])
}

func Traceparent(traceID, spanID string) string { return "00-" + traceID + "-" + spanID + "-01" }
