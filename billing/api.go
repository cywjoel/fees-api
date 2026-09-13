package billing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"encore.dev"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"fees-api/internal/bill"
	"fees-api/internal/billflow"
	"fees-api/internal/money"
)

// temporalCallTimeout bounds how long an API request waits on Temporal. Without
// it the SDK retries against the request's own deadline-free context, so an
// outage leaves a mutation hanging rather than refusing it. Timing out is safe
// because every mutation is idempotent: a premature 503 cannot double-charge.
const temporalCallTimeout = 10 * time.Second

// temporalContext bounds a Temporal call, while still cancelling if the client
// disconnects first.
func temporalContext(req *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(req.Context(), temporalCallTimeout)
}

type CreateBillRequest struct {
	CustomerID  string    `json:"customerId"`
	Currency    string    `json:"currency"`
	PeriodStart time.Time `json:"periodStart"`
	PeriodEnd   time.Time `json:"periodEnd"`
}

// AddLineItemRequest accrues one charge. The item's identifier is the path
// segment, not a body field: it identifies the resource being created.
type AddLineItemRequest struct {
	// Optional, and a checksum rather than an identity claim: the caller already
	// chose the bill by its id, so this states what the caller believes and lets
	// the system disagree.
	CustomerID  string      `json:"customerId,omitempty"`
	Amount      money.Money `json:"amount"`
	Description string      `json:"description"`
}

// matchesRequest reports whether an existing bill is the one this request asked
// for, which is what makes an accidentally reused idempotency key safe.
func matchesRequest(snap bill.Snapshot, currency money.Currency, in CreateBillRequest) bool {
	// Equal, never ==, which would also compare location and monotonic reading;
	// and both sides at storage precision, since a bill read back from Postgres
	// has lost its sub-microsecond digits. Each caused a false 409 by its absence.
	return snap.Currency.Code == currency.Code &&
		atStoragePrecision(snap.PeriodStart).Equal(atStoragePrecision(in.PeriodStart)) &&
		atStoragePrecision(snap.PeriodEnd).Equal(atStoragePrecision(in.PeriodEnd))
}

// writeKeyReuse refuses a key that already named a different bill.
func writeKeyReuse(w http.ResponseWriter, billID string, snap bill.Snapshot) {
	// Deliberately unread: echoing the existing bill's currency and period would
	// make this a read endpoint wearing a 409.
	_ = snap
	writeProblem(w, http.StatusConflict, reasonKeyReuse,
		"this idempotency key already created bill "+billID+" with different parameters; "+
			"retry with the parameters that key was first used for, or use a different key")
}

// validateLineItemRequest reports what is wrong with a charge before it is sent
// anywhere.
//
// It exists because an absent amount survives decoding: the zero Money marshals
// onto the update payload and fails on the worker, so the caller is told the
// service broke rather than that its request was malformed.
func validateLineItemRequest(itemID string, in AddLineItemRequest) error {
	if itemID == "" {
		return fmt.Errorf("%w: item id is required", bill.ErrInvalidLineItem)
	}
	if in.Amount.Currency().IsZero() {
		return fmt.Errorf("%w: amount and its currency are required", bill.ErrInvalidLineItem)
	}
	if in.Description == "" {
		return fmt.Errorf("%w: description is required", bill.ErrInvalidLineItem)
	}
	return nil
}

