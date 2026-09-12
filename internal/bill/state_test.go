package bill_test

import (
	"errors"
	"testing"

	"fees-api/internal/bill"
)

// Spec: billing/bill-lifecycle - "Bill lifecycle states" and "Closed bills are
// terminal". The table is exhaustive over every state/event pair, so a
// transition added to the machine without being considered here fails the test.
func TestTransition(t *testing.T) {
	states := []bill.State{bill.StateOpen, bill.StateClosing, bill.StateClosed}
	events := []bill.Event{bill.EventClose, bill.EventInvoiced}

	legal := map[bill.State]map[bill.Event]bill.State{
		bill.StateOpen:    {bill.EventClose: bill.StateClosing},
		bill.StateClosing: {bill.EventInvoiced: bill.StateClosed},
	}

	for _, s := range states {
		for _, e := range events {
			want, isLegal := legal[s][e]
			name := string(s) + "/" + string(e)
			t.Run(name, func(t *testing.T) {
				got, err := bill.Transition(s, e)
				if !isLegal {
					if err == nil {
						t.Fatalf("Transition(%s, %s) = %s, want an error", s, e, got)
					}
					if !errors.Is(err, bill.ErrInvalidTransition) {
						t.Fatalf("Transition(%s, %s) error = %v, want ErrInvalidTransition", s, e, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Transition(%s, %s) returned error: %v", s, e, err)
				}
				if got != want {
					t.Errorf("Transition(%s, %s) = %s, want %s", s, e, got, want)
				}
			})
		}
	}
}

// A bill that has left OPEN never returns to it. No event reopens a closed or
// closing bill, which is what makes CLOSED terminal in practice and not just by
// convention.
func TestNoEventReturnsABillToOpen(t *testing.T) {
	for _, s := range []bill.State{bill.StateOpen, bill.StateClosing, bill.StateClosed} {
		for _, e := range []bill.Event{bill.EventClose, bill.EventInvoiced} {
			got, err := bill.Transition(s, e)
			if err == nil && got == bill.StateOpen {
				t.Errorf("Transition(%s, %s) = OPEN: no event may reopen a bill", s, e)
			}
		}
	}
}

func TestTransitionRejectsUnknownState(t *testing.T) {
	if _, err := bill.Transition(bill.State("SETTLED"), bill.EventClose); !errors.Is(err, bill.ErrInvalidTransition) {
		t.Fatalf("Transition from an unknown state error = %v, want ErrInvalidTransition", err)
	}
}

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		state bill.State
		want  bool
	}{
		{state: bill.StateOpen, want: false},
		{state: bill.StateClosing, want: false},
		{state: bill.StateClosed, want: true},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := bill.IsTerminal(tc.state); got != tc.want {
				t.Errorf("IsTerminal(%s) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestAcceptsLineItemsOnlyWhileOpen(t *testing.T) {
	tests := []struct {
		state bill.State
		want  bool
	}{
		{state: bill.StateOpen, want: true},
		{state: bill.StateClosing, want: false},
		{state: bill.StateClosed, want: false},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := bill.AcceptsLineItems(tc.state); got != tc.want {
				t.Errorf("AcceptsLineItems(%s) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}
