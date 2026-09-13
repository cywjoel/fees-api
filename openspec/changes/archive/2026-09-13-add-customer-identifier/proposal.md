## Why

A bill knows its currency, its fee period, its total and its charges. It does not know **whose** it is. There is no customer, account or tenant identifier anywhere in the model — not on `Snapshot`, not on `LineItem`, not in either table.

That is not a missing feature so much as a hole in the model, and two consequences follow from it.

The first is that the service cannot do the thing it exists to do. `EmitInvoice` hands a finished invoice to the payee, and has no idea who the payee is.

The second is a live defect. A bill's identity is derived from its idempotency key, globally:

```
  sha256("september-2026")  ->  the same bill for every caller
```

Two fee engines, or one engine acting for two customers, that both choose a natural key like `september-2026` collide: the second caller silently receives the first customer's bill, and accrues charges onto it. This needs no authentication to be wrong, and it is wrong today.

## What Changes

- **A bill carries a customer identifier**, supplied at creation, required, reported on every retrieval, and persisted with the invoice.
- **Idempotency keys are scoped to the customer.** A key identifies a request *within* a customer rather than across all of them, so two callers choosing the same key string for different customers no longer collide.
- **A line item may state the customer it believes it is charging.** When present it must match the bill, otherwise the addition is rejected. This is a checksum, not an identity claim: a fee engine that computes the wrong bill id currently charges the wrong customer in silence, and this turns that into an error. It mirrors the currency check the API already performs for exactly the same reason.

### Decisions taken

- **The identifier is opaque to this service.** It is a string the caller supplies and this system stores, compares and reports. There is no customer entity, no validation against a registry, no lifecycle. Anything more belongs to whatever system owns customers.
- **It is required, not optional.** An optional owner produces bills that cannot be invoiced, and a later migration to make it required has to invent owners for the bills created without one.

### Non-goals

- **No authorization.** Every caller is a trusted system and authentication remains upstream, as the archived design assumes. Adding the identifier does not make this service decide who may read what.
- **No customer portal, and therefore no list or search.** A system that creates a bill receives its identifier and retains it; nothing needs to discover bills by customer. This was the portal's requirement and the portal is out of scope.
- **No derived bill identifier.** Deriving the id from `(customer, currency, period)` would make the invariant "one open bill per customer per period" free from Temporal's workflow-id uniqueness, but it requires canonical period bounds, and periods here are caller-supplied arbitrary instants that a caller cannot reliably reproduce. That invariant stays a convention the caller upholds through a stable key.
- **No second API surface.** One kind of caller, one surface. The internal/external split only pays for itself when an end user can call the API directly.
- **No customer capability of its own.** Currency defaults, tenancy and authorization would justify one; none of them are in scope, so this is a field on a bill.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `billing/bill-lifecycle`: creation requires a customer identifier and records it; retrieval reports it; an idempotency key is interpreted within a customer rather than globally.
- `billing/line-item-accrual`: an addition may state the customer it intends to charge, and is rejected when that disagrees with the bill.

`billing/money-representation` is unchanged.

## Impact

- **`internal/bill`** — `Snapshot` and the aggregate gain the identifier; `New` requires it.
- **`internal/billflow`** — `StartBillInput` carries it. This changes the workflow's *input*, not its command sequence, so the committed replay fixtures must still replay: histories recorded before the field existed decode it as empty, which is the migration question below.
- **`billing/api.go`** — creation accepts and validates it; the idempotency key is hashed with it; the line item handler compares it when present.
- **`billing/db.go` and a new migration** — a `customer_id` column on `invoice`, and on `invoice_line_item` only if the checksum is worth persisting.
- **Existing bills** — every bill created before this change has no customer. Whether that is a backfill, a nullable column, or an accepted discontinuity is the one real migration decision, and it is not yet made.
- **No change** to the endpoint set, paths, verbs, status codes, or money representation.
