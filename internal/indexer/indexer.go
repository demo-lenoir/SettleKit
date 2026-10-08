package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"settlekit/internal/chain"
	"settlekit/internal/payments"
	"settlekit/internal/store"
)

const maxBlocksPerPoll = 32

var ErrCanonicalDiscontinuity = errors.New("canonical chain discontinuity; reconciliation required")

type Indexer struct {
	pool           *pgxpool.Pool
	source         Source
	chainID        int64
	contract       common.Address
	confirmations  uint64
	startBlock     uint64
	maxReorgDepth  uint64
	lastRPCHead    atomic.Uint64
	lastCheckpoint atomic.Uint64
	reorgCount     atomic.Uint64
}

func New(pool *pgxpool.Pool, source Source, chainID uint64, contract common.Address, confirmations, startBlock, maxReorgDepth uint64) (*Indexer, error) {
	if pool == nil || source == nil || chainID == 0 || chainID > math.MaxInt64 || contract == (common.Address{}) || confirmations < 2 || confirmations > 128 || startBlock > math.MaxInt64 || maxReorgDepth == 0 || maxReorgDepth > 256 {
		return nil, errors.New("invalid indexer configuration")
	}
	return &Indexer{pool: pool, source: source, chainID: int64(chainID), contract: contract, confirmations: confirmations, startBlock: startBlock, maxReorgDepth: maxReorgDepth}, nil
}

func (w *Indexer) RunOnce(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	checkpoint, hash, hasCheckpoint, err := w.checkpoint(bounded)
	if err != nil {
		return err
	}
	if pinned, ok := w.source.(interface {
		SetCheckpoint(uint64, common.Hash, bool)
	}); ok {
		pinned.SetCheckpoint(checkpoint, hash, hasCheckpoint)
	}
	id, err := w.source.ChainID(bounded)
	if err != nil {
		return fmt.Errorf("RPC chain ID: %w", err)
	}
	if !id.IsInt64() || id.Int64() != w.chainID {
		return fmt.Errorf("RPC chain ID %s differs from configured %d", id, w.chainID)
	}
	head, err := w.source.Head(bounded)
	if err != nil {
		return fmt.Errorf("RPC head: %w", err)
	}
	w.lastRPCHead.Store(head)
	if head > math.MaxInt64 {
		return errors.New("RPC head exceeds PostgreSQL bigint")
	}
	if hasCheckpoint && head < checkpoint {
		if degraded, ok := w.source.(interface{ Degraded() bool }); ok && degraded.Degraded() {
			return errors.New("fallback RPC head is behind canonical checkpoint")
		}
		return w.reconcile(bounded, checkpoint, hash, head)
	}
	if hasCheckpoint {
		w.lastCheckpoint.Store(checkpoint)
	}
	var currentBlock Block
	if hasCheckpoint {
		block, err := w.source.Block(bounded, checkpoint)
		if err != nil {
			return err
		}
		if block.Hash != hash {
			return w.reconcile(bounded, checkpoint, hash, head)
		}
		currentBlock = block
	}
	next := w.startBlock
	if hasCheckpoint {
		next = checkpoint + 1
	}
	processed := 0
	for next <= head && processed < maxBlocksPerPoll {
		block, err := w.source.Block(bounded, next)
		if err != nil {
			return err
		}
		if block.Number != next || block.Hash == (common.Hash{}) || (hasCheckpoint && block.Parent != hash) {
			return fmt.Errorf("block %d parent or identity invalid: %w", next, ErrCanonicalDiscontinuity)
		}
		if err := w.commitBlock(bounded, block, checkpoint, hash, hasCheckpoint); err != nil {
			return err
		}
		checkpoint, hash, hasCheckpoint = next, block.Hash, true
		currentBlock = block
		w.lastCheckpoint.Store(next)
		next++
		processed++
	}
	if processed == 0 && hasCheckpoint {
		return w.advancePending(bounded, currentBlock, checkpoint, hash)
	}
	return nil
}

func (w *Indexer) advancePending(ctx context.Context, head Block, number uint64, hash common.Hash) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin pending-state transaction: %w", err)
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	var storedNumber int64
	var storedHash []byte
	if err := tx.QueryRow(ctx, `SELECT canonical_head_number,canonical_head_hash FROM sync_state WHERE chain_id=$1 FOR UPDATE`, w.chainID).Scan(&storedNumber, &storedHash); err != nil {
		return fmt.Errorf("lock pending-state checkpoint: %w", err)
	}
	if uint64(storedNumber) != number || !bytes.Equal(storedHash, hash[:]) {
		return fmt.Errorf("pending-state checkpoint changed: %w", ErrCanonicalDiscontinuity)
	}
	if err := w.advanceFunding(ctx, tx, head); err != nil {
		return err
	}
	if err := w.advanceTerminal(ctx, tx, head); err != nil {
		return err
	}
	if err := w.expireUnfunded(ctx, tx, head); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pending-state transaction: %w", err)
	}
	return nil
}

