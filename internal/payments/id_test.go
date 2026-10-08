package payments

import (
	"errors"
	"testing"
)

func TestIntentIDRoundTrip(t *testing.T) {
	t.Parallel()
	a, textA, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	b, textB, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || textA == textB {
		t.Fatal("two generated IDs unexpectedly matched")
	}
	parsed, err := ParseID(textA)
	if err != nil || parsed != a {
		t.Fatalf("round trip: %v", err)
	}
	for _, invalid := range []string{"", "00000000-0000-0000-0000-000000000000", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "00000000-0000-4000-0000-000000000000"} {
		if _, err := ParseID(invalid); !errors.Is(err, ErrInvalidID) {
			t.Errorf("ParseID(%q) accepted invalid ID: %v", invalid, err)
		}
	}
}
