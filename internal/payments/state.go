package payments

import (
	"errors"
	"fmt"
	"time"
)

type Status string

const (
	Created       Status = "CREATED"
	AwaitingChain Status = "AWAITING_CHAIN"
	Observed      Status = "OBSERVED"
	Confirming    Status = "CONFIRMING"
	Confirmed     Status = "CONFIRMED"
	Reorged       Status = "REORGED"
	Released      Status = "RELEASED"
	Refunded      Status = "REFUNDED"
	Expired       Status = "EXPIRED"
	Failed        Status = "FAILED"
)

type Signal string

const (
	IntentAccepted       Signal = "INTENT_ACCEPTED"
	FundingObserved      Signal = "FUNDING_OBSERVED"
	ObservationValidated Signal = "OBSERVATION_VALIDATED"
	FundingConfirmed     Signal = "FUNDING_CONFIRMED"
	ReleaseConfirmed     Signal = "RELEASE_CONFIRMED"
	RefundConfirmed      Signal = "REFUND_CONFIRMED"
	PermanentFailure     Signal = "PERMANENT_FAILURE"
	Orphaned             Signal = "ORPHANED"
	ReconcileEmpty       Signal = "RECONCILE_EMPTY"
	ReconcileObserved    Signal = "RECONCILE_OBSERVED"
	ReconcileConfirming  Signal = "RECONCILE_CONFIRMING"
	ReconcileConfirmed   Signal = "RECONCILE_CONFIRMED"
)

var ErrInvalidTransition = errors.New("invalid payment transition")
var ErrInvalidConfirmation = errors.New("invalid confirmation parameters")

var transitions = map[Status]map[Signal]Status{
	Created:       {IntentAccepted: AwaitingChain},
	AwaitingChain: {FundingObserved: Observed, PermanentFailure: Failed},
	Observed:      {ObservationValidated: Confirming, FundingConfirmed: Confirmed, Orphaned: Reorged},
	Confirming:    {FundingConfirmed: Confirmed, Orphaned: Reorged},
	Confirmed:     {ReleaseConfirmed: Released, RefundConfirmed: Refunded, Orphaned: Reorged},
	Released:      {Orphaned: Reorged},
	Refunded:      {Orphaned: Reorged},
	Reorged: {
		ReconcileEmpty:      AwaitingChain,
		ReconcileObserved:   Observed,
		ReconcileConfirming: Confirming,
		ReconcileConfirmed:  Confirmed,
	},
	Expired: {Orphaned: Reorged},
	Failed:  {},
}

func Next(current Status, signal Signal) (Status, error) {
	allowed, known := transitions[current]
	if !known {
		return "", fmt.Errorf("state %q: %w", current, ErrInvalidTransition)
	}
	next, ok := allowed[signal]
	if !ok {
		return "", fmt.Errorf("state %q signal %q: %w", current, signal, ErrInvalidTransition)
	}
	return next, nil
}

func ConfirmationLevel(eventBlock, canonicalHead, threshold uint64, firstSight bool) (Status, error) {
	if threshold < 2 || canonicalHead < eventBlock {
		return "", fmt.Errorf("block=%d head=%d threshold=%d: %w", eventBlock, canonicalHead, threshold, ErrInvalidConfirmation)
	}
	confirmations := canonicalHead - eventBlock + 1
	if confirmations >= threshold {
		return Confirmed, nil
	}
	if firstSight {
		return Observed, nil
	}
	return Confirming, nil
}

func ExpireUnfunded(current Status, canonicalHeadTime, deadline time.Time, indexedThroughDeadline bool) (Status, error) {
	if current != AwaitingChain || !indexedThroughDeadline || canonicalHeadTime.Before(deadline) {
		return "", fmt.Errorf("status=%q head_time=%s deadline=%s indexed=%t: %w", current, canonicalHeadTime.UTC(), deadline.UTC(), indexedThroughDeadline, ErrInvalidTransition)
	}
	return Expired, nil
}
