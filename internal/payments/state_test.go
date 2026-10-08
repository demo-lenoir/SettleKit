package payments

import (
	"errors"
	"testing"
	"time"
)

func TestTransitions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from   Status
		signal Signal
		want   Status
	}{
		{Created, IntentAccepted, AwaitingChain},
		{AwaitingChain, FundingObserved, Observed},
		{Observed, ObservationValidated, Confirming},
		{Observed, FundingConfirmed, Confirmed},
		{Confirming, FundingConfirmed, Confirmed},
		{Confirmed, ReleaseConfirmed, Released},
		{Confirmed, RefundConfirmed, Refunded},
		{AwaitingChain, PermanentFailure, Failed},
		{Observed, Orphaned, Reorged},
		{Confirming, Orphaned, Reorged},
		{Confirmed, Orphaned, Reorged},
		{Released, Orphaned, Reorged},
		{Refunded, Orphaned, Reorged},
		{Expired, Orphaned, Reorged},
		{Reorged, ReconcileEmpty, AwaitingChain},
		{Reorged, ReconcileObserved, Observed},
		{Reorged, ReconcileConfirming, Confirming},
		{Reorged, ReconcileConfirmed, Confirmed},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"/"+string(tc.signal), func(t *testing.T) {
			t.Parallel()
			got, err := Next(tc.from, tc.signal)
			if err != nil || got != tc.want {
				t.Fatalf("Next(%s,%s)=(%s,%v), want %s", tc.from, tc.signal, got, err, tc.want)
			}
		})
	}
}

func TestInvalidTransitions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from   Status
		signal Signal
	}{
		{Created, ReleaseConfirmed},
		{AwaitingChain, ReleaseConfirmed},
		{Observed, RefundConfirmed},
		{Confirming, ReleaseConfirmed},
		{Released, RefundConfirmed},
		{Refunded, ReleaseConfirmed},
		{Expired, FundingObserved},
		{Failed, FundingObserved},
		{Status("BROKEN"), IntentAccepted},
	}
	for _, tc := range cases {
		if _, err := Next(tc.from, tc.signal); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("Next(%s,%s): expected invalid transition, got %v", tc.from, tc.signal, err)
		}
	}
}

func TestConfirmationLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event, head, threshold uint64
		first                  bool
		want                   Status
	}{
		{10, 10, 2, true, Observed},
		{10, 10, 2, false, Confirming},
		{10, 11, 2, false, Confirmed},
		{10, 11, 3, false, Confirming},
		{10, 12, 3, false, Confirmed},
	}
	for _, tc := range cases {
		got, err := ConfirmationLevel(tc.event, tc.head, tc.threshold, tc.first)
		if err != nil || got != tc.want {
			t.Errorf("ConfirmationLevel(%d,%d,%d,%t)=(%s,%v), want %s", tc.event, tc.head, tc.threshold, tc.first, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ event, head, threshold uint64 }{{10, 9, 2}, {10, 10, 0}, {10, 10, 1}} {
		if _, err := ConfirmationLevel(tc.event, tc.head, tc.threshold, false); !errors.Is(err, ErrInvalidConfirmation) {
			t.Errorf("expected invalid confirmation for %+v, got %v", tc, err)
		}
	}
}

func TestExpireUnfunded(t *testing.T) {
	t.Parallel()
	deadline := time.Unix(1_800_000_000, 0)
	if got, err := ExpireUnfunded(AwaitingChain, deadline, deadline, true); err != nil || got != Expired {
		t.Fatalf("at deadline: got (%s,%v)", got, err)
	}
	for _, tc := range []struct {
		status  Status
		now     time.Time
		indexed bool
	}{
		{AwaitingChain, deadline.Add(-time.Nanosecond), true},
		{AwaitingChain, deadline.Add(time.Hour), false},
		{Observed, deadline, true},
		{Confirmed, deadline.Add(time.Hour), true},
	} {
		if _, err := ExpireUnfunded(tc.status, tc.now, deadline, tc.indexed); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("expected rejection for %+v, got %v", tc, err)
		}
	}
}

func FuzzTerminalCannotCrossToOtherTerminal(f *testing.F) {
	f.Add("RELEASED", "REFUND_CONFIRMED")
	f.Add("REFUNDED", "RELEASE_CONFIRMED")
	f.Fuzz(func(t *testing.T, rawStatus, rawSignal string) {
		status := Status(rawStatus)
		signal := Signal(rawSignal)
		got, err := Next(status, signal)
		if status == Released && got == Refunded && err == nil {
			t.Fatal("released escrow became refunded")
		}
		if status == Refunded && got == Released && err == nil {
			t.Fatal("refunded escrow became released")
		}
	})
}