// CreateBill opens a new bill and starts the workflow that owns it.
//
// Idempotent through the Idempotency-Key header, mapped deterministically onto
// the workflow id, so uniqueness comes from Temporal rather than from a
// deduplication table.
//
//encore:api public raw method=POST path=/bills
func (s *Service) CreateBill(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := temporalContext(req)
	defer cancel()

	var in CreateBillRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		writeProblem(w, http.StatusBadRequest, billflow.ReasonInvalidBill,
			"request body is not valid JSON: "+err.Error())
		return
	}

	if strings.TrimSpace(in.CustomerID) == "" {
		// Required here and not in the domain: bill.New must tolerate an empty
		// customer so old histories still replay; see bill.New.
		//
		// Trimmed for the test, stored as given. Whitespace-only identifies nobody,
		// but an identifier that meaningfully carries padding is kept as sent.
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonInvalidBill,
			"customerId is required and cannot be blank; a bill exists to be invoiced to someone")
		return
	}

	currency, err := money.Lookup(in.Currency)
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonUnsupportedCurrency,
			"currency must be one of "+joinCodes(money.Supported()))
		return
	}
	// Before validation, not after: two instants less than a microsecond apart
	// would otherwise satisfy "strictly after" and then collapse on the way to
	// storage, which the invoice table's CHECK constraint forbids.
	in.PeriodStart = atStoragePrecision(in.PeriodStart)
	in.PeriodEnd = atStoragePrecision(in.PeriodEnd)

	if !in.PeriodEnd.After(in.PeriodStart) {
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonInvalidPeriod,
			"periodEnd must be strictly after periodStart")
		return
	}
	if !in.PeriodEnd.After(time.Now()) {
		// A period already ended builds a timer with a negative duration and closes
		// before the caller has read the 201 - a bill that can never take a charge.
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonInvalidPeriod,
			"periodEnd is in the past; a bill cannot accrue charges over a period that has already ended")
		return
	}

	billID, keyed := billIDForCustomer(in.CustomerID, req.Header.Get("Idempotency-Key"))

	// A key that has already produced a bill returns that bill rather than a
	// second one. This covers the completed case too, where the workflow is gone
	// and the invoice is read from storage.
	if keyed {
		switch snap, readErr := s.readBill(ctx, billID); {
		case readErr == nil:
			if !matchesRequest(snap, currency, in) {
				writeKeyReuse(w, billID, snap)
				return
			}
			w.Header().Set("Location", "/bills/"+billID)
			writeJSON(w, http.StatusOK, snap)
			return
		case isWorkflowBusy(readErr):
			// Whether this key already made a bill is unknowable right now.
			// Starting one anyway risks a second bill for the same key, so the
			// honest answer is to ask the caller to retry.
			writeUnavailable(w, "could not determine whether this idempotency key already created a bill: "+readErr.Error())
			return
		}

	}

	started, err := s.startBillWorkflow(ctx, billID, billflow.StartBillInput{
		BillID:      billID,
		CustomerID:  in.CustomerID,
		Currency:    currency.Code,
		PeriodStart: in.PeriodStart,
		PeriodEnd:   in.PeriodEnd,
	})
	if err != nil {
		// The key names a bill that already exists. Return it rather than a second.
		if isWorkflowAlreadyStarted(err) {
			// This request lost the race, or repeats a key that already made a bill.
			// Either way the bill exists; compare here as well as in the pre-check,
			// because two concurrent requests can both pass the pre-check before
			// either has created anything.
			snap, readErr := s.readBill(ctx, billID)
			if readErr == nil {
				if !matchesRequest(snap, currency, in) {
					writeKeyReuse(w, billID, snap)
					return
				}
				w.Header().Set("Location", "/bills/"+billID)
				writeJSON(w, http.StatusOK, snap)
				return
			}
			if isWorkflowBusy(readErr) {
				writeUnavailable(w, "the existing bill for this key could not be read: "+readErr.Error())
				return
			}
		}
		if isWorkflowBusy(err) {
			writeUnavailable(w, "the bill could not be started: "+err.Error())
			return
		}
		writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal,
			"starting bill workflow: "+err.Error())
		return
	}

	snap, err := s.readBill(ctx, billID)
	if err != nil {
		if isWorkflowBusy(err) {
			// The bill exists - the start succeeded - but its state cannot be read
			// back right now. Reporting a fault would tell the caller its bill was
			// not created, and a caller without an idempotency key that retries on
			// that basis creates a second one. 503 asks for the retry that will
			// return the bill, and creation is idempotent so the retry is safe.
			writeUnavailable(w, "the bill was created but its state could not be read back: "+err.Error())
			return
		}
		writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal,
			"reading the new bill: "+err.Error())
		return
	}

	// Only the request that actually created the execution reports 201. Without
	// this, concurrent requests sharing a key all claim to have created the bill.
	if !started && !matchesRequest(snap, currency, in) {
		writeKeyReuse(w, billID, snap)
		return
	}
	status := http.StatusOK
	if started {
		status = http.StatusCreated
	}

	w.Header().Set("Location", "/bills/"+billID)
	writeJSON(w, status, snap)
}

