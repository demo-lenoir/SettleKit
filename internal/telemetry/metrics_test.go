package telemetry

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsUseBoundedLabelsAndTraceFormat(t *testing.T) {
	m := &Metrics{}
	m.HTTP("/v1/payment-intents/{id}", "GET", 200)
	m.HTTP("other", "DELETE", 404)
	m.Poll(true)
	m.Poll(false)
	m.Webhook("retry")
	m.Reorg()
	m.Progress(10, 2)
	m.Ready(true)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{`settlekit_chain_checkpoint 10`, `settlekit_chain_lag_blocks 2`, `settlekit_reorg_total 1`, `route="/v1/payment-intents/{id}"`, `method="OTHER"`, `settlekit_ready 1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing metric %q in %s", want, body)
		}
	}
	trace, span := NewTrace()
	if len(trace) != 32 || len(span) != 16 || len(Traceparent(trace, span)) != 55 {
		t.Fatalf("invalid traceparent %s", Traceparent(trace, span))
	}
}
