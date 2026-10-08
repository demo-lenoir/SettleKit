package indexer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

type FailoverSource struct {
	mu                sync.Mutex
	primary, fallback Source
	chainID           *big.Int
	activeFallback    bool
	checkpoint        uint64
	checkpointHash    common.Hash
	hasCheckpoint     bool
}

func NewFailover(primary, fallback Source, chainID uint64) (*FailoverSource, error) {
	if primary == nil || fallback == nil || chainID == 0 {
		return nil, errors.New("invalid RPC failover configuration")
	}
	return &FailoverSource{primary: primary, fallback: fallback, chainID: new(big.Int).SetUint64(chainID)}, nil
}

func (f *FailoverSource) SetCheckpoint(number uint64, hash common.Hash, exists bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkpoint, f.checkpointHash, f.hasCheckpoint = number, hash, exists
}

func (f *FailoverSource) Degraded() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.activeFallback }

func invoke[T any](f *FailoverSource, ctx context.Context, call func(Source) (T, error)) (T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	active := f.primary
	if f.activeFallback {
		active = f.fallback
	}
	result, err := call(active)
	if err == nil || f.activeFallback || ctx.Err() != nil {
		return result, err
	}
	id, checkErr := f.fallback.ChainID(ctx)
	if checkErr != nil || id == nil || id.Cmp(f.chainID) != 0 {
		var zero T
		return zero, fmt.Errorf("fallback RPC chain identity unavailable or wrong: %w", err)
	}
	if f.hasCheckpoint {
		head, checkErr := f.fallback.Head(ctx)
		if checkErr != nil || head < f.checkpoint {
			var zero T
			return zero, fmt.Errorf("fallback RPC is behind checkpoint: %w", err)
		}
		block, checkErr := f.fallback.Block(ctx, f.checkpoint)
		if checkErr != nil || block.Hash != f.checkpointHash {
			var zero T
			return zero, fmt.Errorf("fallback RPC disagrees with checkpoint: %w", err)
		}
	}
	result, err = call(f.fallback)
	if err == nil {
		f.activeFallback = true
	}
	return result, err
}

func (f *FailoverSource) ChainID(ctx context.Context) (*big.Int, error) {
	return invoke(f, ctx, func(s Source) (*big.Int, error) { return s.ChainID(ctx) })
}
func (f *FailoverSource) Head(ctx context.Context) (uint64, error) {
	return invoke(f, ctx, func(s Source) (uint64, error) { return s.Head(ctx) })
}
func (f *FailoverSource) Block(ctx context.Context, n uint64) (Block, error) {
	return invoke(f, ctx, func(s Source) (Block, error) { return s.Block(ctx, n) })
}
