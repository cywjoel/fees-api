## Context

See `proposal.md` — Why. The implementation archived as `2026-09-12-fees-api` is in place and its specs are the project's contract; this change makes the code meet that contract and amends it in the two places it was silent.

Ten defects, eight reproduced against a running service. They are not ten independent mistakes. Five of them are one confusion expressed in one predicate:

```
  billing/errors.go:122   isWorkflowGone(err) bool
       |
       +-- NotFound                        -> the execution is absent          CORRECT
       +-- WorkflowNotReady                -> running, momentarily busy        WRONG
       +-- WorkflowExecutionAlreadyStarted -> the execution EXISTS             WRONG
       |
       v
  every true becomes 404 bill_not_found
```

and one function that asks no question at all:

```
  billing/api.go:260   readBill
       ev, err := QueryWorkflow(...)
       if err == nil { return snapshot }
       return loadInvoice(...)        <- any error at all falls through here
```

An open bill has no persisted row until it closes, so `loadInvoice` answers `ErrInvoiceNotFound`, and `GetBill` turns that into `404`. Every failure mode — outage, timeout, unready, completed — arrives at the same wrong answer.

## Goals / Non-Goals

**Goals:**

- Make "this bill does not exist" a conclusion the system reaches deliberately, never a default.
- Make every refusal carry a status the caller can act on: retry, fix the request, or stop.
- Keep the workflow's command sequence byte-identical, so the committed replay fixtures pass unmodified and bills open across the deploy resume cleanly.
- Keep the line-item deduplication rule stated exactly once.

**Non-Goals:**

- No new persistence, migration, or column.
- No change to money representation, the path structure, or the set of endpoints.
- Wiring the end-to-end suite into CI is recorded here but may ship separately.

## Decisions

### 1. One predicate becomes three questions

`isWorkflowGone` is replaced by an explicit classification, because the caller needs to act differently on each:

| Condition | Meaning | Read path | Write path |
|---|---|---|---|
| `NotFound` | execution absent or aged out | try storage | try storage |
| workflow completed | bill exists, is closed | try storage | try storage |
| `WorkflowNotReady`, `Unavailable`, `DeadlineExceeded` | reachable later | `503` + `Retry-After` | `503` + `Retry-After` |
| `WorkflowExecutionAlreadyStarted` | execution exists now | creation's "already exists" path only | never reached |

The last row is why the predicate must be split rather than corrected in place: `CreateBill` genuinely wants `AlreadyStarted` to mean "this key already produced a bill", while `writeUpdateFailure` must never see it as absence. One name cannot carry both.

*Alternative considered — keep one predicate and add a second for transients.* Rejected: it leaves the `AlreadyStarted` conflation in place, which is finding 9 and the trap that would re-break `CreateBill` when its conflict policy changes in decision 4.

### 2. `readBill` reports what actually failed

Storage becomes the fallback for exactly two conditions — absent execution, completed execution — and nothing else. Every other error propagates. The decode error, currently discarded at `api.go:264`, propagates too.

```
  QueryWorkflow
    ok            -> snapshot
    absent/done   -> loadInvoice    -> 200, or 404 only if storage also has nothing
    transient     -> 503 + Retry-After
    other         -> 500
```

`404` then means both sources were asked and neither had the bill, which is the only circumstance in which it is true.

### 3. `AddLineItem` gains the fallback `CloseBill` already has

This is the fix for the headline defect and the only part with real design content.

`CloseBill` already reads storage when the workflow is gone (`api.go:221-232`); `AddLineItem` does not, so it answers `404`. Giving it the same fallback is not merely symmetry — the answer it must produce is state-dependent:

```
  workflow gone -> load the invoice
        |
        +-- item id already on the invoice, same charge  -> 200 with that item
        +-- item id already on the invoice, different    -> 409 line_item_conflict
        +-- item id not on the invoice                   -> 409 bill_not_open
```

The first branch is not an edge case: it is the idempotency guarantee the archived spec makes, and answering `409` there would tell a caller its charge failed when the charge is on the invoice.

**The trap:** `loadInvoice` returns a `bill.Snapshot`, not a `*bill.Bill`, so `AddLineItem` cannot be called on it, and the obvious move is to re-implement `sameChargeAs` against the snapshot. That puts the deduplication rule in two places, where they will drift. Instead the domain gains one function:

```go
// internal/bill
func CheckAgainstSnapshot(snap Snapshot, item LineItem) (*LineItem, error)
```

reusing the same comparison `checkLineItem` uses, so the rule stays stated once. Note that `sameChargeAs` deliberately ignores `AccruedAt`; a storage-path comparison must too, since a retry carries a later timestamp than the stored item.

### 4. Creation idempotency needs no fingerprint store

`CreateBillRequest` is exactly `{currency, periodStart, periodEnd}`, and `bill.Snapshot` carries all three. The whole request is recoverable from the bill, so a reused key is checked by comparing against the bill itself — no dedup table, and the archived design's rule that uniqueness comes from Temporal survives intact.

