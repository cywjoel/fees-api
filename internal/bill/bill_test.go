package bill_test

import (
	"errors"
	"testing"
	"time"

	"fees-api/internal/bill"
	"fees-api/internal/money"
)

var (
	usd = money.MustLookup("USD")
	gel = money.MustLookup("GEL")

	periodStart = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

func newOpenBill(t *testing.T) *bill.Bill {
	t.Helper()
	b, err := bill.New("bill_test", usd, periodStart, periodEnd, periodStart)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	return b
}

func item(id string, minorUnits int64, c money.Currency, desc string) bill.LineItem {
	return bill.LineItem{
		ID:          id,
		Amount:      money.New(minorUnits, c),
		Description: desc,
		AccruedAt:   periodStart.Add(time.Hour),
	}
}

// Spec: billing/bill-lifecycle - "Bill is created successfully".
func TestNewBillOpensEmptyAndZeroed(t *testing.T) {
	b := newOpenBill(t)

	if got := b.State(); got != bill.StateOpen {
		t.Errorf("state = %s, want OPEN", got)
	}
	if !b.Total().IsZero() {
		t.Errorf("total = %s, want zero", b.Total())
	}
	if got := b.Total().Currency().Code; got != "USD" {
		t.Errorf("total currency = %q, want USD", got)
	}
	if got := len(b.LineItems()); got != 0 {
		t.Errorf("line items = %d, want 0", got)
	}
}

// Spec: billing/bill-lifecycle - "Period end is not after period start".
func TestNewBillRejectsInvalidPeriod(t *testing.T) {
	tests := []struct {
		name       string
		start, end time.Time
	}{
		{name: "end equals start", start: periodStart, end: periodStart},
		{name: "end before start", start: periodEnd, end: periodStart},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bill.New("b", usd, tc.start, tc.end, tc.start); !errors.Is(err, bill.ErrInvalidPeriod) {
				t.Fatalf("New error = %v, want ErrInvalidPeriod", err)
			}
		})
	}
}

func TestNewBillRequiresIDAndCurrency(t *testing.T) {
	if _, err := bill.New("", usd, periodStart, periodEnd, periodStart); !errors.Is(err, bill.ErrInvalidBill) {
		t.Errorf("New with empty id error = %v, want ErrInvalidBill", err)
	}
	if _, err := bill.New("b", money.Currency{}, periodStart, periodEnd, periodStart); !errors.Is(err, bill.ErrInvalidBill) {
		t.Errorf("New with zero currency error = %v, want ErrInvalidBill", err)
	}
}

// Spec: billing/line-item-accrual - "Adding a line item reports the running
// total".
func TestAddLineItemAccumulatesRunningTotal(t *testing.T) {
	b := newOpenBill(t)

	first, err := b.AddLineItem(item("txn_1", 1255, usd, "card fee"))
	if err != nil {
		t.Fatalf("AddLineItem returned error: %v", err)
	}
	if first.Outcome != bill.OutcomeCreated {
		t.Errorf("first outcome = %s, want created", first.Outcome)
	}
	if got, want := first.RunningTotal.MinorUnits(), int64(1255); got != want {
		t.Errorf("running total after first = %d, want %d", got, want)
	}

	second, err := b.AddLineItem(item("txn_2", 500, usd, "transfer fee"))
	if err != nil {
		t.Fatalf("AddLineItem returned error: %v", err)
	}
	if got, want := second.RunningTotal.MinorUnits(), int64(1755); got != want {
		t.Errorf("running total after second = %d, want %d", got, want)
	}
	if got, want := b.Total().String(), "17.55 USD"; got != want {
		t.Errorf("total = %q, want %q", got, want)
	}
}

