// Package e2e drives the running Fees API over HTTP.
//
// These tests are deliberately end to end: they exercise the real Encore
// endpoints, the real Temporal worker, and the real database, which is the only
// way to verify the parts that unit tests cannot reach - the status codes the raw
// handlers write, the workflow-id reuse policies, and the hand-off from HTTP
// request to workflow update to durable row.
//
// They are skipped unless FEES_API_BASE_URL is set, so `go test ./...` stays
// runnable without infrastructure:
//
//	temporal server start-dev          # terminal 1
//	encore run                         # terminal 2
//	FEES_API_BASE_URL=http://127.0.0.1:4000 go test ./e2e/ -v
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

var baseURL string

func TestMain(m *testing.M) {
	baseURL = os.Getenv("FEES_API_BASE_URL")
	os.Exit(m.Run())
}

func requireAPI(t *testing.T) {
	t.Helper()
	if baseURL == "" {
		t.Skip("set FEES_API_BASE_URL to run the end-to-end API tests")
	}
}

// response is a decoded API response.
type response struct {
	status  int
	headers http.Header
	body    map[string]any
	raw     []byte
}

// reason returns the machine-readable rejection reason, if the body carries one.
func (r response) reason() string {
	if v, ok := r.body["reason"].(string); ok {
		return v
	}
	return ""
}

// totalMinorUnits reads the bill total's exact integer form.
func (r response) totalMinorUnits(t *testing.T) int64 {
	t.Helper()
	total, ok := r.body["total"].(map[string]any)
	if !ok {
		t.Fatalf("response has no total: %s", r.raw)
	}
	f, ok := total["minorUnits"].(float64)
	if !ok {
		t.Fatalf("total has no minorUnits: %s", r.raw)
	}
	return int64(f)
}

func do(t *testing.T, method, path string, body any, headers map[string]string) response {
	t.Helper()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding request: %v", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, headers: resp.Header, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// createBill opens a bill whose period runs for the given duration.
//
// Each call reads the clock afresh, so two calls describe two different fee
// periods. That is fine for creating unrelated bills, but it must not be used to
// model a retry: a repeat carrying the same idempotency key and a period a few
// milliseconds different is a different request, and is refused as key reuse.
// Use createBillBody for a retry, which resends identical bytes the way a client
// retrying a failed call actually would.
func createBill(t *testing.T, currency string, period time.Duration, headers map[string]string) response {
	t.Helper()
	return do(t, http.MethodPost, "/bills", createBillBody(currency, period), headers)
}

// createBillBody builds a creation request that can be sent more than once.
func createBillBody(currency string, period time.Duration) map[string]any {
	now := time.Now().UTC()
	return map[string]any{
		"currency":    currency,
		"periodStart": now.Format(time.RFC3339Nano),
		"periodEnd":   now.Add(period).Format(time.RFC3339Nano),
	}
}

func addItem(t *testing.T, billID, itemID string, minorUnits int64, currency, desc string) response {
	t.Helper()
	return do(t, http.MethodPut, "/bills/"+billID+"/line-items/"+itemID, map[string]any{
		"amount":      map[string]any{"minorUnits": minorUnits, "currency": currency},
		"description": desc,
	}, nil)
}

func billIDOf(t *testing.T, r response) string {
	t.Helper()
	id, ok := r.body["id"].(string)
	if !ok {
		t.Fatalf("response has no bill id: %s", r.raw)
	}
	return id
}

// waitForState polls until the bill reaches want, or the deadline passes.
func waitForState(t *testing.T, billID, want string, within time.Duration) response {
	t.Helper()
	deadline := time.Now().Add(within)
	var last response
	for time.Now().Before(deadline) {
		last = do(t, http.MethodGet, "/bills/"+billID, nil, nil)
		if s, _ := last.body["state"].(string); s == want {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("bill %s did not reach %s within %s; last state was %v (%s)",
		billID, want, within, last.body["state"], last.raw)
	return last
}

// --- 1.3 to 1.6: defects pinned before their fixes --------------------------
//
// These fail today. They document defects found reviewing the initial commit,
// so that the fixes in groups 4 and 5 are verified by something durable and
// executed rather than by an ad-hoc request - which is how the defect they sit
// alongside survived in the first place.

// 1.3 A body carrying no amount decodes to a zero Money whose currency is empty.
// Nothing validates it before dispatch, and the update argument then fails to
// deserialise on the worker, so the validator never runs and the caller is told
// the service broke rather than that its request was malformed.
func TestLineItemWithoutAmountIsRejectedAsMalformed(t *testing.T) {
	requireAPI(t)

	billID := billIDOf(t, createBill(t, "USD", time.Hour, nil))

	got := do(t, http.MethodPut, "/bills/"+billID+"/line-items/txn_no_amount",
		map[string]any{"description": "fee with no amount"}, nil)

	if got.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)\n\n"+
			"A 500 here means the amount reached the worker unvalidated and failed to "+
			"deserialise, so the rejection never happened in the validator where it belongs.",
			got.status, got.raw)
	}
	if got.reason() != "invalid_line_item" {
		t.Errorf("reason = %q, want invalid_line_item", got.reason())
	}
}

