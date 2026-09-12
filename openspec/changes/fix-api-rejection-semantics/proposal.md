## Why

A code review of the initial implementation found ten defects, eight of them reproduced against a running service. They share one root cause: **the API cannot distinguish "Temporal has no answer right now" from "this bill does not exist"**, and answers `404` for both.

The most serious consequence is that the brief's headline requirement is not met. `openspec/specs/billing/line-item-accrual` requires `409 Conflict` when a charge is offered to a closed bill. The service returns `409` only during the brief `CLOSING` window; once the workflow completes — the state every bill reaches within seconds and stays in forever — it returns `404 bill_not_found`. Observed on one bill at one instant:

```
GET  /bills/{id}          -> 200  state=CLOSED total=5.00 items=1
PUT  .../line-items/late  -> 404  "no bill with id bill_42eb..."
POST .../close            -> 202
```

Three endpoints, three different answers to "does this bill exist". A client told `404` may reasonably conclude its charge was never recorded.

The existing test for this asserts the correct `409`, but adds its late charge immediately after closing, landing in the one window where the behaviour is right — and the end-to-end suite is skipped in CI regardless. The defect was invisible from both directions.

## What Changes

Five clusters. Most restore behaviour the archived specs already require; two change what is required.

- **Separate "absent" from "unavailable" (findings 1, 3, 8, 9, 11).** `isWorkflowGone` currently answers true for `NotFound`, `WorkflowNotReady` and `WorkflowExecutionAlreadyStarted` — three unrelated conditions, only the first of which means the bill is gone. Split them, and narrow `readBill`'s fallback to storage so a transport failure is no longer read as a missing bill.
- **Give `AddLineItem` the storage fallback `CloseBill` already has (finding 11).** A completed workflow must be answered from the persisted invoice: `409` for a new charge, `200` for a retry of one already invoiced.
- **Validate the line item amount before dispatch (finding 2).** A body with no `amount` currently marshals to an empty currency and fails deserialisation on the worker, producing `500` where the spec requires `422`.
- **Fix creation idempotency (findings 5, 6).** Concurrent requests sharing a key all return `201`; and a key reused with different parameters silently returns the original bill.
- **Reject a fee period that has already ended (finding 7).** Currently accepted, producing `201 Created` carrying `state: CLOSING`.
- **Propagate a failed close instead of reporting success (finding 10).** Latent today; `beginClose` logs the error and returns an `OPEN` snapshot as a successful `202`.

### Decisions taken

- A read that cannot reach Temporal returns **`503` with `Retry-After`**, not `404` and not `500`. A caller must be able to tell a retryable outage from a bill that does not exist.
- A fee period whose `periodEnd` is in the past is **rejected with `422`**. Backfilling a historical period is not supported through this endpoint.
- Creation idempotency needs **no fingerprint store**. `CreateBillRequest` is exactly `{currency, periodStart, periodEnd}`, and all three are carried on the bill snapshot, so a reused key is checked by comparing against the bill itself. This preserves the archived design's rule that uniqueness comes from Temporal rather than from a deduplication table.

### Non-goals

- **No change to the workflow's command sequence.** Every fix is in the API layer or the domain; `internal/billflow` changes only where `beginClose` propagates an error. The committed replay fixtures must continue to pass unmodified.
- **No new persistence.** No migration, no new table, no new column.
- **No change to money representation.** `internal/money` is untouched except for an optional marshal guard.
- **No listing or search**, no reopening, no cross-currency settlement — all remain out of scope as archived.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `billing/bill-lifecycle`: creation must reject a period that has already ended; creation idempotency is scoped to requests whose parameters match the existing bill, with a conflict otherwise; retrieval must distinguish a bill that does not exist from one that cannot currently be reached.
- `billing/line-item-accrual`: the rejection of a charge onto a closed bill must hold once the bill's workflow has completed, not only while it is running.

`billing/money-representation` is unchanged: the `422` for a malformed amount is already required there, and the fix restores compliance rather than altering the contract.

## Impact

- **`billing/errors.go`** — `isWorkflowGone` split into distinct predicates; a transient class added to the reason/status tables.
- **`billing/api.go`** — error handling in all four endpoints; storage fallback in `AddLineItem`; conflict policy and parameter comparison in `CreateBill`; amount validation; period validation.
- **`internal/bill`** — a small helper so the line-item deduplication rule can be applied to a persisted snapshot without being reimplemented against it.
- **`internal/billflow/workflow.go`** — `beginClose` returns its error. No change to the command sequence.
- **Tests** — two red tests already written (`billing/errors_test.go`, and the wait-for-`CLOSED` fix in `e2e/api_test.go:325`) must go green; new cases for the transient class, the storage fallback, parameter mismatch, and the past period.
- **CI** — the end-to-end suite is currently skipped for want of `FEES_API_BASE_URL`, so it guards nothing. Wiring it up is tracked here but may be split out.
- **No migration. No dependency change. No change to the public path structure.**
