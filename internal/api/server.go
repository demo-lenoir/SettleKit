package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"

	"settlekit/internal/chain"
	"settlekit/internal/payments"
	"settlekit/internal/store"
	"settlekit/internal/telemetry"
)

const (
	maxBodyBytes   = 4096
	requestTimeout = 5 * time.Second
)

var amountPattern = regexp.MustCompile(`^[1-9][0-9]*$`)
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{16,128}$`)
var maxAmount = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

type Config struct {
	ChainID       uint64
	EscrowAddress common.Address
	TokenAddress  common.Address
	MerchantToken string
	OperatorToken string
	Ready         func(context.Context) error
	Metrics       *telemetry.Metrics
	Logger        *slog.Logger
}

type Server struct {
	pool *pgxpool.Pool
	cfg  Config
	rate rateWindow
	now  func() time.Time
}

type rateWindow struct {
	mu      sync.Mutex
	started time.Time
	count   int
}

type unsignedCall struct {
	ChainID uint64 `json:"chain_id"`
	To      string `json:"to"`
	Data    string `json:"data"`
	Value   string `json:"value"`
}

type chainEvidence struct {
	BlockHash string `json:"block_hash,omitempty"`
	TxHash    string `json:"tx_hash,omitempty"`
}

type intentResponse struct {
	ID              string        `json:"id"`
	EscrowID        string        `json:"escrow_id"`
	Status          string        `json:"status"`
	AmountBaseUnits string        `json:"amount_base_units"`
	Token           string        `json:"token"`
	Payer           string        `json:"payer"`
	Payee           string        `json:"payee"`
	ExpiresAt       string        `json:"expires_at"`
	PayerCall       unsignedCall  `json:"payer_call"`
	ChainEvidence   chainEvidence `json:"chain_evidence"`
}

type createRequest struct {
	Payer           string `json:"payer"`
	Payee           string `json:"payee"`
	AmountBaseUnits string `json:"amount_base_units"`
	ExpiresAt       string `json:"expires_at"`
}

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func New(pool *pgxpool.Pool, cfg Config) (*Server, error) {
	if pool == nil || cfg.ChainID == 0 || cfg.ChainID > math.MaxInt64 || cfg.EscrowAddress == (common.Address{}) || cfg.TokenAddress == (common.Address{}) || len(cfg.MerchantToken) < 32 || len(cfg.OperatorToken) < 32 || cfg.MerchantToken == cfg.OperatorToken {
		return nil, errors.New("invalid API configuration")
	}
	return &Server{pool: pool, cfg: cfg, now: time.Now}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.cfg.Metrics != nil {
		mux.Handle("GET /metrics", s.cfg.Metrics)
	}
	mux.HandleFunc("GET /v1/health/live", func(w http.ResponseWriter, r *http.Request) {
		requestID(w)
		writeBody(w, 200, []byte(`{"status":"alive"}`))
	})
	mux.HandleFunc("GET /v1/health/ready", func(w http.ResponseWriter, r *http.Request) {
		id := requestID(w)
		if s.cfg.Ready == nil {
			if s.cfg.Metrics != nil {
				s.cfg.Metrics.Ready(false)
			}
			writeError(w, 503, "NOT_READY", "readiness check is not configured", id)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.cfg.Ready(ctx); err != nil {
			if s.cfg.Metrics != nil {
				s.cfg.Metrics.Ready(false)
			}
			writeError(w, 503, "NOT_READY", "dependencies or chain sync are unhealthy", id)
			return
		}
		if s.cfg.Metrics != nil {
			s.cfg.Metrics.Ready(true)
		}
		writeBody(w, 200, []byte(`{"status":"ready"}`))
	})
	mux.HandleFunc("POST /v1/payment-intents", s.createIntent)
	mux.HandleFunc("GET /v1/payment-intents/{id}", s.getIntent)
	mux.HandleFunc("POST /v1/payment-intents/{id}/release", s.release)
	mux.HandleFunc("POST /v1/payment-intents/{id}/refund", s.refund)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "route not found", requestID(w))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		traceID, spanID := telemetry.NewTrace()
		w.Header().Set("traceparent", telemetry.Traceparent(traceID, spanID))
		tracked := &statusWriter{ResponseWriter: w, status: 200}
		defer func() {
			route := metricRoute(r.URL.Path)
			if s.cfg.Metrics != nil {
				s.cfg.Metrics.HTTP(route, r.Method, tracked.status)
			}
			if s.cfg.Logger != nil {
				s.cfg.Logger.Info("http request", "trace_id", traceID, "span_id", spanID, "request_id", tracked.Header().Get("X-Request-Id"), "route", route, "method", r.Method, "status", tracked.status, "duration_ms", time.Since(started).Milliseconds())
			}
		}()
		if r.Method == http.MethodPost && !s.rate.allow(time.Now()) {
			writeError(tracked, http.StatusTooManyRequests, "RATE_LIMITED", "request rate exceeded", requestID(tracked))
			return
		}
		mux.ServeHTTP(tracked, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

func metricRoute(path string) string {
	switch path {
	case "/v1/payment-intents", "/v1/health/live", "/v1/health/ready", "/metrics":
		return path
	}
	if strings.HasPrefix(path, "/v1/payment-intents/") {
		parts := strings.Split(strings.TrimPrefix(path, "/v1/payment-intents/"), "/")
		if len(parts) == 1 {
			return "/v1/payment-intents/{id}"
		}
		if len(parts) == 2 && (parts[1] == "release" || parts[1] == "refund") {
			return "/v1/payment-intents/{id}/" + parts[1]
		}
	}
	return "other"
}

func (r *rateWindow) allow(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started.IsZero() || now.Sub(r.started) >= time.Minute {
		r.started = now
		r.count = 0
	}
	if r.count >= 120 {
		return false
	}
	r.count++
	return true
}

func (s *Server) authorized(r *http.Request, operator bool) bool {
	expected := s.cfg.MerchantToken
	if operator {
		expected = s.cfg.OperatorToken
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	actual := strings.TrimPrefix(header, "Bearer ")
	actualHash, expectedHash := sha256.Sum256([]byte(actual)), sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(actualHash[:], expectedHash[:]) == 1
}

func requestID(w http.ResponseWriter) string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "unavailable"
	}
	id := hex.EncodeToString(raw[:])
	w.Header().Set("X-Request-Id", id)
	return id
}

func writeError(w http.ResponseWriter, status int, code, message, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{code, message, id})
}

func writeBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func key(r *http.Request) (string, bool) {
	value := r.Header.Get("Idempotency-Key")
	return value, keyPattern.MatchString(value)
}

func readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return nil, errors.New("Content-Type must be application/json")
	}
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
}

func parseCreate(raw []byte, now time.Time, contract common.Address) (createRequest, common.Address, common.Address, *big.Int, time.Time, error) {
	var req createRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, common.Address{}, common.Address{}, nil, time.Time{}, fmt.Errorf("decode request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return req, common.Address{}, common.Address{}, nil, time.Time{}, errors.New("request must contain exactly one JSON object")
	}
	if !common.IsHexAddress(req.Payer) || !common.IsHexAddress(req.Payee) {
		return req, common.Address{}, common.Address{}, nil, time.Time{}, errors.New("payer and payee must be EVM addresses")
	}
	payer, payee := common.HexToAddress(req.Payer), common.HexToAddress(req.Payee)
	if payer == (common.Address{}) || payee == (common.Address{}) || payer == payee || payer == contract || payee == contract {
		return req, payer, payee, nil, time.Time{}, errors.New("invalid payer or payee")
	}
	if !amountPattern.MatchString(req.AmountBaseUnits) {
		return req, payer, payee, nil, time.Time{}, errors.New("amount_base_units must be a positive decimal integer")
	}
	amount, ok := new(big.Int).SetString(req.AmountBaseUnits, 10)
	if !ok || amount.Cmp(maxAmount) > 0 {
		return req, payer, payee, nil, time.Time{}, errors.New("amount_base_units exceeds uint128")
	}
	expiry, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil || expiry.UTC().Format(time.RFC3339) != req.ExpiresAt || !strings.HasSuffix(req.ExpiresAt, "Z") || strings.Contains(req.ExpiresAt, ".") {
		return req, payer, payee, nil, time.Time{}, errors.New("expires_at must be UTC at whole-second precision")
	}
	if expiry.Before(now.Add(5*time.Minute)) || expiry.After(now.Add(30*24*time.Hour)) {
		return req, payer, payee, nil, time.Time{}, errors.New("expires_at must be 5 minutes to 30 days in the future")
	}
	return req, payer, payee, amount, expiry, nil
}

func (s *Server) createIntent(w http.ResponseWriter, r *http.Request) {
	id := requestID(w)
	if !s.authorized(r, false) {
		writeError(w, 401, "UNAUTHORIZED", "merchant token required", id)
		return
	}
	idemKey, ok := key(r)
	if !ok {
		writeError(w, 400, "INVALID_IDEMPOTENCY_KEY", "valid Idempotency-Key required", id)
		return
	}
	raw, err := readJSONBody(w, r)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, 413, "BODY_TOO_LARGE", "request body exceeds limit", id)
		} else {
			writeError(w, 400, "INVALID_REQUEST", err.Error(), id)
		}
		return
	}
	hash := sha256.Sum256(raw)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	idem := store.Idempotency{Principal: "merchant", Method: "POST", Route: "/v1/payment-intents", Key: idemKey, Hash: hash}
	if prior, lookupErr := store.FindIdempotency(ctx, s.pool, idem); lookupErr == nil {
		writeBody(w, prior.Status, prior.Body)
		return
	} else if !errors.Is(lookupErr, store.ErrIdempotencyNotFound) {
		writeStoreError(w, lookupErr, id)
		return
	}
	req, payer, payee, amount, expiry, err := parseCreate(raw, s.now().UTC(), s.cfg.EscrowAddress)
	if err != nil {
		writeError(w, 400, "INVALID_REQUEST", err.Error(), id)
		return
	}
	intentID, intentText, err := payments.NewID()
	if err != nil {
		writeError(w, 500, "INTERNAL", "could not create intent ID", id)
		return
	}
	escrowID, err := chain.EscrowID(s.cfg.ChainID, s.cfg.EscrowAddress, intentID, payer, payee, amount, uint64(expiry.Unix()))
	if err != nil {
		writeError(w, 500, "INTERNAL", "could not derive escrow ID", id)
		return
	}
	callData, err := chain.CreateCall(intentID, payee, amount, uint64(expiry.Unix()))
	if err != nil {
		writeError(w, 500, "INTERNAL", "could not encode payer call", id)
		return
	}
	response := intentResponse{
		ID: intentText, EscrowID: escrowID.Hex(), Status: string(payments.AwaitingChain),
		AmountBaseUnits: req.AmountBaseUnits, Token: s.cfg.TokenAddress.Hex(),
		Payer: payer.Hex(), Payee: payee.Hex(), ExpiresAt: req.ExpiresAt,
		PayerCall:     unsignedCall{s.cfg.ChainID, s.cfg.EscrowAddress.Hex(), "0x" + hex.EncodeToString(callData), "0"},
		ChainEvidence: chainEvidence{},
	}
	body, err := json.Marshal(response)
	if err != nil {
		writeError(w, 500, "INTERNAL", "could not encode response", id)
		return
	}
	result, err := store.CreateIntent(ctx, s.pool,
		idem,
		store.Intent{ID: intentText, MerchantPrincipal: "merchant", EscrowID: escrowID[:], ChainID: int64(s.cfg.ChainID), EscrowContract: s.cfg.EscrowAddress.Bytes(), Payer: payer.Bytes(), Payee: payee.Bytes(), Token: s.cfg.TokenAddress.Bytes(), Amount: req.AmountBaseUnits, Status: string(payments.AwaitingChain), ExpiresAt: expiry}, body)
	if err != nil {
		writeStoreError(w, err, id)
		return
	}
	writeBody(w, result.Status, result.Body)
}

func (s *Server) getIntent(w http.ResponseWriter, r *http.Request) {
	id := requestID(w)
	if !s.authorized(r, false) {
		writeError(w, 401, "UNAUTHORIZED", "merchant token required", id)
		return
	}
	pathID := r.PathValue("id")
	intentID, err := payments.ParseID(pathID)
	if err != nil {
		writeError(w, 400, "INVALID_ID", "invalid intent ID", id)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	intent, err := store.GetIntent(ctx, s.pool, pathID, "merchant")
	if err != nil {
		writeStoreError(w, err, id)
		return
	}
	amount, ok := new(big.Int).SetString(intent.Amount, 10)
	if !ok {
		writeError(w, 500, "INTERNAL", "stored amount invalid", id)
		return
	}
	callData, err := chain.CreateCall(intentID, common.BytesToAddress(intent.Payee), amount, uint64(intent.ExpiresAt.Unix()))
	if err != nil {
		writeError(w, 500, "INTERNAL", "stored call invalid", id)
		return
	}
	evidence := chainEvidence{}
	if len(intent.ObservedBlockHash) == 32 {
		evidence.BlockHash = common.BytesToHash(intent.ObservedBlockHash).Hex()
	}
	if len(intent.ObservedTxHash) == 32 {
		evidence.TxHash = common.BytesToHash(intent.ObservedTxHash).Hex()
	}
	response := intentResponse{
		ID: intent.ID, EscrowID: common.BytesToHash(intent.EscrowID).Hex(), Status: intent.Status,
		AmountBaseUnits: intent.Amount, Token: common.BytesToAddress(intent.Token).Hex(),
		Payer: common.BytesToAddress(intent.Payer).Hex(), Payee: common.BytesToAddress(intent.Payee).Hex(),
		ExpiresAt:     intent.ExpiresAt.UTC().Format(time.RFC3339),
		PayerCall:     unsignedCall{uint64(intent.ChainID), common.BytesToAddress(intent.EscrowContract).Hex(), "0x" + hex.EncodeToString(callData), "0"},
		ChainEvidence: evidence,
	}
	body, err := json.Marshal(response)
	if err != nil {
		writeError(w, 500, "INTERNAL", "could not encode response", id)
		return
	}
	writeBody(w, 200, body)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) { s.operatorAction(w, r, "release") }
func (s *Server) refund(w http.ResponseWriter, r *http.Request)  { s.operatorAction(w, r, "refund") }

func (s *Server) operatorAction(w http.ResponseWriter, r *http.Request, kind string) {
	id := requestID(w)
	if !s.authorized(r, true) {
		writeError(w, 401, "UNAUTHORIZED", "operator token required", id)
		return
	}
	idemKey, ok := key(r)
	if !ok {
		writeError(w, 400, "INVALID_IDEMPOTENCY_KEY", "valid Idempotency-Key required", id)
		return
	}
	pathID := r.PathValue("id")
	if _, err := payments.ParseID(pathID); err != nil {
		writeError(w, 400, "INVALID_ID", "invalid intent ID", id)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || len(raw) != 0 {
		writeError(w, 400, "INVALID_REQUEST", "operator action body must be empty", id)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	hash := sha256.Sum256(raw)
	route := "/v1/payment-intents/" + pathID + "/" + kind
	idem := store.Idempotency{Principal: "operator", Method: "POST", Route: route, Key: idemKey, Hash: hash}
	if prior, lookupErr := store.FindIdempotency(ctx, s.pool, idem); lookupErr == nil {
		writeBody(w, prior.Status, prior.Body)
		return
	} else if !errors.Is(lookupErr, store.ErrIdempotencyNotFound) {
		writeStoreError(w, lookupErr, id)
		return
	}
	intent, err := store.GetIntent(ctx, s.pool, pathID, "merchant")
	if err != nil {
		writeStoreError(w, err, id)
		return
	}
	callData, err := chain.OperatorCall(kind, common.BytesToHash(intent.EscrowID))
	if err != nil {
		writeError(w, 500, "INTERNAL", "could not encode operator call", id)
		return
	}
	response := unsignedCall{uint64(intent.ChainID), common.BytesToAddress(intent.EscrowContract).Hex(), "0x" + hex.EncodeToString(callData), "0"}
	body, err := json.Marshal(response)
	if err != nil {
		writeError(w, 500, "INTERNAL", "could not encode response", id)
		return
	}
	_, actionID, err := payments.NewID()
	if err != nil {
		writeError(w, 500, "INTERNAL", "could not create action ID", id)
		return
	}
	result, err := store.RequestAction(ctx, s.pool,
		idem,
		store.Action{ID: actionID, IntentID: pathID, Principal: "operator", Kind: strings.ToUpper(kind), CallData: callData}, body)
	if err != nil {
		writeStoreError(w, err, id)
		return
	}
	writeBody(w, result.Status, result.Body)
}

func writeStoreError(w http.ResponseWriter, err error, id string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "NOT_FOUND", "payment intent not found", id)
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeError(w, 409, "IDEMPOTENCY_CONFLICT", "key already used with different request", id)
	case errors.Is(err, store.ErrInvalidPaymentState):
		writeError(w, 409, "INVALID_STATE", "payment is not confirmed", id)
	case errors.Is(err, store.ErrActionConflict):
		writeError(w, 409, "OPERATION_CONFLICT", "operator action already requested", id)
	default:
		writeError(w, 503, "DEPENDENCY_UNAVAILABLE", "operation could not be completed; retry with the same Idempotency-Key", id)
	}
}
