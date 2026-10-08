package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestConfirmationPolicy(t *testing.T) {
	for _, tc := range []struct {
		chain uint64
		raw   string
		want  uint64
		valid bool
	}{
		{31337, "", 2, true}, {31337, "6", 6, true}, {11155111, "6", 6, true}, {1, "128", 128, true},
		{11155111, "", 0, false}, {1, "", 0, false}, {1337, "", 0, false}, {31337, "0", 0, false},
		{11155111, "1", 0, false}, {11155111, "129", 0, false}, {11155111, "-1", 0, false},
		{11155111, " 6", 0, false}, {11155111, "six", 0, false}, {11155111, "18446744073709551616", 0, false},
	} {
		got, err := confirmationPolicy(tc.chain, tc.raw)
		if (err == nil) != tc.valid || got != tc.want {
			t.Errorf("chain=%d value=%q got=%d err=%v", tc.chain, tc.raw, got, err)
		}
	}
}

func TestStartupRejectsMissingNonlocalConfirmationsBeforeIO(t *testing.T) {
	t.Setenv("SETTLEKIT_CHAIN_ID", "11155111")
	t.Setenv("SETTLEKIT_CONFIRMATIONS", "")
	err := run(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "SETTLEKIT_CONFIRMATIONS") {
		t.Fatalf("wrong startup failure: %v", err)
	}
}