// Spec: billing/line-item-accrual - "Retried addition does not duplicate a
// charge" and "Repeated retries remain safe".
func TestAddLineItemIsIdempotentByIdentifier(t *testing.T) {
	b := newOpenBill(t)
	in := item("txn_1", 500, usd, "card fee")

	first, err := b.AddLineItem(in)
	if err != nil {
		t.Fatalf("first AddLineItem returned error: %v", err)
	}
	if first.Outcome != bill.OutcomeCreated {
		t.Fatalf("first outcome = %s, want created", first.Outcome)
	}

	for i := 0; i < 5; i++ {
		retry, err := b.AddLineItem(in)
		if err != nil {
			t.Fatalf("retry %d returned error: %v", i, err)
		}
		if retry.Outcome != bill.OutcomeAlreadyAccrued {
			t.Errorf("retry %d outcome = %s, want already_accrued", i, retry.Outcome)
		}
		if retry.LineItem.ID != in.ID {
			t.Errorf("retry %d returned item %q, want %q", i, retry.LineItem.ID, in.ID)
		}
	}

	if got, want := len(b.LineItems()), 1; got != want {
		t.Errorf("line items = %d, want %d", got, want)
	}
	if got, want := b.Total().MinorUnits(), int64(500); got != want {
		t.Errorf("total = %d minor units, want %d: a retry must not double-charge", got, want)
	}
}

// A retry carries a later timestamp than the original. That difference must not
// be mistaken for a conflicting charge, and the stored item keeps its original
// accrual time.
func TestRetryWithLaterTimestampIsStillTheSameCharge(t *testing.T) {
	b := newOpenBill(t)
	original := item("txn_1", 500, usd, "card fee")
	if _, err := b.AddLineItem(original); err != nil {
		t.Fatalf("AddLineItem returned error: %v", err)
	}

	retry := original
	retry.AccruedAt = original.AccruedAt.Add(30 * time.Second)

	got, err := b.AddLineItem(retry)
	if err != nil {
		t.Fatalf("retry returned error: %v", err)
	}
	if got.Outcome != bill.OutcomeAlreadyAccrued {
		t.Errorf("outcome = %s, want already_accrued", got.Outcome)
	}
	if !got.LineItem.AccruedAt.Equal(original.AccruedAt) {
		t.Errorf("accrued at = %s, want the original %s", got.LineItem.AccruedAt, original.AccruedAt)
	}
}

// Spec: billing/line-item-accrual - "Accrued line items are immutable".
func TestAddLineItemRejectsConflictingDetail(t *testing.T) {
	tests := []struct {
		name     string
		conflict bill.LineItem
	}{
		{name: "different amount", conflict: item("txn_1", 999, usd, "card fee")},
		{name: "different description", conflict: item("txn_1", 500, usd, "transfer fee")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newOpenBill(t)
			if _, err := b.AddLineItem(item("txn_1", 500, usd, "card fee")); err != nil {
				t.Fatalf("AddLineItem returned error: %v", err)
			}

			if _, err := b.AddLineItem(tc.conflict); !errors.Is(err, bill.ErrLineItemConflict) {
				t.Fatalf("conflicting add error = %v, want ErrLineItemConflict", err)
			}
			if got, want := b.Total().MinorUnits(), int64(500); got != want {
				t.Errorf("total = %d, want %d: a rejected add must not change the total", got, want)
			}
			items := b.LineItems()
			if len(items) != 1 {
				t.Fatalf("line items = %d, want 1", len(items))
			}
			if got, want := items[0].Amount.MinorUnits(), int64(500); got != want {
				t.Errorf("stored amount = %d, want %d: the stored item must not be overwritten", got, want)
			}
		})
	}
}

// Spec: billing/money-representation - "Line item in a different currency is
// rejected".
func TestAddLineItemRejectsCurrencyMismatch(t *testing.T) {
	b := newOpenBill(t)

	_, err := b.AddLineItem(item("txn_1", 500, gel, "card fee"))
	if !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("GEL item on a USD bill error = %v, want ErrCurrencyMismatch", err)
	}
	if !b.Total().IsZero() {
		t.Errorf("total = %s, want zero after a rejected add", b.Total())
	}
	if got := len(b.LineItems()); got != 0 {
		t.Errorf("line items = %d, want 0", got)
	}
}

// Spec: billing/line-item-accrual - "Line item is missing required detail".
func TestAddLineItemRejectsMissingDetail(t *testing.T) {
	tests := []struct {
		name string
		in   bill.LineItem
	}{
		{name: "no id", in: bill.LineItem{Amount: money.New(1, usd), Description: "fee"}},
		{name: "no currency", in: bill.LineItem{ID: "txn_1", Description: "fee"}},
		{name: "no description", in: bill.LineItem{ID: "txn_1", Amount: money.New(1, usd)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newOpenBill(t)
			if _, err := b.AddLineItem(tc.in); !errors.Is(err, bill.ErrInvalidLineItem) {
				t.Fatalf("error = %v, want ErrInvalidLineItem", err)
			}
		})
	}
}