func (w *Indexer) Progress() (checkpoint, head, reorgs uint64) {
	return w.lastCheckpoint.Load(), w.lastRPCHead.Load(), w.reorgCount.Load()
}

func (w *Indexer) checkpoint(ctx context.Context) (uint64, common.Hash, bool, error) {
	var number *int64
	var raw []byte
	err := w.pool.QueryRow(ctx, `SELECT canonical_head_number, canonical_head_hash FROM sync_state WHERE chain_id=$1`, w.chainID).Scan(&number, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, common.Hash{}, false, nil
	}
	if err != nil {
		return 0, common.Hash{}, false, fmt.Errorf("read chain checkpoint: %w", err)
	}
	if number == nil {
		return 0, common.Hash{}, false, nil
	}
	if len(raw) != common.HashLength || *number < 0 {
		return 0, common.Hash{}, false, errors.New("corrupted chain checkpoint")
	}
	return uint64(*number), common.BytesToHash(raw), true, nil
}

func (w *Indexer) commitBlock(ctx context.Context, block Block, priorNumber uint64, priorHash common.Hash, hadPrior bool) error {
	created, err := w.validatedCreations(block)
	if err != nil {
		return err
	}
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin block transaction: %w", err)
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	_, err = tx.Exec(ctx, `INSERT INTO sync_state(chain_id) VALUES($1) ON CONFLICT DO NOTHING`, w.chainID)
	if err != nil {
		return fmt.Errorf("initialize sync state: %w", err)
	}
	var actualNumber *int64
	var actualHash []byte
	if err := tx.QueryRow(ctx, `SELECT canonical_head_number, canonical_head_hash FROM sync_state WHERE chain_id=$1 FOR UPDATE`, w.chainID).Scan(&actualNumber, &actualHash); err != nil {
		return fmt.Errorf("lock sync state: %w", err)
	}
	if hadPrior {
		if actualNumber == nil || uint64(*actualNumber) != priorNumber || !bytes.Equal(actualHash, priorHash[:]) {
			return fmt.Errorf("checkpoint changed during poll: %w", ErrCanonicalDiscontinuity)
		}
	} else if actualNumber != nil {
		return fmt.Errorf("checkpoint initialized concurrently: %w", ErrCanonicalDiscontinuity)
	}
	var knownBlock bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chain_blocks WHERE chain_id=$1 AND block_hash=$2)`, w.chainID, block.Hash[:]).Scan(&knownBlock); err != nil {
		return err
	}
	var previousCount int
	if knownBlock {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM chain_logs WHERE chain_id=$1 AND block_hash=$2`, w.chainID, block.Hash[:]).Scan(&previousCount); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO chain_blocks(chain_id,block_hash,block_number,parent_hash,canonical)
		VALUES($1,$2,$3,$4,true) ON CONFLICT (chain_id,block_hash) DO UPDATE SET canonical=true
		WHERE chain_blocks.block_number=EXCLUDED.block_number AND chain_blocks.parent_hash=EXCLUDED.parent_hash`,
		w.chainID, block.Hash[:], int64(block.Number), block.Parent[:])
	if err != nil {
		return fmt.Errorf("record canonical block %d: %w", block.Number, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("stored block identity changed: %w", ErrCanonicalDiscontinuity)
	}
	relevantCount := 0
	for _, log := range block.Logs {
		kind, escrowID, relevant := eventIdentity(log)
		if !relevant {
			continue
		}
		relevantCount++
		if log.BlockHash != block.Hash || log.BlockNumber != block.Number || log.Address != w.contract || log.Removed || log.Index > math.MaxInt32 {
			return errors.New("inconsistent log in block")
		}
		payload, err := json.Marshal(map[string]any{"topics": log.Topics, "data": common.Bytes2Hex(log.Data)})
		if err != nil {
			return fmt.Errorf("encode chain log: %w", err)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO chain_logs(chain_id,contract_address,block_hash,tx_hash,log_index,escrow_id,event_kind,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (chain_id,block_hash,tx_hash,log_index) DO UPDATE SET removed=false
			WHERE chain_logs.contract_address=EXCLUDED.contract_address AND chain_logs.escrow_id=EXCLUDED.escrow_id
			AND chain_logs.event_kind=EXCLUDED.event_kind AND chain_logs.payload=EXCLUDED.payload`,
			w.chainID, w.contract.Bytes(), block.Hash[:], log.TxHash[:], int32(log.Index), escrowID[:], kind, payload)
		if err != nil {
			return fmt.Errorf("record %s log: %w", kind, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("stored log identity changed: %w", ErrCanonicalDiscontinuity)
		}
	}
	var storedCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM chain_logs WHERE chain_id=$1 AND block_hash=$2`, w.chainID, block.Hash[:]).Scan(&storedCount); err != nil {
		return fmt.Errorf("check complete stored log set: %w", err)
	}
	if storedCount != relevantCount || (knownBlock && previousCount != relevantCount) {
		return fmt.Errorf("stored log set changed: %w", ErrCanonicalDiscontinuity)
	}

	for _, event := range created {
		if err := w.observeFunding(ctx, tx, block, event); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_state SET canonical_head_number=$2,canonical_head_hash=$3,updated_at=now() WHERE chain_id=$1`, w.chainID, int64(block.Number), block.Hash[:]); err != nil {
		return fmt.Errorf("advance checkpoint: %w", err)
	}
	if err := w.advanceFunding(ctx, tx, block); err != nil {
		return err
	}
	if err := w.advanceTerminal(ctx, tx, block); err != nil {
		return err
	}
	if err := w.expireUnfunded(ctx, tx, block); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit block %d; outcome unknown until checkpoint read: %w", block.Number, err)
	}
	return nil
}