// startBillWorkflow starts a bill's workflow and reports whether this request is
// the one that created it.
//
// It calls the service API directly rather than client.ExecuteWorkflow because
// the SDK helper cannot report that: under any conflict policy it returns the
// existing run handle and a nil error, so every concurrent request sharing a key
// reports 201. The raw response's Started flag is the missing signal.
func (s *Service) startBillWorkflow(ctx context.Context, billID string, in billflow.StartBillInput) (bool, error) {
	payload, err := dataConverter.ToPayloads(in)
	if err != nil {
		return false, fmt.Errorf("encoding bill workflow input: %w", err)
	}

	resp, err := s.temporal.WorkflowService().StartWorkflowExecution(ctx,
		&workflowservice.StartWorkflowExecutionRequest{
			Namespace:    temporalNamespace(),
			WorkflowId:   billID,
			WorkflowType: &commonpb.WorkflowType{Name: billflow.WorkflowTypeName},
			TaskQueue:    &taskqueuepb.TaskQueue{Name: billflow.TaskQueue},
			Input:        payload,
			// Identifies this attempt. Distinct per request, so two concurrent
			// requests are two attempts and exactly one of them starts the bill; a
			// shared value would make the server treat them as one retried request.
			RequestId: newRequestID(),
			// REJECT_DUPLICATE: without it a repeated key would silently start a
			// second bill once the first had closed. USE_EXISTING: attaching is the
			// ordinary outcome of a retry, not an error path.
			WorkflowIdReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
			WorkflowIdConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		})
	if err != nil {
		return false, err
	}
	return resp.Started, nil
}

