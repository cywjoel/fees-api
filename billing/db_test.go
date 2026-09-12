package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"fees-api/internal/bill"
	"fees-api/internal/money"
)

var (
	testUSD      = money.MustLookup("USD")
	testStart    = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	testEnd      = testStart.AddDate(0, 1, 0)
	testClosedAt = testStart.Add(12 * time.Hour)
)

func testSnapshot(billID string, state bill.State, items ...bill.LineItem) bill.Snapshot {
	closedAt := testClosedAt
	total := money.Zero(testUSD)
	for _, it := range items {
		total, _ = total.Add(it.Amount)
	}
	if items == nil {
		items = []bill.LineItem{}
	}
	return bill.Snapshot{
		ID:          billID,
		State:       state,
		Currency:    testUSD,
		PeriodStart: testStart,
		PeriodEnd:   testEnd,
		CreatedAt:   testStart,
		Total:       total,
		LineItems:   items,
		ClosedAt:    &closedAt,
		ClosedBy:    bill.TriggerAPIRequest,
	}
}

func testItem(id string, minorUnits int64, desc string) bill.LineItem {
	return bill.LineItem{
		ID:          id,
		Amount:      money.New(minorUnits, testUSD),
		Description: desc,
		AccruedAt:   testStart.Add(time.Hour),
	}
}

func countRows(ctx context.Context, t *testing.T, query, billID string) int {
	t.Helper()
	var n int
	if err := billsDB.QueryRow(ctx, query, billID).Scan(&n); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	return n
}

