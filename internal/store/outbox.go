package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"settlekit/internal/payments"
)

type StateEvent struct {
	ID             string `json:"event_id"`
	IntentID       string `json:"intent_id"`
	Version        int64  `json:"status_version"`
	PreviousStatus string `json:"previous_status"`
	Status         string `json:"status"`
	Reason         string `json:"reason"`
	BlockHash      string `json:"block_hash,omitempty"`
	TxHash         string `json:"tx_hash,omitempty"`
	RevertedEvent  string `json:"reverted_event_id,omitempty"`
}

func InsertStateEvent(ctx context.Context, tx pgx.Tx, event StateEvent, kind string) error {
	_, id, err := payments.NewID()
	if err != nil {
		return fmt.Errorf("new outbox event ID: %w", err)
	}
	event.ID = id
	if kind != "PAYMENT_STATE_CHANGED" && kind != "PAYMENT_REVERSED" {
		return fmt.Errorf("invalid outbox event kind %q", kind)
	}
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode outbox event: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(id,intent_id,status_version,kind,body)
		VALUES($1,$2,$3,$4,$5)`, id, event.IntentID, event.Version, kind, body)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

func HashHex(raw []byte) string {
	if len(raw) != common.HashLength {
		return ""
	}
	return common.BytesToHash(raw).Hex()
}

var ErrOutboxNotDead = errors.New("outbox event is not dead-lettered")

func RequeueDeadLetter(ctx context.Context, pool *pgxpool.Pool, eventID string) error {
	bounded, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	tag, err := pool.Exec(bounded, `UPDATE outbox_events SET attempts=0,next_attempt_at=now(),lease_owner=NULL,
		lease_until=NULL,dead_lettered_at=NULL WHERE id=$1 AND dead_lettered_at IS NOT NULL AND delivered_at IS NULL`, eventID)
	if err != nil {
		return fmt.Errorf("requeue dead-letter event: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrOutboxNotDead
	}
	return nil
}
