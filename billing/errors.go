package billing

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"

	"fees-api/internal/billflow"
)

// problem is the error body every endpoint returns, modelled on RFC 9457.
//
// The reason field is the machine-readable part, and it is what makes the
// difference between 409 and 422 actionable rather than decorative: a client
// seeing bill_not_open knows the charge arrived too late and must not be retried
// as-is, while currency_mismatch says the request itself was malformed. Both are
// refusals, but they call for different responses from the caller.
type problem struct {
	Status int    `json:"status"`
	Reason string `json:"reason"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// reasonStatus maps each rejection reason to its status code.
//
// The split is deliberate and consistent across the whole API:
//
//	409 - the request contradicts the bill's state or an immutable fact about it
//	422 - the request is well-formed JSON but cannot be acted on as stated
//	404 - no such bill
//	500 - a fault on our side
var reasonStatus = map[string]int{
	billflow.ReasonBillNotOpen:         http.StatusConflict,
	billflow.ReasonLineItemConflict:    http.StatusConflict,
	billflow.ReasonCurrencyMismatch:    http.StatusUnprocessableEntity,
	billflow.ReasonInvalidLineItem:     http.StatusUnprocessableEntity,
	billflow.ReasonInvalidPeriod:       http.StatusUnprocessableEntity,
	billflow.ReasonUnsupportedCurrency: http.StatusUnprocessableEntity,
	billflow.ReasonInvalidBill:         http.StatusUnprocessableEntity,
	billflow.ReasonInternal:            http.StatusInternalServerError,
}

var reasonTitle = map[string]string{
	billflow.ReasonBillNotOpen:         "Bill is not open",
	billflow.ReasonLineItemConflict:    "Line item id already used with different detail",
	billflow.ReasonCurrencyMismatch:    "Line item currency does not match the bill",
	billflow.ReasonInvalidLineItem:     "Line item is missing required detail",
	billflow.ReasonInvalidPeriod:       "Fee period must end after it begins",
	billflow.ReasonUnsupportedCurrency: "Unsupported currency",
	billflow.ReasonInvalidBill:         "Bill is missing required detail",
	billflow.ReasonInternal:            "Internal error",
}

// reasonNotFound is used when no bill exists, in the workflow or in storage.
const reasonNotFound = "bill_not_found"

// writeJSON writes a success response.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// writeProblem writes an error response with an explicit reason.
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

// writeNotFound reports a bill that exists in neither the workflow nor storage.
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

// writeRejection translates an error returned by a Temporal update into an HTTP
// response.
//
// A rejected update arrives as an application error carrying the reason the
// workflow's validator assigned it. Anything else - a transport failure, a
// workflow that no longer exists - is not a business refusal and is reported as
// such rather than being flattened into a 400.
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

// isWorkflowGone reports whether err means the workflow is no longer available
// to serve an update or query - because it has completed and aged out, or never
// existed. It is the signal to fall back to durable storage.
func isWorkflowGone(err error) bool {
	if err == nil {
		return false
	}
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var alreadyCompleted *serviceerror.WorkflowNotReady
	if errors.As(err, &alreadyCompleted) {
		return true
	}
	var execAlreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	return errors.As(err, &execAlreadyStarted)
}