func eventIdentity(log Log) (string, common.Hash, bool) {
	if len(log.Topics) < 2 {
		return "", common.Hash{}, false
	}
	for _, name := range []string{"EscrowCreated", "EscrowFunded", "EscrowReleased", "EscrowRefunded", "EscrowExpired"} {
		if log.Topics[0] == chain.ABI().Events[name].ID {
			return name, log.Topics[1], true
		}
	}
	return "", common.Hash{}, false
}

type creation struct {
	escrowID common.Hash
	txHash   common.Hash
	payer    common.Address
	payee    common.Address
	token    common.Address
	amount   *big.Int
	expiry   uint64
	index    uint
}

func (w *Indexer) validatedCreations(block Block) ([]creation, error) {
	type pair struct{ tx, escrow common.Hash }
	created := map[pair]creation{}
	funded := map[pair]*big.Int{}
	fundedIndex := map[pair]uint{}
	for _, log := range block.Logs {
		if log.Address != w.contract || len(log.Topics) == 0 {
			continue
		}
		kind, escrowID, relevant := eventIdentity(log)
		if !relevant {
			continue
		}
		key := pair{log.TxHash, escrowID}
		switch kind {
		case "EscrowCreated":
			if len(log.Topics) != 4 {
				return nil, errors.New("malformed EscrowCreated topics")
			}
			values, err := chain.ABI().Events[kind].Inputs.NonIndexed().Unpack(log.Data)
			if err != nil || len(values) != 3 {
				return nil, errors.New("malformed EscrowCreated data")
			}
			token, tokenOK := values[0].(common.Address)
			amount, amountOK := values[1].(*big.Int)
			expiry, expiryOK := values[2].(uint64)
			if !tokenOK || !amountOK || !expiryOK || amount.Sign() <= 0 {
				return nil, errors.New("invalid EscrowCreated values")
			}
			if _, exists := created[key]; exists {
				return nil, errors.New("duplicate EscrowCreated in one transaction")
			}
			created[key] = creation{escrowID, log.TxHash, common.BytesToAddress(log.Topics[2].Bytes()[12:]), common.BytesToAddress(log.Topics[3].Bytes()[12:]), token, amount, expiry, log.Index}
		case "EscrowFunded":
			if len(log.Topics) != 2 {
				return nil, errors.New("malformed EscrowFunded topics")
			}
			values, err := chain.ABI().Events[kind].Inputs.NonIndexed().Unpack(log.Data)
			if err != nil || len(values) != 1 {
				return nil, errors.New("malformed EscrowFunded data")
			}
			amount, ok := values[0].(*big.Int)
			if !ok || amount.Sign() <= 0 {
				return nil, errors.New("invalid EscrowFunded amount")
			}
			if _, exists := funded[key]; exists {
				return nil, errors.New("duplicate EscrowFunded in one transaction")
			}
			funded[key], fundedIndex[key] = amount, log.Index
		}
	}
	result := make([]creation, 0, len(created))
	for key, event := range created {
		if amount, ok := funded[key]; ok && amount.Cmp(event.amount) == 0 && fundedIndex[key] > event.index {
			result = append(result, event)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].index < result[j].index })
	return result, nil
}