// The migration is exercised by every test here: none of these queries would run
// against a schema that had not been applied.
func TestMigrationCreatesTheInvoiceSchema(t *testing.T) {
	ctx := context.Background()

	var n int
	if err := billsDB.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_name IN ('invoice', 'invoice_line_item')`).Scan(&n); err != nil {
		t.Fatalf("querying schema: %v", err)
	}
	if want := 2; n != want {
		t.Fatalf("found %d of the expected invoice tables, want %d", n, want)
	}
}

// Spec: billing/bill-lifecycle - "Closed bill remains retrievable long after
// closure". Also task 5.2: the write must converge, because Temporal runs an
// activity at least once and a retry must not duplicate a charge.
func TestSaveInvoiceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	snap := testSnapshot("bill_idem", bill.StateClosing,
		testItem("txn_1", 1000, "card fee"),
		testItem("txn_2", 255, "transfer fee"),
	)

	for attempt := 1; attempt <= 3; attempt++ {
		if err := saveInvoice(ctx, snap); err != nil {
			t.Fatalf("saveInvoice attempt %d returned error: %v", attempt, err)
		}
	}

	if got := countRows(ctx, t, `SELECT count(*) FROM invoice WHERE bill_id = $1`, snap.ID); got != 1 {
		t.Errorf("invoice rows = %d, want 1 after three writes", got)
	}
	if got := countRows(ctx, t, `SELECT count(*) FROM invoice_line_item WHERE bill_id = $1`, snap.ID); got != 2 {
		t.Errorf("line item rows = %d, want 2 after three writes", got)
	}

	var total int64
	if err := billsDB.QueryRow(ctx,
		`SELECT total_minor_units FROM invoice WHERE bill_id = $1`, snap.ID).Scan(&total); err != nil {
		t.Fatalf("reading total: %v", err)
	}
	if want := int64(1255); total != want {
		t.Errorf("total = %d minor units, want %d", total, want)
	}
}

// Line items are immutable once accrued, so a retry carrying different detail for
// an item id already stored must not overwrite it.
func TestSaveInvoiceDoesNotOverwriteAnAccruedLineItem(t *testing.T) {
	ctx := context.Background()
	original := testSnapshot("bill_immutable", bill.StateClosing, testItem("txn_1", 1000, "card fee"))
	if err := saveInvoice(ctx, original); err != nil {
		t.Fatalf("saveInvoice returned error: %v", err)
	}

	tampered := testSnapshot("bill_immutable", bill.StateClosing, testItem("txn_1", 9999, "tampered"))
	if err := saveInvoice(ctx, tampered); err != nil {
		t.Fatalf("second saveInvoice returned error: %v", err)
	}

	var (
		minor int64
		desc  string
	)
	if err := billsDB.QueryRow(ctx,
		`SELECT amount_minor_units, description FROM invoice_line_item WHERE bill_id = $1 AND item_id = $2`,
		"bill_immutable", "txn_1").Scan(&minor, &desc); err != nil {
		t.Fatalf("reading line item: %v", err)
	}
	if minor != 1000 || desc != "card fee" {
		t.Errorf("line item = %d %q, want 1000 \"card fee\": an accrued item must not be rewritten", minor, desc)
	}
}

// Spec: billing/bill-lifecycle - "Closed bill is retrieved with its final
// invoice".
func TestLoadInvoiceReturnsTheFrozenInvoice(t *testing.T) {
	ctx := context.Background()
	snap := testSnapshot("bill_load", bill.StateClosed,
		testItem("txn_a", 1000, "card fee"),
		testItem("txn_b", 200, "transfer fee"),
		testItem("txn_c", 55, "fx fee"),
	)
	if err := saveInvoice(ctx, snap); err != nil {
		t.Fatalf("saveInvoice returned error: %v", err)
	}

	got, err := loadInvoice(ctx, snap.ID)
	if err != nil {
		t.Fatalf("loadInvoice returned error: %v", err)
	}

	if got.State != bill.StateClosed {
		t.Errorf("state = %s, want CLOSED", got.State)
	}
	if !got.Total.Equal(snap.Total) {
		t.Errorf("total = %s, want %s", got.Total, snap.Total)
	}
	if got.Total.Currency().Code != "USD" {
		t.Errorf("currency = %q, want USD", got.Total.Currency().Code)
	}
	if got.ClosedBy != bill.TriggerAPIRequest {
		t.Errorf("closedBy = %s, want api_request", got.ClosedBy)
	}
	if got.ClosedAt == nil || !got.ClosedAt.Equal(testClosedAt) {
		t.Errorf("closedAt = %v, want %s", got.ClosedAt, testClosedAt)
	}

	// Accrual order survives the round trip.
	wantIDs := []string{"txn_a", "txn_b", "txn_c"}
	if len(got.LineItems) != len(wantIDs) {
		t.Fatalf("line items = %d, want %d", len(got.LineItems), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got.LineItems[i].ID != want {
			t.Errorf("line item %d = %q, want %q", i, got.LineItems[i].ID, want)
		}
	}
	if got, want := got.LineItems[0].Amount.MinorUnits(), int64(1000); got != want {
		t.Errorf("first line item = %d minor units, want %d", got, want)
	}
}

// The total read back is exactly the total written. Storing minor units as BIGINT
// means no representation in the database can round it.
func TestInvoiceTotalSurvivesStorageExactly(t *testing.T) {
	ctx := context.Background()

	items := make([]bill.LineItem, 100)
	for i := range items {
		items[i] = testItem("txn_"+time.Duration(i).String(), 10, "dime fee")
	}
	snap := testSnapshot("bill_exact", bill.StateClosed, items...)
	if err := saveInvoice(ctx, snap); err != nil {
		t.Fatalf("saveInvoice returned error: %v", err)
	}

	got, err := loadInvoice(ctx, snap.ID)
	if err != nil {
		t.Fatalf("loadInvoice returned error: %v", err)
	}
	if want := int64(1000); got.Total.MinorUnits() != want {
		t.Errorf("total = %d minor units, want %d", got.Total.MinorUnits(), want)
	}
	if want := "10.00 USD"; got.Total.String() != want {
		t.Errorf("total = %q, want %q", got.Total.String(), want)
	}
}

func TestSetInvoiceStateAdvancesToClosed(t *testing.T) {
	ctx := context.Background()
	snap := testSnapshot("bill_state", bill.StateClosing, testItem("txn_1", 500, "card fee"))
	if err := saveInvoice(ctx, snap); err != nil {
		t.Fatalf("saveInvoice returned error: %v", err)
	}

	loaded, err := loadInvoice(ctx, snap.ID)
	if err != nil {
		t.Fatalf("loadInvoice returned error: %v", err)
	}
	if loaded.State != bill.StateClosing {
		t.Fatalf("state before finalizing = %s, want CLOSING", loaded.State)
	}

	if err := setInvoiceState(ctx, snap.ID, bill.StateClosed); err != nil {
		t.Fatalf("setInvoiceState returned error: %v", err)
	}

	loaded, err = loadInvoice(ctx, snap.ID)
	if err != nil {
		t.Fatalf("loadInvoice returned error: %v", err)
	}
	if loaded.State != bill.StateClosed {
		t.Errorf("state = %s, want CLOSED", loaded.State)
	}
	if got, want := loaded.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("total = %d, want %d: finalizing must not alter the frozen total", got, want)
	}
}

func TestLoadInvoiceReportsAMissingBill(t *testing.T) {
	_, err := loadInvoice(context.Background(), "bill_does_not_exist")
	if !errors.Is(err, ErrInvoiceNotFound) {
		t.Fatalf("loadInvoice error = %v, want ErrInvoiceNotFound", err)
	}
}

// An invoice with no line items is legitimate: a bill can close having accrued
// nothing, and its total is zero in the bill's currency.
func TestSaveInvoiceWithNoLineItems(t *testing.T) {
	ctx := context.Background()
	snap := testSnapshot("bill_empty", bill.StateClosed)
	if err := saveInvoice(ctx, snap); err != nil {
		t.Fatalf("saveInvoice returned error: %v", err)
	}

	got, err := loadInvoice(ctx, snap.ID)
	if err != nil {
		t.Fatalf("loadInvoice returned error: %v", err)
	}
	if !got.Total.IsZero() {
		t.Errorf("total = %s, want zero", got.Total)
	}
	if len(got.LineItems) != 0 {
		t.Errorf("line items = %d, want 0", len(got.LineItems))
	}
}
