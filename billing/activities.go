package billing

import (
	"context"

	"go.temporal.io/sdk/activity"

	"fees-api/internal/bill"
	"fees-api/internal/billflow"
)

// Activities implements the side effects the bill workflow needs.
//
// They live here, in the Encore service, rather than alongside the workflow,
// because they are the only part of the system that touches infrastructure. The
// workflow itself stays free of Encore so its lifecycle can be tested without it.
type Activities struct{}

// PersistInvoice writes the frozen invoice to durable storage.
//
// Activities run at least once, not exactly once: a worker can complete the write
// and then crash before recording that it did, and Temporal will retry. The write
// is therefore idempotent, so a retry converges on one invoice row and one row per
// line item rather than duplicating a charge.
func (a *Activities) PersistInvoice(ctx context.Context, snap bill.Snapshot) error {
	logger := activity.GetLogger(ctx)
	logger.Info("persisting invoice",
		"billID", snap.ID, "state", snap.State,
		"total", snap.Total.String(), "lineItems", len(snap.LineItems))

	if err := saveInvoice(ctx, snap); err != nil {
		logger.Error("persisting invoice failed", "billID", snap.ID, "error", err)
		return err
	}
	return nil
}

// EmitInvoice hands the finished invoice to the payee.
//
// The implementation is out of scope for this service - producing and delivering
// the invoice document belongs elsewhere. What is in scope is the seam and its
// failure behaviour: this is an activity with a bounded retry policy, so a
// transient downstream failure does not lose the close. While it is failing the
// bill stays visibly in CLOSING with its totals already frozen, which is the
// honest description of that state, and the reason CLOSING exists at all.
//
// A real implementation would render the invoice and dispatch it, and would keep
// the same shape: idempotent, retryable, and unable to alter what was charged.
func (a *Activities) EmitInvoice(ctx context.Context, snap bill.Snapshot) error {
	logger := activity.GetLogger(ctx)
	logger.Info("emitting invoice",
		"billID", snap.ID,
		"total", snap.Total.String(),
		"lineItems", len(snap.LineItems),
		"closedBy", snap.ClosedBy,
		"note", "delivery is out of scope; this records the hand-off")
	return nil
}

// FinalizeInvoice records that the hand-off completed and the bill is done.
//
// Separate from PersistInvoice because the two straddle the hand-off: the invoice
// is recorded before it is attempted, so a hand-off that keeps failing still
// leaves a durable record of what was charged, and the state advances to CLOSED
// only once the hand-off has actually succeeded.
func (a *Activities) FinalizeInvoice(ctx context.Context, in billflow.FinalizeInvoiceInput) error {
	activity.GetLogger(ctx).Info("finalizing invoice", "billID", in.BillID, "state", in.State)
	return setInvoiceState(ctx, in.BillID, in.State)
}