// Spec: billing/line-item-accrual - "Addition to a closed bill is rejected" and
// "Addition to a closing bill is rejected".
func TestAddLineItemRejectedOnceBillLeavesOpen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		advance func(t *testing.T, b *bill.Bill)
	}{
		{
			name: "closing",
			advance: func(t *testing.T, b *bill.Bill) {
				if _, err := b.Close(bill.TriggerAPIRequest, periodEnd); err != nil {
					t.Fatalf("Close returned error: %v", err)
				}
			},
		},
		{
			name: "closed",
			advance: func(t *testing.T, b *bill.Bill) {
				if _, err := b.Close(bill.TriggerAPIRequest, periodEnd); err != nil {
					t.Fatalf("Close returned error: %v", err)
				}
				if err := b.MarkInvoiced(); err != nil {
					t.Fatalf("MarkInvoiced returned error: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newOpenBill(t)
			if _, err := b.AddLineItem(item("txn_1", 500, usd, "card fee")); err != nil {
				t.Fatalf("AddLineItem returned error: %v", err)
			}
			tc.advance(t, b)

			_, err := b.AddLineItem(item("txn_2", 700, usd, "late fee"))
			if !errors.Is(err, bill.ErrNotOpen) {
				t.Fatalf("add to a %s bill error = %v, want ErrNotOpen", tc.name, err)
			}
			if got, want := b.Total().MinorUnits(), int64(500); got != want {
				t.Errorf("total = %d, want %d: a rejected add must not change a frozen total", got, want)
			}
			if got, want := len(b.LineItems()), 1; got != want {
				t.Errorf("line items = %d, want %d", got, want)
			}
		})
	}
}

// Spec: billing/line-item-accrual - "Retry of an accrued item is honoured after
// the bill closes".
//
// Deduplication precedes the state check. The charge is genuinely on the
// invoice, so reporting it as present is truthful; answering "conflict" would
// tell the client its charge failed and invite a compensating re-charge for
// money already billed.
func TestRetryOfAccruedItemIsHonouredAfterClose(t *testing.T) {
	b := newOpenBill(t)
	in := item("txn_1", 500, usd, "card fee")
	if _, err := b.AddLineItem(in); err != nil {
		t.Fatalf("AddLineItem returned error: %v", err)
	}
	if _, err := b.Close(bill.TriggerPeriodEnd, periodEnd); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	got, err := b.AddLineItem(in)
	if err != nil {
		t.Fatalf("retry after close returned error: %v, want the accrued item", err)
	}
	if got.Outcome != bill.OutcomeAlreadyAccrued {
		t.Errorf("outcome = %s, want already_accrued", got.Outcome)
	}
	if got, want := b.Total().MinorUnits(), int64(500); got != want {
		t.Errorf("total = %d, want %d: the frozen total must not change", got, want)
	}
}

// Spec: billing/line-item-accrual - "Simultaneous additions are all retained".
// The workflow serialises writes, so the domain sees them in some order; what
// matters is that every distinct charge survives and the total is their exact sum.
func TestEveryDistinctLineItemIsRetained(t *testing.T) {
	b := newOpenBill(t)

	const n = 250
	for i := 0; i < n; i++ {
		id := "txn_" + time.Duration(i).String()
		if _, err := b.AddLineItem(item(id, 10, usd, "fee")); err != nil {
			t.Fatalf("AddLineItem %d returned error: %v", i, err)
		}
	}

	if got := len(b.LineItems()); got != n {
		t.Errorf("line items = %d, want %d", got, n)
	}
	if got, want := b.Total().MinorUnits(), int64(n*10); got != want {
		t.Errorf("total = %d minor units, want %d", got, want)
	}
}

// Line items list in accrual order. Map iteration in Go is randomised, so an
// implementation that listed straight from the map would vary between runs -
// inside a workflow that non-determinism would break history replay.
func TestLineItemsListInAccrualOrder(t *testing.T) {
	b := newOpenBill(t)
	ids := []string{"txn_c", "txn_a", "txn_b", "txn_z", "txn_m"}
	for _, id := range ids {
		if _, err := b.AddLineItem(item(id, 10, usd, "fee")); err != nil {
			t.Fatalf("AddLineItem returned error: %v", err)
		}
	}

	for run := 0; run < 20; run++ {
		got := b.LineItems()
		for i, want := range ids {
			if got[i].ID != want {
				t.Fatalf("run %d: line item %d = %q, want %q (accrual order must be stable)", run, i, got[i].ID, want)
			}
		}
	}
}

// Spec: billing/bill-lifecycle - "Close a bill on request" and "Totals are
// frozen on entry to CLOSING".
func TestCloseFreezesTotalAndRecordsTrigger(t *testing.T) {
	b := newOpenBill(t)
	for i, amt := range []int64{1000, 200, 55} {
		if _, err := b.AddLineItem(item("txn_"+string(rune('a'+i)), amt, usd, "fee")); err != nil {
			t.Fatalf("AddLineItem returned error: %v", err)
		}
	}

	closedAt := periodStart.Add(12 * time.Hour)
	snap, err := b.Close(bill.TriggerAPIRequest, closedAt)
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if snap.State != bill.StateClosing {
		t.Errorf("state = %s, want CLOSING", snap.State)
	}
	if snap.ClosedBy != bill.TriggerAPIRequest {
		t.Errorf("closedBy = %s, want api_request", snap.ClosedBy)
	}
	if snap.ClosedAt == nil || !snap.ClosedAt.Equal(closedAt) {
		t.Errorf("closedAt = %v, want %s", snap.ClosedAt, closedAt)
	}
	if got, want := snap.Total.MinorUnits(), int64(1255); got != want {
		t.Errorf("total = %d minor units, want %d", got, want)
	}
	if got, want := len(snap.LineItems), 3; got != want {
		t.Errorf("line items = %d, want %d", got, want)
	}
}

// Spec: billing/bill-lifecycle - "Bill with no line items closes with a zero
// total".
func TestCloseWithNoLineItemsYieldsZeroTotal(t *testing.T) {
	b := newOpenBill(t)

	snap, err := b.Close(bill.TriggerPeriodEnd, periodEnd)
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if !snap.Total.IsZero() {
		t.Errorf("total = %s, want zero", snap.Total)
	}
	if got := len(snap.LineItems); got != 0 {
		t.Errorf("line items = %d, want 0", got)
	}
	if snap.Total.Currency().Code != "USD" {
		t.Errorf("total currency = %q, want USD", snap.Total.Currency().Code)
	}
}

// Spec: billing/bill-lifecycle - "Closing an already-closing or closed bill is
// not an error" and "Close request against a bill already closed by the
// deadline".
func TestCloseIsIdempotentAndReportsTheOriginalTrigger(t *testing.T) {
	b := newOpenBill(t)
	if _, err := b.AddLineItem(item("txn_1", 500, usd, "fee")); err != nil {
		t.Fatalf("AddLineItem returned error: %v", err)
	}

	deadlineClose := periodEnd
	first, err := b.Close(bill.TriggerPeriodEnd, deadlineClose)
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// A close request arriving after the deadline already closed the bill.
	second, err := b.Close(bill.TriggerAPIRequest, deadlineClose.Add(time.Millisecond))
	if err != nil {
		t.Fatalf("second Close returned error: %v, want the existing snapshot", err)
	}

	if second.ClosedBy != bill.TriggerPeriodEnd {
		t.Errorf("closedBy = %s, want period_end: the later request did not cause the close", second.ClosedBy)
	}
	if second.ClosedAt == nil || !second.ClosedAt.Equal(deadlineClose) {
		t.Errorf("closedAt = %v, want the original %s", second.ClosedAt, deadlineClose)
	}
	if !second.Total.Equal(first.Total) {
		t.Errorf("total = %s, want the frozen %s", second.Total, first.Total)
	}
}

// Spec: billing/bill-lifecycle - "Bill reaches CLOSED after the invoice hand-off
// succeeds".
func TestMarkInvoicedCompletesTheBill(t *testing.T) {
	b := newOpenBill(t)
	if _, err := b.Close(bill.TriggerAPIRequest, periodEnd); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := b.MarkInvoiced(); err != nil {
		t.Fatalf("MarkInvoiced returned error: %v", err)
	}
	if got := b.State(); got != bill.StateClosed {
		t.Errorf("state = %s, want CLOSED", got)
	}
	// Repeating the hand-off is safe: the activity that drives it may be retried.
	if err := b.MarkInvoiced(); err != nil {
		t.Errorf("second MarkInvoiced returned error: %v, want a no-op", err)
	}
}

func TestMarkInvoicedRejectsAnOpenBill(t *testing.T) {
	b := newOpenBill(t)
	if err := b.MarkInvoiced(); !errors.Is(err, bill.ErrInvalidTransition) {
		t.Fatalf("MarkInvoiced on an open bill error = %v, want ErrInvalidTransition", err)
	}
}

// Spec: billing/bill-lifecycle - "Totals are frozen on entry to CLOSING".
//
// A caller holding a snapshot must not have it altered underneath them, so the
// snapshot's line items are a copy rather than a view onto the bill's own slice.
func TestSnapshotIsUnaffectedByLaterMutation(t *testing.T) {
	b := newOpenBill(t)
	if _, err := b.AddLineItem(item("txn_1", 500, usd, "fee")); err != nil {
		t.Fatalf("AddLineItem returned error: %v", err)
	}

	snap, err := b.Close(bill.TriggerAPIRequest, periodEnd)
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// Every mutation attempt available after a close.
	_, _ = b.AddLineItem(item("txn_2", 9999, usd, "late fee"))
	_, _ = b.Close(bill.TriggerPeriodEnd, periodEnd.Add(time.Hour))
	_ = b.MarkInvoiced()

	// Mutating the returned slice must not reach the bill either.
	if len(snap.LineItems) > 0 {
		snap.LineItems[0].Amount = money.New(123456, usd)
	}

	if got, want := b.Total().MinorUnits(), int64(500); got != want {
		t.Errorf("bill total = %d, want the frozen %d", got, want)
	}
	fresh := b.Snapshot()
	if got, want := fresh.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("snapshot total = %d, want the frozen %d", got, want)
	}
	if got, want := len(fresh.LineItems), 1; got != want {
		t.Fatalf("snapshot line items = %d, want %d", got, want)
	}
	if got, want := fresh.LineItems[0].Amount.MinorUnits(), int64(500); got != want {
		t.Errorf("snapshot line item amount = %d, want %d: the snapshot must be a copy", got, want)
	}
}