// newRequestID returns a fresh identifier for one start attempt.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// AddLineItem accrues a charge onto an open bill, keyed by a caller-supplied
// item id.
//
//encore:api public raw method=PUT path=/bills/:billId/line-items/:itemId
func (s *Service) AddLineItem(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := temporalContext(req)
	defer cancel()
	billID := pathParam("billId")
	itemID := pathParam("itemId")

	var in AddLineItemRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonInvalidLineItem,
			"request body is not a valid line item: "+err.Error())
		return
	}

	if err := validateLineItemRequest(itemID, in); err != nil {
		writeProblem(w, statusForDomainError(err), billflow.ClassifyRejection(err), err.Error())
		return
	}

	// No UpdateID, deliberately: the item id is the obvious candidate and would be
	// wrong. Temporal deduplicates by update id before the validator runs, so a
	// second request reusing an item id with a different amount would be answered
	// from the first result and the conflict never detected. Deduplication belongs
	// to the domain, which can tell a retry from a contradiction.
	handle, err := s.temporal.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   billID,
		UpdateName:   billflow.UpdateAddLineItem,
		WaitForStage: client.WorkflowUpdateStageCompleted,
		Args: []any{billflow.AddLineItemInput{
			ItemID:      itemID,
			CustomerID:  in.CustomerID,
			Amount:      in.Amount,
			Description: in.Description,
		}},
	})
	if err != nil {
		s.writeAddFailure(w, ctx, billID, itemID, in, err)
		return
	}

	var result bill.AddResult
	if err := handle.Get(ctx, &result); err != nil {
		s.writeAddFailure(w, ctx, billID, itemID, in, err)
		return
	}

	location := "/bills/" + billID + "/line-items/" + itemID
	w.Header().Set("Location", location)

	// 201 when the charge was newly accrued, 200 when this was a retry of one the
	// bill already carries.
	status := http.StatusOK
	if result.Outcome == bill.OutcomeCreated {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

// CloseBill freezes a bill's totals and starts the invoice hand-off.
//
// 202, not 200: the totals are final, the bill is not. Closing an already-closed
// bill is not an error - the response reports closedBy, so a request that lost
// the race against the period-end deadline can tell what closed it.
//
//encore:api public raw method=POST path=/bills/:billId/close
func (s *Service) CloseBill(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := temporalContext(req)
	defer cancel()
	billID := pathParam("billId")

	handle, err := s.temporal.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   billID,
		UpdateName:   billflow.UpdateCloseBill,
		WaitForStage: client.WorkflowUpdateStageCompleted,
		Args:         []any{billflow.CloseBillInput{}},
	})
	if err == nil {
		var snap bill.Snapshot
		if err := handle.Get(ctx, &snap); err != nil {
			s.writeUpdateFailure(w, billID, err)
			return
		}
		w.Header().Set("Location", "/bills/"+billID)
		writeJSON(w, http.StatusAccepted, snap)
		return
	}

	// The workflow has already finished, so the bill is closed and its invoice
	// lives in storage. That still satisfies the request.
	if isWorkflowBusy(err) {
		writeUnavailable(w, "the bill could not be reached to close it: "+err.Error())
		return
	}
	if isWorkflowAbsent(err) {
		snap, readErr := loadInvoice(ctx, billID)
		switch {
		case readErr == nil:
			w.Header().Set("Location", "/bills/"+billID)
			writeJSON(w, http.StatusAccepted, snap)
		case errors.Is(readErr, ErrInvoiceNotFound):
			// Neither source has the bill, so it really is absent.
			writeNotFound(w, billID)
		default:
			// Storage failed. Falling through here reported a database outage as a
			// missing bill, which reads as "this never existed" and pages nobody.
			writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal,
				"reading the invoice for bill "+billID+": "+readErr.Error())
		}
		return
	}
	s.writeUpdateFailure(w, billID, err)
}

// GetBill returns a bill in any state: live from the workflow while it is open,
// from the persisted invoice once the workflow has finished.
//
//encore:api public raw method=GET path=/bills/:billId
func (s *Service) GetBill(w http.ResponseWriter, req *http.Request) {
	billID := pathParam("billId")

	ctx, cancel := temporalContext(req)
	defer cancel()

	snap, err := s.readBill(ctx, billID)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvoiceNotFound):
			// Both sources were asked and neither had the bill. This is the only
			// circumstance in which "not found" is true.
			writeNotFound(w, billID)
		case isWorkflowBusy(err):
			writeUnavailable(w, "the bill's state could not be read: "+err.Error())
		default:
			writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// readBill returns a bill from whichever source can answer for it: the running
// workflow first, then durable storage.
func (s *Service) readBill(ctx context.Context, billID string) (bill.Snapshot, error) {
	ev, err := s.temporal.QueryWorkflow(ctx, billID, "", billflow.QueryGetBill)
	switch {
	case err == nil:
		var snap bill.Snapshot
		if decodeErr := ev.Get(&snap); decodeErr != nil {
			// Previously discarded, which turned a malformed answer into a missing
			// bill. A workflow that answers unintelligibly is a fault, not an absence.
			return bill.Snapshot{}, fmt.Errorf("billing: decoding bill %q from its workflow: %w", billID, decodeErr)
		}
		return snap, nil

	case isWorkflowAbsent(err):
		// The only condition under which storage is the right place to look: there
		// is no live execution to ask, so the persisted invoice is all there is.
		return loadInvoice(ctx, billID)

	default:
		// Everything else - unreachable, busy, overloaded, or an outright fault -
		// propagates. Falling back here is what made a Temporal outage look like
		// every open bill having been deleted.
		return bill.Snapshot{}, fmt.Errorf("billing: querying bill %q: %w", billID, err)
	}
}

