package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound            = errors.New("payment intent not found")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different request")
	ErrInvalidPaymentState = errors.New("payment state does not allow operation")
	ErrActionConflict      = errors.New("operator action already exists")
	ErrIdempotencyNotFound = errors.New("idempotency key not found")
)

type Idempotency struct {
	Principal string
	Method    string
	Route     string
	Key       string
	Hash      [32]byte
}

type Response struct {
	Status   int
	Body     []byte
	Replayed bool
}

type Intent struct {
	ID                string
	MerchantPrincipal string
	EscrowID          []byte
	ChainID           int64
	EscrowContract    []byte
	Payer             []byte
	Payee             []byte
	Token             []byte
	Amount            string
	Status            string
	StatusVersion     int64
	ExpiresAt         time.Time
	ObservedBlockHash []byte
	ObservedTxHash    []byte
}

type Action struct {
	ID        string
	IntentID  string
	Principal string
	Kind      string
	CallData  []byte
}

const operationTimeout = 5 * time.Second

func FindIdempotency(ctx context.Context, pool *pgxpool.Pool, idem Idempotency) (Response, error) {
	bounded, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var storedHash []byte
	var response Response
	err := pool.QueryRow(bounded, `SELECT request_hash, response_status, response_body
		FROM idempotency_records WHERE principal=$1 AND method=$2 AND route=$3 AND key=$4`,
		idem.Principal, idem.Method, idem.Route, idem.Key).Scan(&storedHash, &response.Status, &response.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Response{}, ErrIdempotencyNotFound
	}
	if err != nil {
		return Response{}, fmt.Errorf("find idempotency result: %w", err)
	}
	if !bytes.Equal(storedHash, idem.Hash[:]) {
		return Response{}, ErrIdempotencyConflict
	}
	response.Replayed = true
	return response, nil
}

func CreateIntent(ctx context.Context, pool *pgxpool.Pool, idem Idempotency, intent Intent, responseBody []byte) (Response, error) {
	return idempotent(ctx, pool, idem, 201, responseBody, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_intents
			(id, merchant_principal, escrow_id, chain_id, escrow_contract, payer, payee, token, amount, status, expires_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			intent.ID, intent.MerchantPrincipal, intent.EscrowID, intent.ChainID,
			intent.EscrowContract, intent.Payer, intent.Payee, intent.Token, intent.Amount, intent.Status, intent.ExpiresAt)
		if err != nil {
			return fmt.Errorf("insert payment intent: %w", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO payment_state_history
			(intent_id, version, from_status, to_status, reason)
			VALUES($1,0,'CREATED','AWAITING_CHAIN','INTENT_ACCEPTED')`, intent.ID)
		if err != nil {
			return fmt.Errorf("insert initial payment history: %w", err)
		}
		return InsertStateEvent(ctx, tx, StateEvent{IntentID: intent.ID, Version: 0, PreviousStatus: "CREATED", Status: "AWAITING_CHAIN", Reason: "INTENT_ACCEPTED"}, "PAYMENT_STATE_CHANGED")
	})
}

func GetIntent(ctx context.Context, pool *pgxpool.Pool, id, merchantPrincipal string) (Intent, error) {
	bounded, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var in Intent
	err := pool.QueryRow(bounded, `SELECT id::text, merchant_principal, escrow_id, chain_id, escrow_contract,
		payer, payee, token, amount::text, status, status_version, expires_at,
		observed_block_hash, observed_tx_hash
		FROM payment_intents WHERE id=$1 AND merchant_principal=$2`, id, merchantPrincipal).Scan(
		&in.ID, &in.MerchantPrincipal, &in.EscrowID, &in.ChainID, &in.EscrowContract,
		&in.Payer, &in.Payee, &in.Token, &in.Amount, &in.Status, &in.StatusVersion,
		&in.ExpiresAt, &in.ObservedBlockHash, &in.ObservedTxHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, ErrNotFound
	}
	if err != nil {
		return Intent{}, fmt.Errorf("get payment intent: %w", err)
	}
	return in, nil
}

func RequestAction(ctx context.Context, pool *pgxpool.Pool, idem Idempotency, action Action, responseBody []byte) (Response, error) {
	return idempotent(ctx, pool, idem, 202, responseBody, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM payment_intents WHERE id=$1 FOR UPDATE`, action.IntentID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock payment intent: %w", err)
		}
		if status != "CONFIRMED" {
			return fmt.Errorf("status %s: %w", status, ErrInvalidPaymentState)
		}
		_, err = tx.Exec(ctx, `INSERT INTO operator_actions(id,intent_id,principal,kind,call_data)
			VALUES($1,$2,$3,$4,$5)`, action.ID, action.IntentID, action.Principal, action.Kind, action.CallData)
		if isUniqueViolation(err) {
			return ErrActionConflict
		}
		if err != nil {
			return fmt.Errorf("insert operator action: %w", err)
		}
		return nil
	})
}

func idempotent(ctx context.Context, pool *pgxpool.Pool, idem Idempotency, responseStatus int, responseBody []byte, effect func(context.Context, pgx.Tx) error) (Response, error) {
	bounded, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	tx, err := pool.Begin(bounded)
	if err != nil {
		return Response{}, fmt.Errorf("begin idempotent operation: %w", err)
	}
	defer func() {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer rollbackCancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	tag, err := tx.Exec(bounded, `INSERT INTO idempotency_records
		(principal, method, route, key, request_hash, response_status, response_body)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`,
		idem.Principal, idem.Method, idem.Route, idem.Key, idem.Hash[:], responseStatus, responseBody)
	if err != nil {
		return Response{}, fmt.Errorf("reserve idempotency key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var storedHash []byte
		var stored Response
		err := tx.QueryRow(bounded, `SELECT request_hash, response_status, response_body
			FROM idempotency_records WHERE principal=$1 AND method=$2 AND route=$3 AND key=$4`,
			idem.Principal, idem.Method, idem.Route, idem.Key).Scan(&storedHash, &stored.Status, &stored.Body)
		if err != nil {
			return Response{}, fmt.Errorf("read idempotency result: %w", err)
		}
		if !bytes.Equal(storedHash, idem.Hash[:]) {
			return Response{}, ErrIdempotencyConflict
		}
		stored.Replayed = true
		return stored, nil
	}
	if err := effect(bounded, tx); err != nil {
		return Response{}, err
	}
	if err := tx.Commit(bounded); err != nil {
		return Response{}, fmt.Errorf("commit idempotent operation; outcome must be checked by key: %w", err)
	}
	return Response{Status: responseStatus, Body: responseBody}, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
