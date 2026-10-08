package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestRPCRespectsCancellationAndRedactsProviderURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer server.Close()
	source, err := NewRPCSource(server.URL+"?credential=secret-provider-token", common.HexToAddress("0x1000000000000000000000000000000000000001"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = source.Head(ctx)
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("RPC did not stop on canceled context: %v", err)
	}
	if strings.Contains(err.Error(), "secret-provider-token") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("RPC error leaked provider URL: %v", err)
	}
}