// An open bill has no closure detail to report; a closed one does.
func TestSnapshotOmitsClosureDetailWhileOpen(t *testing.T) {
	b := newOpenBill(t)
	snap := b.Snapshot()
	if snap.ClosedAt != nil {
		t.Errorf("closedAt = %v, want nil while open", snap.ClosedAt)
	}
	if snap.ClosedBy != "" {
		t.Errorf("closedBy = %q, want empty while open", snap.ClosedBy)
	}
}

// --- 3.1, 3.2 --------------------------------------------------------------

// closedSnapshotWith builds the invoice a finished bill leaves behind.
func closedSnapshotWith(t *testing.T, items ...bill.LineItem) bill.Snapshot {
	t.Helper()
	closedAt := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	total := money.Zero(usd)
	for _, it := range items {
		sum, err := total.Add(it.Amount)
		if err != nil {
			t.Fatalf("building snapshot total: %v", err)
		}
		total = sum
	}
	return bill.Snapshot{
		ID:        "bill_closed",
		State:     bill.StateClosed,
		Currency:  usd,
		Total:     total,
		LineItems: items,
		ClosedAt:  &closedAt,
		ClosedBy:  bill.TriggerAPIRequest,
	}
}

func invoicedItem(id string, minor int64, desc string) bill.LineItem {
	return bill.LineItem{
		ID:          id,
		Amount:      money.New(minor, usd),
		Description: desc,
		AccruedAt:   time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC),
	}
}