// 1.4 Under CONFLICT_POLICY_USE_EXISTING a start that attaches to a running
// execution returns no error, so the handler cannot tell creating from
// attaching and every concurrent request claims to have created the bill.
func TestConcurrentCreationsSharingAKeyReportOneCreation(t *testing.T) {
	requireAPI(t)

	const attempts = 4
	key := fmt.Sprintf("concurrent-%d", time.Now().UnixNano())
	headers := map[string]string{"Idempotency-Key": key}

	// One body shared by every goroutine: this tests concurrent retries of the
	// same request, not four different requests wearing one key.
	body := createBillBody("USD", time.Hour)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([]response, 0, attempts)
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := do(t, http.MethodPost, "/bills", body, headers)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}()
	}
	wg.Wait()

	created, existing, ids := 0, 0, map[string]bool{}
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			existing++
		default:
			t.Errorf("unexpected status %d (%s)", r.status, r.raw)
		}
		if id, ok := r.body["id"].(string); ok {
			ids[id] = true
		}
	}

	if created != 1 {
		t.Errorf("%d requests reported 201 Created, want exactly 1 (%d reported 200)\n\n"+
			"Every concurrent request claiming creation means a caller counting 201s "+
			"believes several bills exist for one key.", created, existing)
	}
	if len(ids) != 1 {
		t.Errorf("requests returned %d distinct bill ids, want 1: %v", len(ids), ids)
	}
}

// 1.5 The idempotency pre-check returns the bill a key already created without
// comparing it against what was asked for, so a key reused by accident is
// answered with a bill in the wrong currency, for the wrong period.
func TestKeyReusedWithDifferentParametersIsRejected(t *testing.T) {
	requireAPI(t)

	headers := map[string]string{"Idempotency-Key": fmt.Sprintf("reuse-%d", time.Now().UnixNano())}

	first := createBill(t, "USD", time.Hour, headers)
	if first.status != http.StatusCreated {
		t.Fatalf("first creation = %d, want 201 (%s)", first.status, first.raw)
	}

	now := time.Now().UTC().Add(720 * time.Hour)
	second := do(t, http.MethodPost, "/bills", map[string]any{
		"currency":    "GEL",
		"periodStart": now.Format(time.RFC3339Nano),
		"periodEnd":   now.Add(time.Hour).Format(time.RFC3339Nano),
	}, headers)

	if second.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)\n\n"+
			"A 200 here returns a USD bill to a caller that asked for a GEL one, for a "+
			"different period. It will then accrue GEL charges that are all rejected 422, "+
			"with nothing explaining why.", second.status, second.raw)
	}
}