func (w *Indexer) observeFunding(ctx context.Context, tx pgx.Tx, block Block, event creation) error {
	var id, status, amountText string
	var version int64
	var payer, payee, token []byte
	var expiry time.Time
	err := tx.QueryRow(ctx, `SELECT id::text,status,status_version,payer,payee,token,amount::text,expires_at
		FROM payment_intents WHERE escrow_id=$1 AND chain_id=$2 AND escrow_contract=$3 FOR UPDATE`,
		event.escrowID[:], w.chainID, w.contract.Bytes()).Scan(&id, &status, &version, &payer, &payee, &token, &amountText, &expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("match funding event: %w", err)
	}
	if !bytes.Equal(payer, event.payer.Bytes()) || !bytes.Equal(payee, event.payee.Bytes()) || !bytes.Equal(token, event.token.Bytes()) || amountText != event.amount.String() || expiry.Unix() != int64(event.expiry) {
		return errors.New("funding event contradicts stored intent")
	}
	if status != string(payments.AwaitingChain) {
		return fmt.Errorf("funding event for intent in %s: %w", status, payments.ErrInvalidTransition)
	}
	_, err = tx.Exec(ctx, `UPDATE payment_intents SET status='OBSERVED',status_version=status_version+1,
		observed_block_hash=$2,observed_tx_hash=$3,updated_at=now() WHERE id=$1`, id, block.Hash[:], event.txHash[:])
	if err != nil {
		return fmt.Errorf("observe funding: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_state_history(intent_id,version,from_status,to_status,reason,chain_block_hash)
		SELECT id,status_version,'AWAITING_CHAIN','OBSERVED','FUNDING_OBSERVED',$2 FROM payment_intents WHERE id=$1`, id, block.Hash[:])
	if err != nil {
		return fmt.Errorf("record funding observation: %w", err)
	}
	return store.InsertStateEvent(ctx, tx, store.StateEvent{IntentID: id, Version: version + 1, PreviousStatus: "AWAITING_CHAIN", Status: "OBSERVED", Reason: "FUNDING_OBSERVED", BlockHash: block.Hash.Hex(), TxHash: event.txHash.Hex()}, "PAYMENT_STATE_CHANGED")
}

func (w *Indexer) advanceFunding(ctx context.Context, tx pgx.Tx, head Block) error {
	rows, err := tx.Query(ctx, `SELECT i.id::text,i.status,i.status_version,b.block_number
		FROM payment_intents i JOIN chain_blocks b ON b.chain_id=i.chain_id AND b.block_hash=i.observed_block_hash
		WHERE i.chain_id=$1 AND b.canonical AND b.block_number < $2
		AND (i.status='OBSERVED' OR (i.status='CONFIRMING' AND $2-b.block_number+1 >= $3))
		ORDER BY b.block_number,i.id LIMIT 1000 FOR UPDATE OF i`, w.chainID, int64(head.Number), int64(w.confirmations))
	if err != nil {
		return fmt.Errorf("find confirming payments: %w", err)
	}
	type row struct {
		id, status     string
		version, block int64
	}
	var pending []row
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.id, &item.status, &item.version, &item.block); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range pending {
		next, err := payments.ConfirmationLevel(uint64(item.block), head.Number, w.confirmations, false)
		if err != nil {
			return err
		}
		if string(next) == item.status {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status=$2,status_version=status_version+1,updated_at=now() WHERE id=$1`, item.id, next); err != nil {
			return fmt.Errorf("advance confirmation: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_state_history(intent_id,version,from_status,to_status,reason,chain_block_hash)
			VALUES($1,$2,$3,$4,$5,$6)`, item.id, item.version+1, item.status, next, "CANONICAL_CONFIRMATIONS", head.Hash[:]); err != nil {
			return fmt.Errorf("record confirmation: %w", err)
		}
		if err := store.InsertStateEvent(ctx, tx, store.StateEvent{IntentID: item.id, Version: item.version + 1, PreviousStatus: item.status, Status: string(next), Reason: "CANONICAL_CONFIRMATIONS", BlockHash: head.Hash.Hex()}, "PAYMENT_STATE_CHANGED"); err != nil {
			return err
		}
	}
	return nil
}