// A bill whose workflow has finished still has to answer for charges, and the
// answer depends on whether the charge is already on its invoice.
func TestCheckAgainstSnapshot(t *testing.T) {
	invoice := closedSnapshotWith(t, invoicedItem("txn_a", 500, "wire fee"))

	t.Run("identical retry returns the accrued item", func(t *testing.T) {
		got, err := bill.CheckAgainstSnapshot(invoice, invoicedItem("txn_a", 500, "wire fee"))
		if err != nil {
			t.Fatalf("returned error: %v", err)
		}
		if got == nil {
			t.Fatal("returned no item; a charge already on the invoice must be reported as present")
		}
		if got.Amount.MinorUnits() != 500 {
			t.Errorf("amount = %d, want the stored 500", got.Amount.MinorUnits())
		}
	})

	t.Run("same id with a different amount conflicts", func(t *testing.T) {
		_, err := bill.CheckAgainstSnapshot(invoice, invoicedItem("txn_a", 900, "wire fee"))
		if !errors.Is(err, bill.ErrLineItemConflict) {
			t.Fatalf("error = %v, want ErrLineItemConflict", err)
		}
	})

	t.Run("same id with a different description conflicts", func(t *testing.T) {
		_, err := bill.CheckAgainstSnapshot(invoice, invoicedItem("txn_a", 500, "late fee"))
		if !errors.Is(err, bill.ErrLineItemConflict) {
			t.Fatalf("error = %v, want ErrLineItemConflict", err)
		}
	})

	t.Run("a charge absent from the invoice arrived too late", func(t *testing.T) {
		_, err := bill.CheckAgainstSnapshot(invoice, invoicedItem("txn_new", 700, "late fee"))
		if !errors.Is(err, bill.ErrNotOpen) {
			t.Fatalf("error = %v, want ErrNotOpen", err)
		}
	})

	t.Run("missing detail is refused the same way the live path refuses it", func(t *testing.T) {
		_, err := bill.CheckAgainstSnapshot(invoice, bill.LineItem{ID: "txn_x", Description: "no amount"})
		if !errors.Is(err, bill.ErrInvalidLineItem) {
			t.Fatalf("error = %v, want ErrInvalidLineItem", err)
		}
	})
}