// The same key with the same period stated in another time zone denotes the same
// instant and must still be a retry. Comparing with == rather than time.Equal
// would reject a correct request - a false 409, worse than the defect above.
func TestKeyReuseComparesInstantsNotRepresentations(t *testing.T) {
	requireAPI(t)

	headers := map[string]string{"Idempotency-Key": fmt.Sprintf("tz-%d", time.Now().UnixNano())}

	start := time.Now().UTC().Truncate(time.Second)
	end := start.Add(time.Hour)

	first := do(t, http.MethodPost, "/bills", map[string]any{
		"currency":    "USD",
		"periodStart": start.Format(time.RFC3339),
		"periodEnd":   end.Format(time.RFC3339),
	}, headers)
	if first.status != http.StatusCreated {
		t.Fatalf("first creation = %d, want 201 (%s)", first.status, first.raw)
	}

	elsewhere := time.FixedZone("UTC+4", 4*60*60)
	second := do(t, http.MethodPost, "/bills", map[string]any{
		"currency":    "USD",
		"periodStart": start.In(elsewhere).Format(time.RFC3339),
		"periodEnd":   end.In(elsewhere).Format(time.RFC3339),
	}, headers)

	if second.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)\n\n"+
			"The same instant written with a different offset is the same fee period. "+
			"Rejecting it refuses a correct retry.", second.status, second.raw)
	}
}

// 1.6 Creation checks only that periodEnd follows periodStart, never that it is
// still ahead. A period that has already ended builds a timer with a negative
// duration, which fires at once, so the bill is closing before the caller has
// read the response that says it was created.
func TestFeePeriodAlreadyEndedIsRejected(t *testing.T) {
	requireAPI(t)

	start := time.Now().UTC().Add(-720 * time.Hour)
	got := do(t, http.MethodPost, "/bills", map[string]any{
		"currency":    "USD",
		"periodStart": start.Format(time.RFC3339Nano),
		"periodEnd":   start.Add(24 * time.Hour).Format(time.RFC3339Nano),
	}, map[string]string{"Idempotency-Key": fmt.Sprintf("past-%d", time.Now().UnixNano())})

	if got.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d with state %v, want 422 (%s)\n\n"+
			"A 201 carrying state CLOSING reports success for a bill that can never "+
			"accept a charge.", got.status, got.body["state"], got.raw)
	}
	if got.reason() != "invalid_period" {
		t.Errorf("reason = %q, want invalid_period", got.reason())
	}
	if id, ok := got.body["id"].(string); ok && id != "" {
		t.Errorf("a bill was created (%s) for a period that had already ended", id)
	}
}

// --- 10.1 ------------------------------------------------------------------

// The whole path in one test: HTTP request, workflow update, activity execution,
// durable row.
func TestHappyPathEndToEnd(t *testing.T) {
	requireAPI(t)

	created := createBill(t, "USD", time.Hour, nil)
	if created.status != http.StatusCreated {
		t.Fatalf("POST /bills = %d, want 201 (%s)", created.status, created.raw)
	}
	if loc := created.headers.Get("Location"); loc == "" {
		t.Error("POST /bills returned no Location header")
	}
	billID := billIDOf(t, created)

	for _, it := range []struct {
		id    string
		minor int64
		desc  string
	}{
		{id: "txn_1", minor: 1000, desc: "card fee"},
		{id: "txn_2", minor: 200, desc: "transfer fee"},
		{id: "txn_3", minor: 55, desc: "fx fee"},
	} {
		got := addItem(t, billID, it.id, it.minor, "USD", it.desc)
		if got.status != http.StatusCreated {
			t.Fatalf("PUT line item %s = %d, want 201 (%s)", it.id, got.status, got.raw)
		}
	}

	open := do(t, http.MethodGet, "/bills/"+billID, nil, nil)
	if open.status != http.StatusOK {
		t.Fatalf("GET open bill = %d, want 200", open.status)
	}
	if got := open.totalMinorUnits(t); got != 1255 {
		t.Errorf("running total = %d minor units, want 1255", got)
	}

	closed := do(t, http.MethodPost, "/bills/"+billID+"/close", nil, nil)
	if closed.status != http.StatusAccepted {
		t.Fatalf("POST close = %d, want 202 (%s)", closed.status, closed.raw)
	}
	if got := closed.body["closedBy"]; got != "api_request" {
		t.Errorf("closedBy = %v, want api_request", got)
	}
	if got := closed.totalMinorUnits(t); got != 1255 {
		t.Errorf("frozen total = %d minor units, want 1255", got)
	}
	items, _ := closed.body["lineItems"].([]any)
	if len(items) != 3 {
		t.Errorf("close returned %d line items, want 3", len(items))
	}

	// The invoice reaches durable storage and reads back identically.
	final := waitForState(t, billID, "CLOSED", 30*time.Second)
	if got := final.totalMinorUnits(t); got != 1255 {
		t.Errorf("persisted total = %d minor units, want 1255", got)
	}
	finalItems, _ := final.body["lineItems"].([]any)
	if len(finalItems) != 3 {
		t.Errorf("persisted invoice has %d line items, want 3", len(finalItems))
	}
}

