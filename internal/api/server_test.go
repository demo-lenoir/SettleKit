package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"

	"settlekit/internal/payments"
	"settlekit/internal/store"
	"settlekit/internal/telemetry"
)

const merchantToken = "merchant-local-test-token-0000000000000000"
const operatorToken = "operator-local-test-token-0000000000000000"

func apiFixture(t *testing.T) (*pgxpool.Pool, *Server) {
	t.Helper()
	dsn := os.Getenv("SETTLEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SETTLEKIT_TEST_DATABASE_URL is required for API integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.ApplyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}
	server, err := New(pool, Config{
		ChainID:       31337,
		EscrowAddress: common.HexToAddress("0x1000000000000000000000000000000000000001"),
		TokenAddress:  common.HexToAddress("0x2000000000000000000000000000000000000002"),
		MerchantToken: merchantToken,
		OperatorToken: operatorToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pool, server
}

func newKey(t *testing.T) string {
	t.Helper()
	_, key, err := payments.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func createBody() string {
	expires := time.Now().UTC().Truncate(time.Second).Add(time.Hour).Format(time.RFC3339)
	return fmt.Sprintf(`{"payer":"0x3000000000000000000000000000000000000003","payee":"0x4000000000000000000000000000000000000004","amount_base_units":"1000000","expires_at":%q}`, expires)
}

func call(handler http.Handler, method, path, token, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decodeIntent(t *testing.T, raw []byte) intentResponse {
	t.Helper()
	var in intentResponse
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("decode intent response: %v: %s", err, raw)
	}
	return in
}

func TestCreateIntentIdempotencyAndInputValidation(t *testing.T) {
	pool, server := apiFixture(t)
	handler := server.Handler()
	key := newKey(t)
	body := createBody()
	unauthorized := call(handler, "POST", "/v1/payment-intents", "", key, body)
	if unauthorized.Code != 401 {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}
	rawTokenRequest := httptest.NewRequest("POST", "/v1/payment-intents", strings.NewReader(body))
	rawTokenRequest.Header.Set("Authorization", merchantToken)
	rawTokenResponse := httptest.NewRecorder()
	handler.ServeHTTP(rawTokenResponse, rawTokenRequest)
	if rawTokenResponse.Code != 401 {
		t.Fatalf("bearer scheme bypass status=%d", rawTokenResponse.Code)
	}
	first := call(handler, "POST", "/v1/payment-intents", merchantToken, key, body)
	if first.Code != 201 {
		t.Fatalf("create status=%d body=%s", first.Code, first.Body)
	}
	created := decodeIntent(t, first.Body.Bytes())
	if created.Status != "AWAITING_CHAIN" || created.ID == "" || created.EscrowID == "" || created.PayerCall.Data == "" {
		t.Fatalf("incomplete create response: %+v", created)
	}
	if bytes.Contains(first.Body.Bytes(), []byte(merchantToken)) || bytes.Contains(first.Body.Bytes(), []byte(operatorToken)) {
		t.Fatal("credential leaked in response")
	}
	replay := call(handler, "POST", "/v1/payment-intents", merchantToken, key, body)
	if replay.Code != 201 || !bytes.Equal(replay.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("replay changed result: status=%d body=%s", replay.Code, replay.Body)
	}
	server.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	expiredReplay := call(handler, "POST", "/v1/payment-intents", merchantToken, key, body)
	if expiredReplay.Code != 201 || !bytes.Equal(expiredReplay.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("replay after time window changed result: %d %s", expiredReplay.Code, expiredReplay.Body)
	}
	conflict := call(handler, "POST", "/v1/payment-intents", merchantToken, key, strings.Replace(body, "1000000", "2000000", 1))
	if conflict.Code != 409 || !strings.Contains(conflict.Body.String(), "IDEMPOTENCY_CONFLICT") {
		t.Fatalf("key conflict status=%d body=%s", conflict.Code, conflict.Body)
	}
	var count int
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_intents WHERE id=$1`, created.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("intent count=%d err=%v", count, err)
	}
	got := call(handler, "GET", "/v1/payment-intents/"+created.ID, merchantToken, "", "")
	if got.Code != 200 || decodeIntent(t, got.Body.Bytes()).EscrowID != created.EscrowID {
		t.Fatalf("get status=%d body=%s", got.Code, got.Body)
	}
	rotated, err := New(pool, Config{ChainID: 31337,
		EscrowAddress: common.HexToAddress("0x5000000000000000000000000000000000000005"),
		TokenAddress:  common.HexToAddress("0x2000000000000000000000000000000000000002"),
		MerchantToken: merchantToken, OperatorToken: operatorToken})
	if err != nil {
		t.Fatal(err)
	}
	rotatedGet := call(rotated.Handler(), "GET", "/v1/payment-intents/"+created.ID, merchantToken, "", "")
	if rotatedGet.Code != 200 || decodeIntent(t, rotatedGet.Body.Bytes()).PayerCall.To != created.PayerCall.To {
		t.Fatalf("configuration rotation redirected stored intent: %d %s", rotatedGet.Code, rotatedGet.Body)
	}
	restartedReplay := call(rotated.Handler(), "POST", "/v1/payment-intents", merchantToken, key, body)
	if restartedReplay.Code != 201 || !bytes.Equal(restartedReplay.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("new API instance changed durable replay: %d %s", restartedReplay.Code, restartedReplay.Body)
	}
	if got.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing request ID")
	}

	invalid := call(handler, "POST", "/v1/payment-intents", merchantToken, newKey(t), strings.Replace(body, "1000000", "1.1", 1))
	if invalid.Code != 400 {
		t.Fatalf("fractional amount accepted: %d", invalid.Code)
	}
	tooLarge := call(handler, "POST", "/v1/payment-intents", merchantToken, newKey(t), body+strings.Repeat(" ", maxBodyBytes))
	if tooLarge.Code != 413 {
		t.Fatalf("large body accepted: %d", tooLarge.Code)
	}
	missingKey := call(handler, "POST", "/v1/payment-intents", merchantToken, "", body)
	if missingKey.Code != 400 {
		t.Fatalf("missing key accepted: %d", missingKey.Code)
	}
}

func TestPostRateLimitDoesNotBlockHealth(t *testing.T) {
	_, server := apiFixture(t)
	server.rate.started = time.Now()
	server.rate.count = 120
	handler := server.Handler()
	if got := call(handler, "POST", "/v1/payment-intents", merchantToken, newKey(t), createBody()); got.Code != http.StatusTooManyRequests {
		t.Fatalf("POST rate limit status=%d", got.Code)
	}
	if got := call(handler, "GET", "/v1/health/live", "", "", ""); got.Code != http.StatusOK {
		t.Fatalf("liveness blocked by POST rate limit: %d", got.Code)
	}
}

func TestWeakBearerTokenConfigurationRejected(t *testing.T) {
	pool, server := apiFixture(t)
	config := server.cfg
	config.OperatorToken = "weak"
	if _, err := New(pool, config); err == nil {
		t.Fatal("short operator bearer token accepted")
	}
	config = server.cfg
	config.MerchantToken = "weak"
	if _, err := New(pool, config); err == nil {
		t.Fatal("short merchant bearer token accepted")
	}
}

func TestOperatorAuthorizationAndOneActionPerIntent(t *testing.T) {
	pool, server := apiFixture(t)
	handler := server.Handler()
	createdResp := call(handler, "POST", "/v1/payment-intents", merchantToken, newKey(t), createBody())
	if createdResp.Code != 201 {
		t.Fatalf("create: %d %s", createdResp.Code, createdResp.Body)
	}
	created := decodeIntent(t, createdResp.Body.Bytes())
	path := "/v1/payment-intents/" + created.ID + "/release"
	key := newKey(t)
	if got := call(handler, "POST", path, merchantToken, key, ""); got.Code != 401 {
		t.Fatalf("merchant authorized as operator: %d", got.Code)
	}
	if got := call(handler, "POST", path, operatorToken, key, ""); got.Code != 409 {
		t.Fatalf("unconfirmed intent action accepted: %d %s", got.Code, got.Body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE payment_intents SET status='CONFIRMED' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	first := call(handler, "POST", path, operatorToken, key, "")
	if first.Code != 202 {
		t.Fatalf("operator release status=%d body=%s", first.Code, first.Body)
	}
	replay := call(handler, "POST", path, operatorToken, key, "")
	if replay.Code != 202 || !bytes.Equal(replay.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("operator replay changed result: %d %s", replay.Code, replay.Body)
	}
	refund := call(handler, "POST", "/v1/payment-intents/"+created.ID+"/refund", operatorToken, newKey(t), "")
	if refund.Code != 409 || !strings.Contains(refund.Body.String(), "OPERATION_CONFLICT") {
		t.Fatalf("conflicting action status=%d body=%s", refund.Code, refund.Body)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operator_actions WHERE intent_id=$1`, created.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("action count=%d err=%v", count, err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM payment_intents WHERE id=$1`, created.ID).Scan(&status); err != nil || status != "CONFIRMED" {
		t.Fatalf("HTTP action changed money state: %s %v", status, err)
	}
}

func TestConcurrentIdenticalRequestsCreateOneIntent(t *testing.T) {
	pool, server := apiFixture(t)
	handler := server.Handler()
	key := newKey(t)
	body := createBody()
	const workers = 12
	results := make([]*httptest.ResponseRecorder, workers)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index] = call(handler, "POST", "/v1/payment-intents", merchantToken, key, body)
		}(i)
	}
	wg.Wait()
	first := results[0]
	for i, result := range results {
		if result.Code != 201 || !bytes.Equal(result.Body.Bytes(), first.Body.Bytes()) {
			t.Fatalf("worker %d got %d %s, first=%d %s", i, result.Code, result.Body, first.Code, first.Body)
		}
	}
	created := decodeIntent(t, first.Body.Bytes())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_intents WHERE id=$1`, created.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent create count=%d err=%v", count, err)
	}
}

func TestConcurrentReleaseAndRefundAllowOnlyOneAction(t *testing.T) {
	pool, server := apiFixture(t)
	handler := server.Handler()
	createdResp := call(handler, "POST", "/v1/payment-intents", merchantToken, newKey(t), createBody())
	if createdResp.Code != 201 {
		t.Fatalf("create: %d %s", createdResp.Code, createdResp.Body)
	}
	created := decodeIntent(t, createdResp.Body.Bytes())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE payment_intents SET status='CONFIRMED' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		"/v1/payment-intents/" + created.ID + "/release",
		"/v1/payment-intents/" + created.ID + "/refund",
	}
	keys := []string{newKey(t), newKey(t)}
	results := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range paths {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index] = call(handler, "POST", paths[index], operatorToken, keys[index], "")
		}(i)
	}
	wg.Wait()
	accepted, conflicted := 0, 0
	for _, result := range results {
		switch result.Code {
		case 202:
			accepted++
		case 409:
			conflicted++
		default:
			t.Fatalf("unexpected concurrent action status=%d body=%s", result.Code, result.Body)
		}
	}
	if accepted != 1 || conflicted != 1 {
		t.Fatalf("accepted=%d conflicted=%d", accepted, conflicted)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operator_actions WHERE intent_id=$1`, created.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("action count=%d err=%v", count, err)
	}
}

func TestReadinessMetricsAndRequestLogsRedactCredentials(t *testing.T) {
	pool, _ := apiFixture(t)
	var logs bytes.Buffer
	metrics := &telemetry.Metrics{}
	ready := false
	server, err := New(pool, Config{ChainID: 31337, EscrowAddress: common.HexToAddress("0x1000000000000000000000000000000000000001"), TokenAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"), MerchantToken: merchantToken, OperatorToken: operatorToken, Metrics: metrics, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Ready: func(context.Context) error {
		if !ready {
			return fmt.Errorf("not ready")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	if got := call(handler, "GET", "/v1/health/ready", "", "", ""); got.Code != 503 {
		t.Fatalf("unhealthy readiness=%d", got.Code)
	}
	ready = true
	if got := call(handler, "GET", "/v1/health/ready", "", "", ""); got.Code != 200 {
		t.Fatalf("healthy readiness=%d", got.Code)
	}
	invalid := call(handler, "GET", "/v1/payment-intents/private-path-value", merchantToken, "", "")
	if invalid.Code != 400 || invalid.Header().Get("traceparent") == "" {
		t.Fatalf("trace/status %d %s", invalid.Code, invalid.Header().Get("traceparent"))
	}
	metricResponse := call(handler, "GET", "/metrics", "", "", "")
	if metricResponse.Code != 200 || !strings.Contains(metricResponse.Body.String(), "settlekit_ready 1") {
		t.Fatalf("metrics=%d %s", metricResponse.Code, metricResponse.Body)
	}
	if strings.Contains(logs.String(), merchantToken) || strings.Contains(logs.String(), operatorToken) || strings.Contains(logs.String(), "private-path-value") {
		t.Fatalf("sensitive value in logs: %s", logs.String())
	}
}

func TestMutatingRateLimitDoesNotStarveHealth(t *testing.T) {
	_, server := apiFixture(t)
	handler := server.Handler()
	for i := 0; i < 120; i++ {
		call(handler, "POST", "/v1/payment-intents", "", "", "")
	}
	if got := call(handler, "POST", "/v1/payment-intents", "", "", ""); got.Code != 429 {
		t.Fatalf("rate limit status=%d", got.Code)
	}
	if got := call(handler, "GET", "/v1/health/live", "", "", ""); got.Code != 200 {
		t.Fatalf("liveness starved status=%d", got.Code)
	}
}
