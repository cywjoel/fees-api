## Why

A financial platform needs to accrue fees against a customer over a defined billing period and produce an authoritative invoice when that period ends. The hard parts are not the arithmetic: a bill is a long-lived process (a period may run for a month), concurrent line-item writes must not lose or duplicate charges, the close must fire reliably on a wall-clock deadline even across deploys and crashes, and downstream invoice emission must survive transient failure. Modelling the bill as a durable Temporal workflow rather than a row in a table addresses each of these directly.

## What Changes

- **New Fees API service** built with Encore, exposing REST endpoints for the bill lifecycle.
- **One Temporal workflow instance per bill.** The workflow is the authoritative writer for a bill's state while the bill is open. Its single-threaded execution model serialises concurrent line-item writes, removing the need for row locks or optimistic-concurrency retries.
- **Three-state lifecycle:** `OPEN -> CLOSING -> CLOSED`. `CLOSING` exists because closing hands off to invoice emission, which can fail and be retried; the bill's totals are frozen before that hand-off completes.
- **Temporal Updates, not Signals**, for `AddLineItem` and `CloseBill`. Updates return a value and can reject synchronously in a validator, which is what makes the state-integrity requirement (reject additions to a closed bill) expressible as a real HTTP status rather than a silent drop.
- **Two close triggers, one deterministic outcome.** A bill closes either on an explicit API request or when the period-end timer fires. Both run on the workflow's single thread, so whichever occurs first wins and the other becomes a no-op. The response reports `closedBy` so the caller can tell which happened.
- **Idempotency on every mutation.** Bill creation is idempotent via an `Idempotency-Key` mapped to the Temporal workflow ID; line items are idempotent via a client-supplied item ID under `PUT`. Temporal delivers updates at-least-once, so without this a client retry double-charges.
- **Mono-currency bills.** A bill's currency is fixed at creation. Line items in a different currency are rejected. Money is represented as `int64` minor units plus an ISO 4217 code; rate arithmetic uses arbitrary-precision decimals and rounds to minor units at line-item creation.
- **Durable invoices.** On close, an activity writes the final invoice and its line items to an Encore-provisioned Postgres database. A Temporal Query is a view of live process state, not a database; closed bills must remain readable beyond the namespace retention horizon.
- **Test strategy anchored on time skipping and replay**, so a 30-day period is exercised in milliseconds and workflow changes cannot silently break bills that are mid-period.

### Non-goals

- **Invoice production and delivery.** The workflow calls an `EmitInvoice` activity at the `CLOSING -> CLOSED` boundary; its implementation is a stub. The seam is in scope, the invoice document and its transport are not.
- **Reopening a closed bill.** `CLOSED` is terminal. Corrections would be a separate credit-note capability.
- **Listing or searching bills.** Read access is by bill ID only. There is no collection endpoint, no filtering, and therefore no read-model projection for open bills.
- **Cross-currency settlement.** A bill is denominated and payable in one currency. Paying a USD bill in GEL belongs to a settlement context with its own FX rate snapshot, spread, and quote expiry.
- **Payment capture.** Producing the invoice is the terminus of this service.
- **Line-item volume limits.** Bills are assumed to stay within a history size that does not require continue-as-new.

## Capabilities

### New Capabilities

- `billing/bill-lifecycle`: Creating a bill for a fee period, its state machine (`OPEN -> CLOSING -> CLOSED`), the two close triggers and their deterministic resolution, close response contents (total and all line items), and the terminality of `CLOSED`.
- `billing/line-item-accrual`: Adding line items to an open bill, idempotency by client-supplied item ID, immutability of accrued items, rejection when the bill is not `OPEN`, and the running total returned on each addition.
- `billing/money-representation`: Money as `int64` minor units with an ISO 4217 currency, the per-line-item rounding policy, mono-currency bill enforcement, and currency-mismatch rejection.

### Modified Capabilities

None. This is a greenfield service; `openspec/specs/` is currently empty.

## Impact

- **New Encore service** (Go) exposing `POST /bills`, `PUT /bills/{billId}/line-items/{itemId}`, `POST /bills/{billId}/close`, and `GET /bills/{billId}`.
- **New Temporal worker** hosting the bill workflow and its activities. Worker lifecycle must be attached to an Encore service struct, since Temporal is not Encore-provisioned infrastructure — this is the one seam where the two frameworks do not compose automatically, and it affects both runtime wiring and the integration-test setup.
- **New Encore Postgres database** holding immutable closed invoices and their line items. Written only by workflow activities; the API layer reads but never writes, which preserves the workflow as the sole writer.
- **New dependencies:** `go.temporal.io/sdk`, an arbitrary-precision decimal library for rate arithmetic.
- **New local dependency for running and testing:** a Temporal dev server (`temporal server start-dev`) alongside `encore run`.
- **CI:** replay tests require committed workflow-history fixtures under `testdata/`, refreshed when workflow structure changes intentionally.
- **Documentation:** README covering how to run the service, drive the workflow end to end, and observe it in the Temporal Web UI.
