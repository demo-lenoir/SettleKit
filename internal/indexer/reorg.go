package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"settlekit/internal/payments"
	"settlekit/internal/store"
)

type paymentRow struct {
	id, status                        string
	version                           int64
	escrow, observedBlock, observedTx []byte
	expiry                            time.Time
}

type derived struct {
	status            payments.Status
	blockHash, txHash []byte
}

func (w *Indexer) reconcile(ctx context.Context, oldNumber uint64, oldHash common.Hash, rpcHead uint64) error {
	var ancestor Block
	found := false
	for depth := uint64(0); depth <= w.maxReorgDepth && depth <= oldNumber; depth++ {
		number := oldNumber - depth
		if number < w.startBlock {
			break
		}
		if number > rpcHead {
			continue
		}
		candidate, err := w.source.Block(ctx, number)
		if err != nil {
			return fmt.Errorf("find reorg ancestor block %d: %w", number, err)
		}
		var stored []byte
		err = w.pool.QueryRow(ctx, `SELECT block_hash FROM chain_blocks WHERE chain_id=$1 AND block_number=$2 AND canonical`, w.chainID, int64(number)).Scan(&stored)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("missing canonical block %d: %w", number, ErrCanonicalDiscontinuity)
		}
		if err != nil {
			return fmt.Errorf("read canonical block %d: %w", number, err)
		}
		if bytes.Equal(stored, candidate.Hash[:]) {
			ancestor, found = candidate, true
			break
		}
	}
	if !found {
		return fmt.Errorf("no common ancestor within %d blocks: %w", w.maxReorgDepth, ErrCanonicalDiscontinuity)
	}
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin reorg transaction: %w", err)
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	var currentNumber int64
	var currentHash []byte
	if err := tx.QueryRow(ctx, `SELECT canonical_head_number,canonical_head_hash FROM sync_state WHERE chain_id=$1 FOR UPDATE`, w.chainID).Scan(&currentNumber, &currentHash); err != nil {
		return fmt.Errorf("lock reorg checkpoint: %w", err)
	}
	if uint64(currentNumber) != oldNumber || !bytes.Equal(currentHash, oldHash[:]) {
		return fmt.Errorf("checkpoint changed during reorg: %w", ErrCanonicalDiscontinuity)
	}
	if _, err := tx.Exec(ctx, `UPDATE chain_blocks SET canonical=false WHERE chain_id=$1 AND block_number>$2 AND canonical`, w.chainID, int64(ancestor.Number)); err != nil {
		return fmt.Errorf("orphan blocks: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE chain_logs SET removed=true WHERE chain_id=$1 AND block_hash IN
		(SELECT block_hash FROM chain_blocks WHERE chain_id=$1 AND block_number>$2 AND NOT canonical)`, w.chainID, int64(ancestor.Number)); err != nil {
		return fmt.Errorf("orphan logs: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sync_state SET canonical_head_number=$2,canonical_head_hash=$3,updated_at=now() WHERE chain_id=$1`, w.chainID, int64(ancestor.Number), ancestor.Hash[:]); err != nil {
		return fmt.Errorf("rewind checkpoint: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT id::text,status,status_version,escrow_id,expires_at,observed_block_hash,observed_tx_hash
		FROM payment_intents p WHERE chain_id=$1 AND escrow_contract=$2 AND (
		(status='EXPIRED' AND expires_at>$5) OR EXISTS (
		 SELECT 1 FROM chain_blocks b WHERE b.chain_id=p.chain_id AND b.block_hash=p.observed_block_hash AND (
		  NOT b.canonical OR
		  (p.status IN ('CONFIRMED','RELEASED','REFUNDED') AND b.block_number>$3) OR
		  (p.status='CONFIRMING' AND b.block_number=$4))))
		ORDER BY id LIMIT 1001 FOR UPDATE`, w.chainID, w.contract.Bytes(),
		int64(ancestor.Number)-int64(w.confirmations)+1, int64(ancestor.Number), ancestor.Time)
	if err != nil {
		return fmt.Errorf("lock reorg payments: %w", err)
	}
	var paymentsToReconcile []paymentRow
	for rows.Next() {
		var item paymentRow
		if err := rows.Scan(&item.id, &item.status, &item.version, &item.escrow, &item.expiry, &item.observedBlock, &item.observedTx); err != nil {
			rows.Close()
			return err
		}
		paymentsToReconcile = append(paymentsToReconcile, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(paymentsToReconcile) > 1000 {
		return fmt.Errorf("more than 1000 payments affected: %w", ErrCanonicalDiscontinuity)
	}
	for _, item := range paymentsToReconcile {
		outcome, err := w.deriveAt(ctx, tx, item, ancestor)
		if err != nil {
			return err
		}
		if item.status == string(outcome.status) && bytes.Equal(item.observedBlock, outcome.blockHash) && bytes.Equal(item.observedTx, outcome.txHash) {
			continue
		}
		if _, err := payments.Next(payments.Status(item.status), payments.Orphaned); err != nil {
			return fmt.Errorf("reorg transition for %s: %w", item.id, err)
		}
		var revertedID string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM outbox_events WHERE intent_id=$1 AND status_version=$2 ORDER BY created_at DESC LIMIT 1`, item.id, item.version).Scan(&revertedID); err != nil {
			return fmt.Errorf("find event to reverse: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status='REORGED',status_version=status_version+1,observed_block_hash=NULL,observed_tx_hash=NULL,updated_at=now() WHERE id=$1`, item.id); err != nil {
			return fmt.Errorf("mark reorged: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_state_history(intent_id,version,from_status,to_status,reason,chain_block_hash)
			VALUES($1,$2,$3,'REORGED','ORPHANED',$4)`, item.id, item.version+1, item.status, ancestor.Hash[:]); err != nil {
			return fmt.Errorf("record reversal: %w", err)
		}
		if err := store.InsertStateEvent(ctx, tx, store.StateEvent{IntentID: item.id, Version: item.version + 1, PreviousStatus: item.status, Status: "REORGED", Reason: "ORPHANED", BlockHash: ancestor.Hash.Hex(), RevertedEvent: revertedID}, "PAYMENT_REVERSED"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status=$2,status_version=status_version+1,observed_block_hash=$3,observed_tx_hash=$4,updated_at=now() WHERE id=$1`, item.id, outcome.status, outcome.blockHash, outcome.txHash); err != nil {
			return fmt.Errorf("apply reconciled status: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_state_history(intent_id,version,from_status,to_status,reason,chain_block_hash)
			VALUES($1,$2,'REORGED',$3,'RECONCILED',$4)`, item.id, item.version+2, outcome.status, ancestor.Hash[:]); err != nil {
			return fmt.Errorf("record reconciled state: %w", err)
		}
		if err := store.InsertStateEvent(ctx, tx, store.StateEvent{IntentID: item.id, Version: item.version + 2, PreviousStatus: "REORGED", Status: string(outcome.status), Reason: "RECONCILED", BlockHash: store.HashHex(outcome.blockHash), TxHash: store.HashHex(outcome.txHash)}, "PAYMENT_STATE_CHANGED"); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit reorg; outcome must be checked by checkpoint: %w", err)
	}
	w.lastCheckpoint.Store(ancestor.Number)
	w.reorgCount.Add(1)
	return nil
}

func (w *Indexer) deriveAt(ctx context.Context, tx pgx.Tx, item paymentRow, ancestor Block) (derived, error) {
	rows, err := tx.Query(ctx, `SELECT b.block_number,l.block_hash,l.tx_hash FROM chain_logs l
		JOIN chain_blocks b ON b.chain_id=l.chain_id AND b.block_hash=l.block_hash
		WHERE l.chain_id=$1 AND l.contract_address=$2 AND l.escrow_id=$3 AND l.event_kind='EscrowFunded'
		AND b.canonical AND NOT l.removed AND EXISTS
		(SELECT 1 FROM chain_logs c WHERE c.chain_id=l.chain_id AND c.block_hash=l.block_hash AND c.tx_hash=l.tx_hash
		AND c.escrow_id=l.escrow_id AND c.event_kind='EscrowCreated' AND NOT c.removed)
		ORDER BY b.block_number LIMIT 2`, w.chainID, w.contract.Bytes(), item.escrow)
	if err != nil {
		return derived{}, fmt.Errorf("derive funding: %w", err)
	}
	type evidence struct {
		number    int64
		block, tx []byte
	}
	var funding []evidence
	for rows.Next() {
		var e evidence
		if err := rows.Scan(&e.number, &e.block, &e.tx); err != nil {
			rows.Close()
			return derived{}, err
		}
		funding = append(funding, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return derived{}, err
	}
	if len(funding) > 1 {
		return derived{}, errors.New("multiple canonical funding transactions for one escrow")
	}
	if len(funding) == 0 {
		if !ancestor.Time.Before(item.expiry) {
			return derived{status: payments.Expired}, nil
		}
		return derived{status: payments.AwaitingChain}, nil
	}
	base := funding[0]
	count := ancestor.Number - uint64(base.number) + 1
	if count < w.confirmations {
		status := payments.Confirming
		if count == 1 {
			status = payments.Observed
		}
		return derived{status: status, blockHash: base.block, txHash: base.tx}, nil
	}
	maxTerminal := int64(ancestor.Number - w.confirmations + 1)
	terminalRows, err := tx.Query(ctx, `SELECT l.event_kind,l.block_hash,l.tx_hash FROM chain_logs l
		JOIN chain_blocks b ON b.chain_id=l.chain_id AND b.block_hash=l.block_hash
		WHERE l.chain_id=$1 AND l.contract_address=$2 AND l.escrow_id=$3 AND l.event_kind IN ('EscrowReleased','EscrowRefunded')
		AND b.canonical AND NOT l.removed AND b.block_number<=$4 ORDER BY b.block_number LIMIT 2`, w.chainID, w.contract.Bytes(), item.escrow, maxTerminal)
	if err != nil {
		return derived{}, fmt.Errorf("derive terminal event: %w", err)
	}
	type terminalEvidence struct {
		kind      string
		block, tx []byte
	}
	var terminal []terminalEvidence
	for terminalRows.Next() {
		var e terminalEvidence
		if err := terminalRows.Scan(&e.kind, &e.block, &e.tx); err != nil {
			terminalRows.Close()
			return derived{}, err
		}
		terminal = append(terminal, e)
	}
	err = terminalRows.Err()
	terminalRows.Close()
	if err != nil {
		return derived{}, err
	}
	if len(terminal) > 1 {
		return derived{}, errors.New("multiple canonical terminal transactions for one escrow")
	}
	if len(terminal) == 1 {
		status := payments.Released
		if terminal[0].kind == "EscrowRefunded" {
			status = payments.Refunded
		}
		return derived{status: status, blockHash: terminal[0].block, txHash: terminal[0].tx}, nil
	}
	return derived{status: payments.Confirmed, blockHash: base.block, txHash: base.tx}, nil
}