// --- 6.1 -------------------------------------------------------------------

func TestCreateBillIsIdempotentByKey(t *testing.T) {
	requireAPI(t)

	key := fmt.Sprintf("key-%d", time.Now().UnixNano())
	headers := map[string]string{"Idempotency-Key": key}

	// One body, sent three times, as a retrying client would send it.
	body := createBillBody("USD", time.Hour)

	first := do(t, http.MethodPost, "/bills", body, headers)
	if first.status != http.StatusCreated {
		t.Fatalf("first POST /bills = %d, want 201 (%s)", first.status, first.raw)
	}
	billID := billIDOf(t, first)

	// The bill is still open, so the start attaches to the running execution and
	// reports that it did not create it.
	second := do(t, http.MethodPost, "/bills", body, headers)
	if second.status != http.StatusOK {
		t.Fatalf("repeat POST /bills while open = %d, want 200 (%s)", second.status, second.raw)
	}
	if got := billIDOf(t, second); got != billID {
		t.Errorf("repeat returned bill %q, want the existing %q", got, billID)
	}

	// Close it, let the workflow finish, and repeat once more. Without
	// REJECT_DUPLICATE this is where a second bill would be created silently.
	if r := do(t, http.MethodPost, "/bills/"+billID+"/close", nil, nil); r.status != http.StatusAccepted {
		t.Fatalf("close = %d, want 202", r.status)
	}
	waitForState(t, billID, "CLOSED", 30*time.Second)

	third := do(t, http.MethodPost, "/bills", body, headers)
	if third.status != http.StatusOK {
		t.Fatalf("repeat POST /bills after close = %d, want 200 (%s)", third.status, third.raw)
	}
	if got := billIDOf(t, third); got != billID {
		t.Errorf("repeat after close returned bill %q, want the existing %q", got, billID)
	}
	if got := third.body["state"]; got != "CLOSED" {
		t.Errorf("repeat after close returned state %v, want CLOSED", got)
	}
}

func TestCreateBillWithoutKeyMakesDistinctBills(t *testing.T) {
	requireAPI(t)

	a := createBill(t, "USD", time.Hour, nil)
	b := createBill(t, "USD", time.Hour, nil)
	if billIDOf(t, a) == billIDOf(t, b) {
		t.Error("two unkeyed creations produced the same bill")
	}
}

// --- 6.2 -------------------------------------------------------------------

