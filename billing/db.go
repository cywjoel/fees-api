package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"encore.dev/storage/sqldb"

	"fees-api/internal/bill"
	"fees-api/internal/money"
)

// billsDB holds closed invoices. It is written only by workflow activities: the
// API layer reads from it but never writes, which keeps the workflow the single
// writer for a bill and avoids a dual-write between storage and workflow state.
var billsDB = sqldb.NewDatabase("bills", sqldb.DatabaseConfig{
	Migrations: "./migrations",
})

// ErrInvoiceNotFound is returned when no invoice has been persisted for a bill.
var ErrInvoiceNotFound = errors.New("billing: invoice not found")

// storageTimePrecision is the resolution a TIMESTAMPTZ column keeps.
//
// Go's time.Time carries nanoseconds and PostgreSQL's does not, so an instant
// that travels through storage comes back truncated. Comparing a truncated
// instant against the untruncated original finds them different, which is how a
// correct retry came to be refused as idempotency-key reuse once a bill's
// workflow had aged out and storage became the only source for its fee period.
const storageTimePrecision = time.Microsecond

// atStoragePrecision truncates an instant to the resolution it will survive at.
//
// Applied to a fee period on the way in, so that what a bill reports is what can
// actually be recorded, and applied again wherever two instants are compared, so
// that a bill created before that was true still matches a correct retry.
func atStoragePrecision(t time.Time) time.Time {
	return t.Truncate(storageTimePrecision)
}

// saveInvoice writes a frozen invoice and its line items.
//
// The write is idempotent because the activity that calls it may be retried:
// Temporal guarantees an activity runs at least once, not exactly once. Re-running
// it must leave one invoice row and one row per line item, never duplicates.
//
// Line items are immutable once accrued, so a conflicting item id is left alone
// rather than overwritten. The invoice row itself is upserted, because a retry
// may be carrying a later state for the same frozen totals.
func saveInvoice(ctx context.Context, snap bill.Snapshot) error {
	if snap.ClosedAt == nil {
		return fmt.Errorf("billing: refusing to persist bill %q with no closure time", snap.ID)
	}
	if strings.TrimSpace(snap.CustomerID) == "" {
		// Checked here as well as by the column constraint, so the failure names
		// itself instead of arriving as a constraint violation from the driver.
		// Reachable only for a bill started before bills had customers.
		return fmt.Errorf("billing: refusing to persist bill %q with a blank customer; "+
			"an invoice has to be billable to someone", snap.ID)
	}

	tx, err := billsDB.Begin(ctx)
	if err != nil {
		return fmt.Errorf("billing: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO invoice (
			bill_id, customer_id, state, currency, period_start, period_end, created_at,
			total_minor_units, closed_at, closed_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (bill_id) DO UPDATE SET
			state             = EXCLUDED.state,
			total_minor_units = EXCLUDED.total_minor_units,
			closed_at         = EXCLUDED.closed_at,
			closed_by         = EXCLUDED.closed_by
	`,
		snap.ID, snap.CustomerID, string(snap.State), snap.Currency.Code,
		snap.PeriodStart, snap.PeriodEnd, snap.CreatedAt,
		snap.Total.MinorUnits(), *snap.ClosedAt, string(snap.ClosedBy),
	); err != nil {
		return fmt.Errorf("billing: saving invoice %q: %w", snap.ID, err)
	}

	for i, item := range snap.LineItems {
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_line_item (
				bill_id, item_id, amount_minor_units, currency, description, accrued_at, seq
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (bill_id, item_id) DO NOTHING
		`,
			snap.ID, item.ID, item.Amount.MinorUnits(), item.Amount.Currency().Code,
			item.Description, item.AccruedAt, i,
		); err != nil {
			return fmt.Errorf("billing: saving line item %q of invoice %q: %w", item.ID, snap.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("billing: committing invoice %q: %w", snap.ID, err)
	}
	return nil
}

// setInvoiceState records a bill's terminal state.
//
// It is separate from saveInvoice because the two happen either side of the
// invoice hand-off: the invoice is recorded before the hand-off is attempted, so
// a hand-off that keeps failing still leaves a durable record of what was
// charged, and the state is advanced to CLOSED only once the hand-off succeeds.
// Until then storage says CLOSING, which is the truth.
func setInvoiceState(ctx context.Context, billID string, state bill.State) error {
	if _, err := billsDB.Exec(ctx, `
		UPDATE invoice SET state = $2 WHERE bill_id = $1
	`, billID, string(state)); err != nil {
		return fmt.Errorf("billing: setting state of invoice %q: %w", billID, err)
	}
	return nil
}

// loadInvoice reads a persisted invoice.
func loadInvoice(ctx context.Context, billID string) (bill.Snapshot, error) {
	var (
		snap        bill.Snapshot
		state       string
		currencyStr string
		totalMinor  int64
		closedAt    = new(time.Time)
		closedBy    string
	)

	row := billsDB.QueryRow(ctx, `
		SELECT customer_id, state, currency, period_start, period_end, created_at,
		       total_minor_units, closed_at, closed_by
		FROM invoice WHERE bill_id = $1
	`, billID)

	if err := row.Scan(&snap.CustomerID, &state, &currencyStr, &snap.PeriodStart, &snap.PeriodEnd,
		&snap.CreatedAt, &totalMinor, closedAt, &closedBy); err != nil {
		if errors.Is(err, sqldb.ErrNoRows) {
			return bill.Snapshot{}, fmt.Errorf("%w: %q", ErrInvoiceNotFound, billID)
		}
		return bill.Snapshot{}, fmt.Errorf("billing: loading invoice %q: %w", billID, err)
	}

	currency, err := money.Lookup(currencyStr)
	if err != nil {
		return bill.Snapshot{}, fmt.Errorf("billing: invoice %q has %w", billID, err)
	}

	snap.ID = billID
	snap.State = bill.State(state)
	snap.Currency = currency
	snap.Total = money.New(totalMinor, currency)
	snap.ClosedAt = closedAt
	snap.ClosedBy = bill.Trigger(closedBy)

	rows, err := billsDB.Query(ctx, `
		SELECT item_id, amount_minor_units, currency, description, accrued_at
		FROM invoice_line_item WHERE bill_id = $1 ORDER BY seq
	`, billID)
	if err != nil {
		return bill.Snapshot{}, fmt.Errorf("billing: loading line items of invoice %q: %w", billID, err)
	}
	defer rows.Close()

	snap.LineItems = []bill.LineItem{}
	for rows.Next() {
		var (
			item     bill.LineItem
			minor    int64
			itemCurr string
		)
		if err := rows.Scan(&item.ID, &minor, &itemCurr, &item.Description, &item.AccruedAt); err != nil {
			return bill.Snapshot{}, fmt.Errorf("billing: scanning line item of invoice %q: %w", billID, err)
		}
		c, err := money.Lookup(itemCurr)
		if err != nil {
			return bill.Snapshot{}, fmt.Errorf("billing: line item %q has %w", item.ID, err)
		}
		item.Amount = money.New(minor, c)
		snap.LineItems = append(snap.LineItems, item)
	}
	if err := rows.Err(); err != nil {
		return bill.Snapshot{}, fmt.Errorf("billing: reading line items of invoice %q: %w", billID, err)
	}

	return snap, nil
}
