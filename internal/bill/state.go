// Package bill holds the fee-bill domain: its lifecycle state machine, the
// aggregate that accrues line items, and the frozen invoice a close produces.
//
// Nothing here imports Temporal or Encore, so the lifecycle rules are pure
// functions over in-memory state and can be tested without running a workflow.
package bill

import (
	"errors"
	"fmt"
)

// State is a bill's position in its lifecycle.
//
//	   create
//	     |
//	     v
//	+---------+  line items accepted
//	|  OPEN   |
//	+----+----+
//	     |  close request, or period end
//	     v
//	+---------+  totals frozen; invoice hand-off pending
//	| CLOSING |
//	+----+----+
//	     |  invoice emitted
//	     v
//	+---------+  terminal; immutable
//	| CLOSED  |
//	+---------+
type State string

const (
	StateOpen State = "OPEN"

	// StateClosing exists because the invoice hand-off can fail and be retried:
	// the totals must be final before it starts, but the bill is not yet done.
	StateClosing State = "CLOSING"

	StateClosed State = "CLOSED"
)

// Event is something that happens to a bill and may move it between states.
type Event string

const (
	EventClose    Event = "CLOSE"
	EventInvoiced Event = "INVOICED"
)

// Trigger records what caused a bill to close. It is reported to the caller so
// that a client whose close request arrived just after the period-end deadline
// can tell that the bill closed without its involvement.
type Trigger string

const (
	TriggerAPIRequest Trigger = "api_request"
	TriggerPeriodEnd  Trigger = "period_end"
)

// ErrInvalidTransition is returned when an event cannot be applied to a state.
var ErrInvalidTransition = errors.New("bill: invalid state transition")

// transitions is the complete set of legal moves. Any pair absent from this
// table is illegal - including every route back to OPEN, because a bill that has
// left OPEN never returns to it.
var transitions = map[State]map[Event]State{
	StateOpen: {
		EventClose: StateClosing,
	},
	StateClosing: {
		EventInvoiced: StateClosed,
	},
	StateClosed: {},
}

// Transition applies e to s and returns the resulting state.
func Transition(s State, e Event) (State, error) {
	byEvent, ok := transitions[s]
	if !ok {
		return "", fmt.Errorf("%w: unknown state %q", ErrInvalidTransition, s)
	}
	next, ok := byEvent[e]
	if !ok {
		return "", fmt.Errorf("%w: cannot apply %q to a bill in %q", ErrInvalidTransition, e, s)
	}
	return next, nil
}

// IsTerminal reports whether no further transition is possible from s.
func IsTerminal(s State) bool { return len(transitions[s]) == 0 }

// AcceptsLineItems reports whether a bill in s may still accrue charges.
func AcceptsLineItems(s State) bool { return s == StateOpen }
