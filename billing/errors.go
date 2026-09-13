package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"

	"fees-api/internal/billflow"
)

// problem is the error body every endpoint returns, modelled on RFC 9457. The
// reason field is what makes the difference between 409 and 422 actionable: a
// client seeing bill_not_open knows the charge arrived too late and must not be
// retried as-is, while currency_mismatch says the request itself was malformed.
type problem struct {
	Status int    `json:"status"`
	Reason string `json:"reason"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// reasonStatus maps each rejection reason to its status code:
//
//	409 - the request contradicts the bill's state or an immutable fact about it
//	422 - the request is well-formed JSON but cannot be acted on as stated
//	404 - no such bill
//	500 - a fault on our side
var reasonStatus = map[string]int{
	billflow.ReasonBillNotOpen:         http.StatusConflict,
	billflow.ReasonLineItemConflict:    http.StatusConflict,
	billflow.ReasonCurrencyMismatch:    http.StatusUnprocessableEntity,
	billflow.ReasonCustomerMismatch:    http.StatusUnprocessableEntity,
	billflow.ReasonInvalidLineItem:     http.StatusUnprocessableEntity,
	billflow.ReasonInvalidPeriod:       http.StatusUnprocessableEntity,
	billflow.ReasonUnsupportedCurrency: http.StatusUnprocessableEntity,
	billflow.ReasonInvalidBill:         http.StatusUnprocessableEntity,
	billflow.ReasonInternal:            http.StatusInternalServerError,
	reasonUnavailable:                  http.StatusServiceUnavailable,
	reasonKeyReuse:                     http.StatusConflict,
}

var reasonTitle = map[string]string{
	billflow.ReasonBillNotOpen:         "Bill is not open",
	billflow.ReasonLineItemConflict:    "Line item id already used with different detail",
	billflow.ReasonCurrencyMismatch:    "Line item currency does not match the bill",
	billflow.ReasonCustomerMismatch:    "Line item customer does not match the bill",
	billflow.ReasonInvalidLineItem:     "Line item is missing required detail",
	billflow.ReasonInvalidPeriod:       "Fee period must end after it begins",
	billflow.ReasonUnsupportedCurrency: "Unsupported currency",
	billflow.ReasonInvalidBill:         "Bill is missing required detail",
	billflow.ReasonInternal:            "Internal error",
	reasonUnavailable:                  "Bill state is temporarily unavailable",
	reasonKeyReuse:                     "Idempotency key already used for a different bill",
}

const reasonNotFound = "bill_not_found"

// reasonKeyReuse is a contradiction, not a retry, so it is a 409 - consistent
// with reusing a line item id for a different charge.
const reasonKeyReuse = "idempotency_key_reuse"

// reasonUnavailable means a bill's state could not be determined, because the
// workflow holding it could not be reached in time.
//
// Deliberately distinct from reasonNotFound. "I cannot reach it" and "it does not
// exist" call for opposite responses: the first should be retried, the second
// must not be. Reporting the first as the second invites a client to conclude its
// bill was never created, and to act on that.
const reasonUnavailable = "temporarily_unavailable"

// retryAfter is advertised on a 503. The failures it covers clear in seconds
// rather than minutes, so a short interval is more useful than a conservative one.
const retryAfter = 2 * time.Second

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeProblem(w http.ResponseWriter, status int, reason, detail string) {
	title, ok := reasonTitle[reason]
	if !ok {
		title = http.StatusText(status)
	}
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Status: status,
		Reason: reason,
		Title:  title,
		Detail: detail,
	})
}

func writeNotFound(w http.ResponseWriter, billID string) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(problem{
		Status: http.StatusNotFound,
		Reason: reasonNotFound,
		Title:  "Bill not found",
		Detail: "no bill with id " + billID,
	})
}

// writeUnavailable reports that the bill could not be reached, and says when to
// try again.
func writeUnavailable(w http.ResponseWriter, detail string) {
	// The actionable half: without it a caller cannot tell a transient 503 from a
	// permanent one.
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	writeProblem(w, http.StatusServiceUnavailable, reasonUnavailable, detail)
}

// writeRejection translates an error returned by a Temporal update into an HTTP
// response.
//
// A rejected update arrives as an application error carrying the reason the
// validator assigned it; anything else is not a business refusal and is reported
// as such rather than flattened into a 400.
func writeRejection(w http.ResponseWriter, err error) {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		reason := appErr.Type()
		status, ok := reasonStatus[reason]
		if !ok {
			status, reason = http.StatusInternalServerError, billflow.ReasonInternal
		}
		writeProblem(w, status, reason, appErr.Message())
		return
	}
	writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal, err.Error())
}

// The three predicates below replace a single "is the workflow gone?", which
// answered true for three unrelated conditions and turned every one into a 404.
// Only one means the bill is absent; the other two describe a workflow that is
// very much present, and the caller must act differently on each:
//
//	absent         -> storage is the only source; 404 only if it has nothing
//	busy           -> 503 with Retry-After; the bill exists and will answer later
//	already started -> the execution exists right now; creation's "already exists"
//
// Keeping them apart is what stops a momentary hiccup being reported as a
// deleted bill.

// isWorkflowAbsent reports that the workflow is not there to be asked - it never
// existed, its history has aged out, or it has completed. In every case storage
// is the only remaining source of truth.
func isWorkflowAbsent(err error) bool {
	if err == nil {
		return false
	}
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}

// isWorkflowBusy reports a transient failure.
//
// None of these say anything about whether the bill exists, which is why they
// must not fall through to storage: an open bill has no persisted row until it
// closes, so falling back would answer "no such bill" for one that is running.
func isWorkflowBusy(err error) bool {
	if err == nil {
		return false
	}
	var (
		unavailable *serviceerror.Unavailable
		notReady    *serviceerror.WorkflowNotReady
		deadline    *serviceerror.DeadlineExceeded
		exhausted   *serviceerror.ResourceExhausted
	)
	switch {
	case errors.As(err, &unavailable),
		errors.As(err, &notReady),
		errors.As(err, &deadline),
		errors.As(err, &exhausted),
		errors.Is(err, context.DeadlineExceeded):
		return true
	default:
		return false
	}
}

// isWorkflowAlreadyStarted reports that a start request collided with an
// execution that already exists - the opposite of absence.
//
// Useful in exactly one place: bill creation, where it means the idempotency key
// has already produced a bill. It must never be read as the bill being missing.
func isWorkflowAlreadyStarted(err error) bool {
	if err == nil {
		return false
	}
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	return errors.As(err, &alreadyStarted)
}