func TestAddLineItemStatusMatrix(t *testing.T) {
	requireAPI(t)

	billID := billIDOf(t, createBill(t, "USD", time.Hour, nil))

	t.Run("201 for a new line item", func(t *testing.T) {
		got := addItem(t, billID, "txn_1", 500, "USD", "card fee")
		if got.status != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (%s)", got.status, got.raw)
		}
		if got.body["outcome"] != "created" {
			t.Errorf("outcome = %v, want created", got.body["outcome"])
		}
	})

	t.Run("200 for an identical retry", func(t *testing.T) {
		got := addItem(t, billID, "txn_1", 500, "USD", "card fee")
		if got.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", got.status, got.raw)
		}
		if got.body["outcome"] != "already_accrued" {
			t.Errorf("outcome = %v, want already_accrued", got.body["outcome"])
		}
	})

	t.Run("409 for the same id with a different amount", func(t *testing.T) {
		got := addItem(t, billID, "txn_1", 999, "USD", "card fee")
		if got.status != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (%s)", got.status, got.raw)
		}
		if got.reason() != "line_item_conflict" {
			t.Errorf("reason = %q, want line_item_conflict", got.reason())
		}
	})

	t.Run("422 for a currency mismatch", func(t *testing.T) {
		got := addItem(t, billID, "txn_gel", 500, "GEL", "lari fee")
		if got.status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (%s)", got.status, got.raw)
		}
		if got.reason() != "currency_mismatch" {
			t.Errorf("reason = %q, want currency_mismatch", got.reason())
		}
	})

	t.Run("409 once the bill is no longer open", func(t *testing.T) {
		if r := do(t, http.MethodPost, "/bills/"+billID+"/close", nil, nil); r.status != http.StatusAccepted {
			t.Fatalf("close = %d, want 202", r.status)
		}

		// Wait for CLOSED before offering the late charge.
		//
		// Without this the request lands in the brief CLOSING window, where the
		// workflow is still running and its validator answers the update - the one
		// state in which the rejection is correct. Every real client meets the bill
		// after its workflow has completed, and this test must meet it there too.
		waitForState(t, billID, "CLOSED", 30*time.Second)

		got := addItem(t, billID, "txn_late", 700, "USD", "late fee")
		if got.status != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (%s)\n\n"+
				"A 404 here means the bill's completed workflow was read as the bill not "+
				"existing. GET on this same id answers 200 with the invoice, so the charge "+
				"is being refused as unknown rather than as too late.", got.status, got.raw)
		}
		if got.reason() != "bill_not_open" {
			t.Errorf("reason = %q, want bill_not_open", got.reason())
		}
	})

	// Spec: billing/line-item-accrual - "Retry of an invoiced charge after the
	// closing process has finished". The 200 above was answered by the running
	// workflow; this one has to be answered from the invoice, because no live
	// execution remains. A caller retrying after a timeout must be told the charge
	// is present, not that the bill is unknown - otherwise it may compensate for
	// money that is genuinely on the invoice.
	t.Run("200 for a retry of an invoiced charge once the workflow has finished", func(t *testing.T) {
		got := addItem(t, billID, "txn_1", 500, "USD", "card fee")
		if got.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", got.status, got.raw)
		}
		if outcome, _ := got.body["outcome"].(string); outcome != "already_accrued" {
			t.Errorf("outcome = %q, want already_accrued", outcome)
		}
	})

	t.Run("404 for a bill that does not exist", func(t *testing.T) {
		got := addItem(t, "bill_nope", "txn_1", 500, "USD", "card fee")
		if got.status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (%s)", got.status, got.raw)
		}
	})
}

