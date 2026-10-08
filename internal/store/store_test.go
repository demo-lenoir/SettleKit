package store

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestOpenHonorsCanceledContext(t *testing.T) {
	dsn := os.Getenv("SETTLEKIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SETTLEKIT_TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pool, err := Open(ctx, dsn)
	if pool != nil {
		pool.Close()
		t.Fatal("Open returned a pool after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error=%v, want context.Canceled", err)
	}
}
