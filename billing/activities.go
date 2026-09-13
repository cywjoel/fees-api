package billing

import (
	"context"

	"go.temporal.io/sdk/activity"

	"fees-api/internal/bill"
	"fees-api/internal/billflow"
)

// Activities implements the side effects the bill workflow needs. They live in
// the Encore service rather than alongside the workflow because they are the
// only part of the system that touches infrastructure.
type Activities struct{}

// PersistInvoice writes the frozen invoice to durable storage.
//
// Activities run at least once, not exactly once: a worker can complete the write
// and crash before recording that it did, so saveInvoice is idempotent.
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
// Producing and delivering the document is out of scope; the seam and its failure
// behaviour are not. While this fails the bill stays visibly in CLOSING with its
// totals frozen, which is why CLOSING exists. A real implementation keeps the
// same shape: idempotent, retryable, unable to alter what was charged.
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
// is recorded before it is attempted, and only reaches CLOSED once it succeeds.
func (a *Activities) FinalizeInvoice(ctx context.Context, in billflow.FinalizeInvoiceInput) error {
	activity.GetLogger(ctx).Info("finalizing invoice", "billID", in.BillID, "state", in.State)
	return setInvoiceState(ctx, in.BillID, in.State)
}
