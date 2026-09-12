package billflow

import (
	"errors"

	"go.temporal.io/sdk/temporal"

	"fees-api/internal/bill"
	"fees-api/internal/money"
)

// Rejection reasons. These are the workflow's machine-readable vocabulary for
// refusing an update, and they cross the process boundary intact.
//
// A domain error's Go identity does not survive the trip from worker to API
// caller - errors.Is cannot match across a serialised Temporal failure - so each
// rejection carries one of these as its application-error type. The API layer
// maps them to status codes, which keeps the mapping in one place and stops the
// caller having to match on message text.
const (
	// ReasonBillNotOpen means a new charge was offered to a bill whose totals are
	// already frozen. A contradiction: 409.
	ReasonBillNotOpen = "bill_not_open"

	// ReasonLineItemConflict means an item id already on the bill was reused with
	// different detail. Also a contradiction: 409.
	ReasonLineItemConflict = "line_item_conflict"

	// ReasonCurrencyMismatch means the line item is denominated differently from
	// the bill. Malformed rather than contradictory: 422.
	ReasonCurrencyMismatch = "currency_mismatch"

	// ReasonInvalidLineItem means the line item is missing required detail: 422.
	ReasonInvalidLineItem = "invalid_line_item"

	// ReasonInvalidPeriod means the fee period does not end after it begins: 422.
	ReasonInvalidPeriod = "invalid_period"

	// ReasonUnsupportedCurrency means the currency is outside the supported set: 422.
	ReasonUnsupportedCurrency = "unsupported_currency"

	// ReasonInvalidBill means the bill is missing required detail: 422.
	ReasonInvalidBill = "invalid_bill"

	// ReasonInternal is the fallback for an error with no domain meaning: 500.
	ReasonInternal = "internal"
)

// ClassifyRejection maps a domain error to its machine-readable reason.
func ClassifyRejection(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, bill.ErrNotOpen):
		return ReasonBillNotOpen
	case errors.Is(err, bill.ErrLineItemConflict):
		return ReasonLineItemConflict
	case errors.Is(err, money.ErrCurrencyMismatch):
		return ReasonCurrencyMismatch
	case errors.Is(err, money.ErrUnsupportedCurrency):
		return ReasonUnsupportedCurrency
	case errors.Is(err, money.ErrPrecisionExceeded):
		return ReasonInvalidLineItem
	case errors.Is(err, bill.ErrInvalidLineItem):
		return ReasonInvalidLineItem
	case errors.Is(err, bill.ErrInvalidPeriod):
		return ReasonInvalidPeriod
	case errors.Is(err, bill.ErrInvalidBill):
		return ReasonInvalidBill
	default:
		return ReasonInternal
	}
}

// rejection wraps a domain error so its reason survives serialisation to the
// caller. Rejections are never retryable: the caller must change the request,
// not repeat it.
func rejection(err error) error {
	if err == nil {
		return nil
	}
	return temporal.NewNonRetryableApplicationError(err.Error(), ClassifyRejection(err), err)
}
