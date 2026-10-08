package indexer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"settlekit/internal/chain"
	"settlekit/internal/payments"
	"settlekit/internal/store"
)

func (w *Indexer) advanceTerminal(ctx context.Context, tx pgx.Tx, head Block) error {
	if head.Number+1 < w.confirmations {
		return nil
	}
	maxEventBlock := int64(head.Number - w.confirmations + 1)
	rows, err := tx.Query(ctx, `SELECT i.id::text,i.status_version,i.escrow_id,i.payer,i.payee,i.amount::text,
		l.event_kind,l.payload,l.block_hash,l.tx_hash
		FROM chain_logs l
		JOIN chain_blocks b ON b.chain_id=l.chain_id AND b.block_hash=l.block_hash
		JOIN payment_intents i ON i.chain_id=l.chain_id AND i.escrow_contract=l.contract_address AND i.escrow_id=l.escrow_id
		WHERE l.chain_id=$1 AND l.contract_address=$2 AND l.event_kind IN ('EscrowReleased','EscrowRefunded')
		AND NOT l.removed AND b.canonical AND b.block_number <= $3 AND i.status='CONFIRMED'
		ORDER BY b.block_number,l.log_index LIMIT 1000 FOR UPDATE OF i`, w.chainID, w.contract.Bytes(), maxEventBlock)
	if err != nil {
		return fmt.Errorf("find terminal observations: %w", err)
	}
	type terminal struct {
		id                   string
		version              int64
		escrow, payer, payee []byte
		amount, kind         string
		payload              json.RawMessage
		blockHash, txHash    []byte
	}
	var pending []terminal
	for rows.Next() {
		var item terminal
		if err := rows.Scan(&item.id, &item.version, &item.escrow, &item.payer, &item.payee, &item.amount, &item.kind, &item.payload, &item.blockHash, &item.txHash); err != nil {
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
	seen := map[string]bool{}
	for _, item := range pending {
		if seen[item.id] {
			return errors.New("multiple canonical terminal events for one intent")
		}
		seen[item.id] = true
		var raw struct {
			Topics []common.Hash `json:"topics"`
			Data   string        `json:"data"`
		}
		if err := json.Unmarshal(item.payload, &raw); err != nil {
			return fmt.Errorf("decode terminal log payload: %w", err)
		}
		if len(raw.Topics) != 3 || raw.Topics[0] != chain.ABI().Events[item.kind].ID || raw.Topics[1] != common.BytesToHash(item.escrow) {
			return errors.New("terminal log topics contradict intent")
		}
		address := item.payee
		next := payments.Released
		reason := "RELEASE_CONFIRMED"
		if item.kind == "EscrowRefunded" {
			address, next, reason = item.payer, payments.Refunded, "REFUND_CONFIRMED"
		}
		if raw.Topics[2] != common.BytesToHash(address) {
			return errors.New("terminal recipient contradicts intent")
		}
		data, err := hex.DecodeString(raw.Data)
		if err != nil {
			return fmt.Errorf("decode terminal amount: %w", err)
		}
		values, err := chain.ABI().Events[item.kind].Inputs.NonIndexed().Unpack(data)
		if err != nil || len(values) != 1 {
			return errors.New("malformed terminal amount")
		}
		amount, ok := values[0].(*big.Int)
		if !ok || amount.String() != item.amount {
			return errors.New("terminal amount contradicts intent")
		}
		if _, err := payments.Next(payments.Confirmed, map[payments.Status]payments.Signal{payments.Released: payments.ReleaseConfirmed, payments.Refunded: payments.RefundConfirmed}[next]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status=$2,status_version=status_version+1,
			observed_block_hash=$3,observed_tx_hash=$4,updated_at=now() WHERE id=$1`, item.id, next, item.blockHash, item.txHash); err != nil {
			return fmt.Errorf("apply terminal observation: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_state_history(intent_id,version,from_status,to_status,reason,chain_block_hash)
			VALUES($1,$2,'CONFIRMED',$3,$4,$5)`, item.id, item.version+1, next, reason, item.blockHash); err != nil {
			return fmt.Errorf("record terminal observation: %w", err)
		}
		if err := store.InsertStateEvent(ctx, tx, store.StateEvent{IntentID: item.id, Version: item.version + 1, PreviousStatus: "CONFIRMED", Status: string(next), Reason: reason, BlockHash: store.HashHex(item.blockHash), TxHash: store.HashHex(item.txHash)}, "PAYMENT_STATE_CHANGED"); err != nil {
			return err
		}
	}
	return nil
}

func (w *Indexer) expireUnfunded(ctx context.Context, tx pgx.Tx, head Block) error {
	rows, err := tx.Query(ctx, `SELECT id::text,status_version,expires_at FROM payment_intents
		WHERE chain_id=$1 AND escrow_contract=$2 AND status='AWAITING_CHAIN' AND expires_at <= $3
		ORDER BY expires_at,id LIMIT 1000 FOR UPDATE`, w.chainID, w.contract.Bytes(), head.Time)
	if err != nil {
		return fmt.Errorf("find expired unfunded intents: %w", err)
	}
	type candidate struct {
		id      string
		version int64
		expiry  time.Time
	}
	var pending []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.version, &item.expiry); err != nil {
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
		if _, err := payments.ExpireUnfunded(payments.AwaitingChain, head.Time, item.expiry, true); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status='EXPIRED',status_version=status_version+1,updated_at=now() WHERE id=$1`, item.id); err != nil {
			return fmt.Errorf("expire unfunded intent: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_state_history(intent_id,version,from_status,to_status,reason,chain_block_hash)
			VALUES($1,$2,'AWAITING_CHAIN','EXPIRED','CANONICAL_DEADLINE',$3)`, item.id, item.version+1, head.Hash[:]); err != nil {
			return fmt.Errorf("record unfunded expiry: %w", err)
		}
		if err := store.InsertStateEvent(ctx, tx, store.StateEvent{IntentID: item.id, Version: item.version + 1, PreviousStatus: "AWAITING_CHAIN", Status: "EXPIRED", Reason: "CANONICAL_DEADLINE", BlockHash: head.Hash.Hex()}, "PAYMENT_STATE_CHANGED"); err != nil {
			return err
		}
	}
	return nil
}