// Spec: billing/line-item-accrual - "Simultaneous additions are all retained".
//
// This is the claim the whole design rests on, and until now it was asserted in
// prose and exercised nowhere. A workflow executes as a single-threaded
// deterministic coroutine scheduler, so concurrent additions to one bill are
// ordered by construction - no row lock, no optimistic-concurrency retry, no lost
// update. The domain cannot demonstrate that, because Bill is deliberately not
// safe for concurrent use: serialising writes is the workflow's job, so the proof
// has to come through the real API against a real worker.
//
// Distinct amounts make a lost update visible rather than merely possible: a
// dropped write changes the total by a unique value, so the failure names itself.
func TestConcurrentLineItemsAreAllRetained(t *testing.T) {
	requireAPI(t)

	billID := billIDOf(t, createBill(t, "USD", time.Hour, nil))

	const items = 24
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses = map[int]int{}
		wantSum  int64
	)
	for i := 0; i < items; i++ {
		amount := int64(100 + i) // distinct, so a lost write is identifiable
		wantSum += amount
		wg.Add(1)
		go func(i int, amount int64) {
			defer wg.Done()
			r := addItem(t, billID, fmt.Sprintf("txn_%02d", i), amount, "USD", "concurrent fee")
			mu.Lock()
			statuses[r.status]++
			mu.Unlock()
		}(i, amount)
	}
	wg.Wait()

	if got := statuses[http.StatusCreated]; got != items {
		t.Errorf("%d additions reported 201, want %d (all statuses: %v)", got, items, statuses)
	}

	final := do(t, http.MethodGet, "/bills/"+billID, nil, nil)
	if final.status != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (%s)", final.status, final.raw)
	}

	lineItems, _ := final.body["lineItems"].([]any)
	if len(lineItems) != items {
		t.Errorf("bill carries %d line items, want %d: a concurrent write was lost", len(lineItems), items)
	}

	seen := map[string]bool{}
	for _, raw := range lineItems {
		if item, ok := raw.(map[string]any); ok {
			id, _ := item["id"].(string)
			if seen[id] {
				t.Errorf("line item %q appears more than once", id)
			}
			seen[id] = true
		}
	}
	for i := 0; i < items; i++ {
		if id := fmt.Sprintf("txn_%02d", i); !seen[id] {
			t.Errorf("line item %q is missing from the bill", id)
		}
	}

	if got := final.totalMinorUnits(t); got != wantSum {
		t.Errorf("total = %d minor units, want %d: the total is not the exact sum of "+
			"every charge, so a concurrent write was lost or double-counted", got, wantSum)
	}
}

// --- 6.3 -------------------------------------------------------------------

func TestCloseIsIdempotentAndReportsTrigger(t *testing.T) {
	requireAPI(t)

	billID := billIDOf(t, createBill(t, "USD", time.Hour, nil))
	addItem(t, billID, "txn_1", 500, "USD", "card fee")

	first := do(t, http.MethodPost, "/bills/"+billID+"/close", nil, nil)
	if first.status != http.StatusAccepted {
		t.Fatalf("first close = %d, want 202 (%s)", first.status, first.raw)
	}

	second := do(t, http.MethodPost, "/bills/"+billID+"/close", nil, nil)
	if second.status != http.StatusAccepted {
		t.Fatalf("repeat close = %d, want 202 (%s)", second.status, second.raw)
	}
	if second.body["closedBy"] != "api_request" {
		t.Errorf("closedBy = %v, want api_request", second.body["closedBy"])
	}
	if first.totalMinorUnits(t) != second.totalMinorUnits(t) {
		t.Error("repeat close reported a different total")
	}

	// And once the workflow has finished, the request is still satisfied from
	// storage rather than failing.
	waitForState(t, billID, "CLOSED", 30*time.Second)
	third := do(t, http.MethodPost, "/bills/"+billID+"/close", nil, nil)
	if third.status != http.StatusAccepted {
		t.Fatalf("close after completion = %d, want 202 (%s)", third.status, third.raw)
	}
}

// Spec: billing/bill-lifecycle - "Bill closes automatically at period end".
//
// The period bounds being caller-supplied is what makes this observable by hand:
// a period of a few seconds exercises exactly the same timer that a month-long
// period would.
func TestBillClosesItselfAtPeriodEnd(t *testing.T) {
	requireAPI(t)

	created := createBill(t, "USD", 5*time.Second, nil)
	billID := billIDOf(t, created)
	addItem(t, billID, "txn_1", 750, "USD", "card fee")

	final := waitForState(t, billID, "CLOSED", 60*time.Second)
	if got := final.body["closedBy"]; got != "period_end" {
		t.Errorf("closedBy = %v, want period_end", got)
	}
	if got := final.totalMinorUnits(t); got != 750 {
		t.Errorf("total = %d minor units, want 750", got)
	}
}

// --- 6.4 -------------------------------------------------------------------

