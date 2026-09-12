package billing

import (
	"testing"

	"go.temporal.io/api/serviceerror"
)

// TestIsWorkflowGoneDistinguishesAbsentFromUnavailable pins the distinction the
// API's error handling rests on.
//
// isWorkflowGone decides whether a failed Temporal call means the bill does not
// exist. Everything it returns true for is reported to the caller as
// 404 bill_not_found (see writeUpdateFailure), and readBill treats it as licence
// to fall back to storage. Only one of the errors below actually carries that
// meaning; the other two describe a workflow that is very much present.
//
// This test needs no Temporal server: the classification is a pure function of
// the error, so it runs in CI alongside the rest of the package.
func TestIsWorkflowGoneDistinguishesAbsentFromUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{
			name: "NotFound",
			err:  serviceerror.NewNotFound("workflow execution not found"),
			want: true,
			why:  "the execution is genuinely absent; 404 is the right answer",
		},
		{
			name: "WorkflowNotReady",
			err:  serviceerror.NewWorkflowNotReady("workflow state is not ready"),
			want: false,
			why: "the workflow is running but momentarily unable to serve the request. " +
				"It is a retry-later condition, not a missing bill. Classifying it as gone " +
				"answers 404 for a bill that exists and is open, and a caller that believes " +
				"the 404 may create a duplicate bill",
		},
		{
			name: "WorkflowExecutionAlreadyStarted",
			err:  serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "run-1"),
			want: false,
			why: "this error means an execution with that id already exists - the opposite " +
				"of absent. CreateBill happens to want the same branch for it, but the two " +
				"meanings must not share one predicate",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWorkflowGone(tc.err); got != tc.want {
				t.Errorf("isWorkflowGone(%s) = %v, want %v\n\n%s", tc.name, got, tc.want, tc.why)
			}
		})
	}
}

// TestWorkflowGoneIsNotTheOnlyFailureMode records the gap that lets a Temporal
// outage read as a missing bill.
//
// readBill discards the error from QueryWorkflow entirely and falls back to
// storage no matter what went wrong. An open bill has no persisted row until it
// closes, so the fallback returns ErrInvoiceNotFound and GetBill answers 404 -
// reporting every open bill in the system as deleted for the duration of the
// outage.
//
// The predicate is the place to start: a transport failure is neither "gone" nor
// "found", and until isWorkflowGone stops being the only question asked, the
// caller cannot tell a missing bill from an unreachable one.
func TestWorkflowGoneIsNotTheOnlyFailureMode(t *testing.T) {
	transient := serviceerror.NewUnavailable("connection error: dial tcp 127.0.0.1:7233: connect: connection refused")

	if isWorkflowGone(transient) {
		t.Errorf("isWorkflowGone(Unavailable) = true, want false\n\n" +
			"Temporal being unreachable does not mean the bill is absent.")
	}

	// Guard the other half: this error must be routed somewhere that is neither a
	// 404 nor a silent fall-through to storage. There is no such branch today.
	t.Log("note: not being classified as gone is necessary but not sufficient - " +
		"readBill still falls back to storage on any query error, so an open bill " +
		"reads as 404 during an outage until that fallback is narrowed to NotFound.")
}
