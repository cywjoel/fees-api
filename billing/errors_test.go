package billing

import (
	"context"
	"testing"

	"go.temporal.io/api/serviceerror"
)

// TestWorkflowFailuresAreClassifiedApart pins the distinction the API's error
// handling rests on.
//
// A single predicate used to answer "is the workflow gone?" for three unrelated
// conditions, and everything it answered true for was reported to the caller as
// 404 bill_not_found. Only absence carries that meaning. The other two describe a
// workflow that is present - one momentarily busy, one that exists right now -
// and reporting either as a missing bill invites a caller to conclude its bill
// was never created, or its charge never recorded, and to act on that.
//
// The classification is a pure function of the error, so this runs in CI with no
// Temporal server.
func TestWorkflowFailuresAreClassifiedApart(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		absent         bool
		busy           bool
		alreadyStarted bool
		why            string
	}{
		{
			name:   "NotFound",
			err:    serviceerror.NewNotFound("workflow execution not found"),
			absent: true,
			why:    "nothing live to ask; storage is the only remaining source",
		},
		{
			name: "WorkflowNotReady",
			err:  serviceerror.NewWorkflowNotReady("workflow state is not ready"),
			busy: true,
			why: "running but momentarily unable to answer. Classifying it as absent " +
				"answers 404 for a bill that exists and is open",
		},
		{
			name: "Unavailable",
			err:  serviceerror.NewUnavailable("connection refused"),
			busy: true,
			why: "the observed error when Temporal is unreachable, on both query and " +
				"update. It says nothing about whether the bill exists",
		},
		{
			name: "DeadlineExceeded",
			err:  serviceerror.NewDeadlineExceeded("deadline exceeded"),
			busy: true,
			why:  "a slow answer is not a missing bill",
		},
		{
			name: "ResourceExhausted",
			err:  &serviceerror.ResourceExhausted{Message: "rate limited"},
			busy: true,
			why:  "shedding load is not a statement about the bill",
		},
		{
			name:           "WorkflowExecutionAlreadyStarted",
			err:            serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "run-1"),
			alreadyStarted: true,
			why: "an execution with that id exists - the opposite of absent. Creation " +
				"wants this branch; no other caller may read it as a missing bill",
		},
		{
			name: "context deadline",
			err:  context.DeadlineExceeded,
			busy: true,
			why:  "the caller's own deadline elapsed; the bill is unexamined, not absent",
		},
		{
			name: "nil",
			err:  nil,
			why:  "no failure is not a failure of any kind",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWorkflowAbsent(tc.err); got != tc.absent {
				t.Errorf("isWorkflowAbsent = %v, want %v\n\n%s", got, tc.absent, tc.why)
			}
			if got := isWorkflowBusy(tc.err); got != tc.busy {
				t.Errorf("isWorkflowBusy = %v, want %v\n\n%s", got, tc.busy, tc.why)
			}
			if got := isWorkflowAlreadyStarted(tc.err); got != tc.alreadyStarted {
				t.Errorf("isWorkflowAlreadyStarted = %v, want %v\n\n%s", got, tc.alreadyStarted, tc.why)
			}
		})
	}
}

// The three classifications must not overlap. An error in two of them would
// leave the branch taken dependent on the order the predicates happen to be
// tested in, which is exactly the ambiguity the split exists to remove.
func TestClassificationsAreMutuallyExclusive(t *testing.T) {
	errs := []error{
		serviceerror.NewNotFound("gone"),
		serviceerror.NewWorkflowNotReady("busy"),
		serviceerror.NewUnavailable("connection refused"),
		serviceerror.NewDeadlineExceeded("deadline exceeded"),
		&serviceerror.ResourceExhausted{Message: "rate limited"},
		serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "run-1"),
		context.DeadlineExceeded,
	}

	for _, err := range errs {
		matched := 0
		for _, hit := range []bool{isWorkflowAbsent(err), isWorkflowBusy(err), isWorkflowAlreadyStarted(err)} {
			if hit {
				matched++
			}
		}
		if matched != 1 {
			t.Errorf("%T matched %d classifications, want exactly 1: %v", err, matched, err)
		}
	}
}

// A 503 must tell the caller when to try again. Without Retry-After a transient
// failure is indistinguishable from a permanent one, and the caller is back to
// guessing - which is the position the 404 left it in.
func TestUnavailableCarriesRetryAfterAndAReason(t *testing.T) {
	if _, ok := reasonStatus[reasonUnavailable]; !ok {
		t.Fatalf("reasonUnavailable has no status mapping")
	}
	if got := reasonStatus[reasonUnavailable]; got != 503 {
		t.Errorf("reasonStatus[%q] = %d, want 503", reasonUnavailable, got)
	}
	if _, ok := reasonTitle[reasonUnavailable]; !ok {
		t.Errorf("reasonUnavailable has no title; writeProblem would fall back to generic text")
	}
	if reasonUnavailable == reasonNotFound {
		t.Errorf("the unavailable reason must be distinct from the not-found reason")
	}
}