func TestGetBillUnknownIsNotFound(t *testing.T) {
	requireAPI(t)

	got := do(t, http.MethodGet, "/bills/bill_definitely_not_real", nil, nil)
	if got.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", got.status, got.raw)
	}
	if got.reason() != "bill_not_found" {
		t.Errorf("reason = %q, want bill_not_found", got.reason())
	}
}

// --- 6.5 -------------------------------------------------------------------

func TestCreateBillValidation(t *testing.T) {
	requireAPI(t)

	now := time.Now().UTC()

	tests := []struct {
		name       string
		body       map[string]any
		wantReason string
	}{
		{
			name: "period ends before it begins",
			body: map[string]any{
				"currency":    "USD",
				"periodStart": now.Format(time.RFC3339Nano),
				"periodEnd":   now.Add(-time.Hour).Format(time.RFC3339Nano),
			},
			wantReason: "invalid_period",
		},
		{
			name: "period ends exactly when it begins",
			body: map[string]any{
				"currency":    "USD",
				"periodStart": now.Format(time.RFC3339Nano),
				"periodEnd":   now.Format(time.RFC3339Nano),
			},
			wantReason: "invalid_period",
		},
		{
			name: "unsupported currency",
			body: map[string]any{
				"currency":    "EUR",
				"periodStart": now.Format(time.RFC3339Nano),
				"periodEnd":   now.Add(time.Hour).Format(time.RFC3339Nano),
			},
			wantReason: "unsupported_currency",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := do(t, http.MethodPost, "/bills", tc.body, nil)
			if got.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", got.status, got.raw)
			}
			if got.reason() != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.reason(), tc.wantReason)
			}
			if _, created := got.body["id"]; created {
				t.Error("a rejected request created a bill")
			}
		})
	}
}

// --- 6.6 -------------------------------------------------------------------

// Every refusal carries a machine-readable reason, and 409 is reserved for a
// request that contradicts the bill's state while 422 marks one that is
// well-formed but cannot be acted on. A client can tell the two apart without
// parsing prose.
func TestEveryRefusalCarriesAMachineReadableReason(t *testing.T) {
	requireAPI(t)

	billID := billIDOf(t, createBill(t, "USD", time.Hour, nil))
	addItem(t, billID, "txn_1", 500, "USD", "card fee")

	contradictions := []response{
		addItem(t, billID, "txn_1", 999, "USD", "card fee"),
	}
	malformed := []response{
		addItem(t, billID, "txn_gel", 500, "GEL", "lari fee"),
	}

	for _, r := range contradictions {
		if r.status != http.StatusConflict {
			t.Errorf("contradictory request returned %d, want 409 (%s)", r.status, r.raw)
		}
		if r.reason() == "" {
			t.Errorf("409 response carries no reason: %s", r.raw)
		}
		if ct := r.headers.Get("Content-Type"); ct == "" {
			t.Error("error response has no Content-Type")
		}
	}
	for _, r := range malformed {
		if r.status != http.StatusUnprocessableEntity {
			t.Errorf("malformed request returned %d, want 422 (%s)", r.status, r.raw)
		}
		if r.reason() == "" {
			t.Errorf("422 response carries no reason: %s", r.raw)
		}
	}
}

// Money crosses the wire without ever being a JSON number, so no consumer's
// decoder can route it through a float.
func TestAmountsCrossTheWireAsStrings(t *testing.T) {
	requireAPI(t)

	billID := billIDOf(t, createBill(t, "USD", time.Hour, nil))
	addItem(t, billID, "txn_1", 10, "USD", "dime fee")

	got := do(t, http.MethodGet, "/bills/"+billID, nil, nil)
	total, ok := got.body["total"].(map[string]any)
	if !ok {
		t.Fatalf("no total in %s", got.raw)
	}
	if _, isString := total["amount"].(string); !isString {
		t.Errorf("total.amount is %T, want a JSON string", total["amount"])
	}
	if amount, _ := total["amount"].(string); amount != "0.10" {
		t.Errorf("total.amount = %q, want \"0.10\"", amount)
	}
}