// writeUpdateFailure reports an update that did not apply.
func (s *Service) writeUpdateFailure(w http.ResponseWriter, billID string, err error) {
	switch {
	case isWorkflowBusy(err):
		writeUnavailable(w, "the bill could not be reached: "+err.Error())
	case isWorkflowAbsent(err):
		writeNotFound(w, billID)
	default:
		writeRejection(w, err)
	}
}

// writeAddFailure answers a charge the workflow could not take, consulting the
// persisted invoice when there is no live execution.
//
// That is the ordinary condition of a closed bill, not an exotic one: treating it
// as a missing bill answered 404 for a charge on a bill GET returns 200 for, and
// a caller told its charge was refused as unknown may compensate for money that
// is genuinely on the invoice.
func (s *Service) writeAddFailure(w http.ResponseWriter, ctx context.Context, billID, itemID string, in AddLineItemRequest, err error) {
	if !isWorkflowAbsent(err) {
		s.writeUpdateFailure(w, billID, err)
		return
	}

	snap, loadErr := loadInvoice(ctx, billID)
	if loadErr != nil {
		if errors.Is(loadErr, ErrInvoiceNotFound) {
			// Neither the workflow nor storage has this bill. Now 404 is true.
			writeNotFound(w, billID)
			return
		}
		writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal,
			"reading the invoice for bill "+billID+": "+loadErr.Error())
		return
	}

	existing, checkErr := bill.CheckAgainstSnapshot(snap, bill.LineItem{
		ID:          itemID,
		Amount:      in.Amount,
		Description: in.Description,
	}, in.CustomerID)
	if checkErr != nil {
		writeProblem(w, statusForDomainError(checkErr), billflow.ClassifyRejection(checkErr), checkErr.Error())
		return
	}

	// The charge is already on the invoice, so the retry succeeded the first time.
	w.Header().Set("Location", "/bills/"+billID+"/line-items/"+itemID)
	writeJSON(w, http.StatusOK, bill.AddResult{
		Outcome:      bill.OutcomeAlreadyAccrued,
		LineItem:     *existing,
		RunningTotal: snap.Total,
	})
}

// statusForDomainError maps a domain error to its status through the same table
// the workflow's rejections use, so storage and the workflow answer identically.
func statusForDomainError(err error) int {
	if status, ok := reasonStatus[billflow.ClassifyRejection(err)]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// pathParam reads a path parameter from the in-flight Encore request.
func pathParam(name string) string {
	if req := encore.CurrentRequest(); req != nil {
		return req.PathParams.Get(name)
	}
	return ""
}

// billIDFor derives a bill id from an idempotency key, or generates one when no
// key was supplied; the second return reports whether the id is reproducible by a
// retry.
func billIDFor(idempotencyKey string) (string, bool) {
	if idempotencyKey == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		return "bill_" + hex.EncodeToString(b[:]), false
	}
	// Hashed so an arbitrary client string cannot become a workflow id with
	// whatever characters and length it happens to carry.
	sum := sha256.Sum256([]byte(idempotencyKey))
	return "bill_" + hex.EncodeToString(sum[:])[:32], true
}

// billIDForCustomer scopes the key to its customer, so it identifies a request
// within one rather than across all of them. Hashed globally, "september-2026"
// named one bill for every caller.
//
// The boundary is encoded, not marked: the identifier is opaque, so no character
// can be reserved as a delimiter, and every naive scheme has a colliding pair:
//
//	customer + key         ("acme","x") and ("acm","ex")   -> "acmex"
//	customer + ":" + key   ("ac:me","x") and ("ac","me:x") -> "ac:me:x"
//
// Length-prefixing the customer removes both: the digest cannot be reached by
// two different splits of the same bytes.
func billIDForCustomer(customerID, idempotencyKey string) (string, bool) {
	if idempotencyKey == "" {
		return billIDFor("")
	}
	scoped := strconv.Itoa(len(customerID)) + ":" + customerID + idempotencyKey
	return billIDFor(scoped)
}

func joinCodes(codes []string) string {
	out := ""
	for i, c := range codes {
		if i > 0 {
			out += ", "
		}
		out += c
	}
	return out
}