// 3.2 A retry carries a later accrual time than the item it repeats, because the
// clock moved between the two attempts. Comparing that field would turn every
// retry into a conflict - reporting a charge as contradictory when it is the
// same charge arriving twice.
func TestCheckAgainstSnapshotIgnoresAccrualTime(t *testing.T) {
	invoice := closedSnapshotWith(t, invoicedItem("txn_a", 500, "wire fee"))

	retry := invoicedItem("txn_a", 500, "wire fee")
	retry.AccruedAt = retry.AccruedAt.Add(90 * time.Second)

	got, err := bill.CheckAgainstSnapshot(invoice, retry)
	if err != nil {
		t.Fatalf("a retry 90s later was refused: %v", err)
	}
	if !got.AccruedAt.Equal(time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("AccruedAt = %s, want the time the charge was first accrued", got.AccruedAt)
	}
}

// The storage path and the live path must agree, or a bill would answer
// differently depending on whether its workflow happened to still be running.
func TestCheckAgainstSnapshotAgreesWithTheLivePath(t *testing.T) {
	b := newOpenBill(t)
	accrued := invoicedItem("txn_a", 500, "wire fee")
	if _, err := b.AddLineItem(accrued); err != nil {
		t.Fatalf("accruing: %v", err)
	}
	if _, err := b.Close(bill.TriggerAPIRequest, time.Now()); err != nil {
		t.Fatalf("closing: %v", err)
	}
	snap := b.Snapshot()

	for _, tc := range []struct {
		name string
		item bill.LineItem
	}{
		{"identical retry", invoicedItem("txn_a", 500, "wire fee")},
		{"conflicting reuse", invoicedItem("txn_a", 900, "wire fee")},
		{"a new charge", invoicedItem("txn_b", 100, "late fee")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, liveErr := b.AddLineItem(tc.item)
			_, snapErr := bill.CheckAgainstSnapshot(snap, tc.item)

			switch {
			case liveErr == nil && snapErr == nil:
			case liveErr == nil || snapErr == nil:
				t.Fatalf("live path returned %v but storage path returned %v", liveErr, snapErr)
			case errors.Is(liveErr, bill.ErrLineItemConflict) != errors.Is(snapErr, bill.ErrLineItemConflict),
				errors.Is(liveErr, bill.ErrNotOpen) != errors.Is(snapErr, bill.ErrNotOpen):
				t.Fatalf("live path returned %v but storage path returned %v", liveErr, snapErr)
			}
		})
	}
}
