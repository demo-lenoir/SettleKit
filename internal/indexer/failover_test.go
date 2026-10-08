package indexer

import (
	"context"
	"errors"
	"testing"
)

type flakySource struct {
	*fakeSource
	failHead bool
}

func (s *flakySource) Head(ctx context.Context) (uint64, error) {
	if s.failHead {
		return 0, errors.New("primary unavailable")
	}
	return s.fakeSource.Head(ctx)
}

func TestFailoverRequiresExactCheckpointAndStaysOnFallback(t *testing.T) {
	block := Block{Number: 1, Hash: fakeHash(87)}
	primary := &flakySource{fakeSource: &fakeSource{chainID: 31337, head: 1, blocks: map[uint64]Block{1: block}}, failHead: true}
	fallback := &fakeSource{chainID: 31337, head: 2, blocks: map[uint64]Block{1: block}}
	f, err := NewFailover(primary, fallback, 31337)
	if err != nil {
		t.Fatal(err)
	}
	f.SetCheckpoint(1, block.Hash, true)
	head, err := f.Head(context.Background())
	if err != nil || head != 2 || !f.Degraded() {
		t.Fatalf("healthy fallback rejected: head=%d degraded=%t err=%v", head, f.Degraded(), err)
	}
	primary.failHead = false
	fallback.head = 3
	head, err = f.Head(context.Background())
	if err != nil || head != 3 {
		t.Fatalf("unexpected failback: head=%d err=%v", head, err)
	}
	for _, test := range []struct {
		name    string
		chainID int64
		head    uint64
		hash    byte
	}{
		{"wrong chain", 1, 2, 87}, {"stale", 31337, 0, 87}, {"wrong checkpoint", 31337, 2, 88},
	} {
		t.Run(test.name, func(t *testing.T) {
			other := &fakeSource{chainID: test.chainID, head: test.head, blocks: map[uint64]Block{1: {Number: 1, Hash: fakeHash(test.hash)}}}
			candidate, err := NewFailover(primary, other, 31337)
			if err != nil {
				t.Fatal(err)
			}
			candidate.SetCheckpoint(1, block.Hash, true)
			primary.failHead = true
			if _, err := candidate.Head(context.Background()); err == nil || candidate.Degraded() {
				t.Fatalf("unsafe fallback accepted: %v", err)
			}
		})
	}
}