Two changes to `CreateBill`:

- **Conflict policy `USE_EXISTING` -> `FAIL`.** Under `USE_EXISTING`, a start that attaches to a running execution returns a nil error, so the code cannot tell creating from attaching and every concurrent request reports `201`. Under `FAIL` the loser receives `WorkflowExecutionAlreadyStarted`, which is a definite signal: read the bill, compare parameters, answer `200` or `409`.
- **Parameter comparison** in both the pre-check and the `AlreadyStarted` path, so a race cannot slip past the pre-check.

**Sequenced after decision 1.** Flipping the conflict policy while `isWorkflowGone` still classifies `AlreadyStarted` as absent would route every duplicate creation into the `404` path.

**Compare instants, not representations.** `periodStart` returns from the workflow as UTC, while a client may have sent `+04:00` for the same moment. `==` on `time.Time` compares wall clock, location and monotonic reading, so it would reject a correct retry as a conflict — a false `409` on a valid request, worse than the defect being fixed. Use `.Equal`.

### 5. Validate the amount before it reaches the worker

A body omitting `amount` decodes to the zero `Money`, whose `MarshalJSON` emits `currency: ""` while `UnmarshalJSON` rejects it. The update argument fails to deserialise on the worker, the validator never runs, and the caller gets `500` where `422` is required:

```
  {"description":"fee"}  ->  {"amount":"0","currency":"","minorUnits":0}
                         ->  worker: money: unsupported currency: ""
                         ->  500 internal
```

A guard in the handler fixes the reported defect in four lines. Make `Money.MarshalJSON` reject a zero currency as well, so no half-formed amount can cross any boundary — that is the more principled half, and it has a tail worth knowing: anything marshalling a struct with an unset `Money`, log lines included, begins to error. Audit the marshal sites when taking it.

### 6. Period validation belongs in the API, not the domain

`bill.New` should not know what "now" is; a domain constructor that reads the clock is harder to test and to replay. The check that `periodEnd` is in the future goes in `CreateBill`, beside the existing `periodEnd > periodStart` check.

Clamp the timer duration at `workflow.go:232` regardless. `periodEnd.Sub(workflow.Now(ctx))` is negative for a past period and the timer fires immediately; a defensive clamp costs one line and means validation being bypassed cannot produce a bill that is born closing.

### 7. `beginClose` propagates

`workflow.go:161-174` logs a failed close and returns an `OPEN` snapshot, which the handler returns as a successful `202`. Return the error instead. The handler is already `(bill.Snapshot, error)`, so no signature changes, and returning an error introduces no yield point — `TestUpdateHandlersAreYieldFree` still passes.

## Risks / Trade-offs

- **`503` is a new status in the API's vocabulary.** Clients that treat any non-2xx as fatal will not retry. → It is strictly better than the `404` it replaces, which invited a *wrong* conclusion rather than no conclusion; `Retry-After` makes the contract explicit.

- **The storage fallback in `AddLineItem` adds a database read to a rejection path.** → It runs only when the workflow cannot answer, which for a closed bill is the normal case and already true of `CloseBill`.

- **`CONFLICT_POLICY_FAIL` turns a previously silent attach into an error path.** If the `AlreadyStarted` branch is wrong, duplicate creation breaks loudly. → Loud is the point; the current failure is silent. Covered by the concurrent-creation scenario.

- **Marshal-guarding zero `Money` can break unrelated call sites,** including logging. → Take it as a separate commit from the handler guard so a bisect separates them.

- **The replay fixtures must not need re-recording.** If they do, the workflow's command sequence changed and bills open across the deploy will not resume. → `internal/billflow` changes only in `beginClose`'s error return, which issues no command. `TestReplayCommittedHistories` is the gate; if it fails, the change is wrong, not the fixtures.

- **The end-to-end suite still does not run in CI**, so its assertions guard nothing until wired up. That is how the headline defect survived: a test asserting the correct `409` existed and was never executed. → Tracked in tasks; if it slips, the `billing` unit tests still run on every push.

## Migration Plan

No data migration. Deploy is a normal rolling replacement:

- Workflow code is unchanged in command sequence, so in-flight bills resume against the new binary. `TestReplayCommittedHistories` is the pre-deploy gate.
- The API changes are backward compatible for every request that was previously *succeeding*. Responses change only where they were wrong: `404 -> 409`, `500 -> 422`, `201 -> 200`, `404 -> 503`, and a new `409` for key reuse with different parameters.
- Rollback is a redeploy of the previous binary; no schema or workflow state is altered, so it is safe in both directions.

## Open Questions

- Whether wiring the end-to-end suite into CI ships with this change or separately. It needs a Temporal dev server and a backgrounded `encore run` in the job, with the flakiness that implies, and it does not gate any code fix here.
