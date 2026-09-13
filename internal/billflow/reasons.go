package billflow

import (
	"errors"

	"go.temporal.io/sdk/temporal"

	"fees-api/internal/bill"
	"fees-api/internal/money"
)

// Rejection reasons are the workflow's machine-readable vocabulary for refusing
// an update. A domain error's Go identity does not survive the trip from worker
// to API caller - errors.Is cannot match across a serialised Temporal failure -
// so each rejection carries one of these as its application-error type. The API
// layer maps them to status codes in one place; see reasonStatus in the billing
// package.
const (
	ReasonBillNotOpen         = "bill_not_open"
	ReasonLineItemConflict    = "line_item_conflict"
	ReasonCurrencyMismatch    = "currency_mismatch"
	ReasonCustomerMismatch    = "customer_mismatch"
	ReasonInvalidLineItem     = "invalid_line_item"
	ReasonInvalidPeriod       = "invalid_period"
	ReasonUnsupportedCurrency = "unsupported_currency"
	ReasonInvalidBill         = "invalid_bill"
	ReasonInternal            = "internal"
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
	case errors.Is(err, bill.ErrCustomerMismatch):
		return ReasonCustomerMismatch
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
