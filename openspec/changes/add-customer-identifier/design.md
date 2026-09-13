## Context

See `proposal.md` — Why.

Two things shape every decision below.

**The workflow input is versioned by history, not by code.** A bill's workflow may run for a month, and Temporal reconstructs a running one by replaying its recorded history through whatever code is deployed now. `StartBillInput` is that history's first event. Changing it is therefore not an ordinary struct change — histories recorded before the field existed will be replayed through code that expects it.

**The identity of a bill is derived, not assigned.** `billIDFor` hashes the idempotency key into the bill id, which is also the Temporal workflow id. Scoping keys to a customer changes how a bill is named, so every bill created after this change has an id that could not have been produced before it.

## Goals / Non-Goals

**Goals:**

- Give a bill an owner, so an invoice has a payee.
- Stop two callers acting for different customers colliding on one idempotency key.
- Leave the workflow's command sequence untouched, so bills open across the deploy resume.
- Catch a fee engine charging the wrong bill, rather than discovering it on an invoice.

**Non-Goals** (design-level; see `proposal.md` — Non-goals for scope):

- No authorization, no customer registry, no list endpoint, no second surface.
- No change to money representation, endpoints, verbs, or status codes.

## Decisions

### 1. The identifier is an opaque string, required at creation

The service stores, compares and reports it, and interprets nothing. No format, no registry lookup, no customer entity.

*Why opaque:* whatever system owns customers is the authority on which exist. A validation rule here would be a second, weaker copy of that authority, wrong whenever the two disagree, and a coupling that buys nothing — this service never needs to know what a customer *is*, only whether two references are the same one.

*Why required rather than optional:* an optional owner produces bills that cannot be invoiced, and defers the problem to a migration that then has to invent owners for them. The cost of requiring it is paid once, at the boundary; the cost of not requiring it compounds.

### 2. Idempotency keys are scoped by hashing the customer in

```
  today       sha256(key)                 -> bill id
  proposed    sha256(customer + sep + key) -> bill id
```

No new storage, no lookup table. The scoping falls out of the existing derivation, and the archived design's rule — uniqueness comes from Temporal rather than from a deduplication table — survives untouched.

The boundary between the two fields must be encoded, not marked. Because the identifier is opaque (decision 1) no character can be reserved as a delimiter, and every naive scheme has a colliding pair:

```
  customer + key           ("acme","x") and ("acm","ex")   -> "acmex"
  customer + ":" + key     ("ac:me","x") and ("ac","me:x") -> "ac:me:x"
  len + ":" + customer+key  5:ac:mex   2:acme:x   4:acmex   3:acmex
```

So hash `len(customer) + ":" + customer + key`, or write the two fields as separate updates into the digest. This is a small detail that silently reintroduces the exact defect being fixed if it is skipped, and a fix that merely swaps concatenation for a separator does not escape it.

*Alternative considered — a `(customer, key)` table.* Rejected: it adds a store to keep consistent with the workflow, which is the dual-write the architecture avoids everywhere else.

### 3. The workflow stays lenient; the API validates

This is the decision that protects bills that are open across the deploy, and it is not obvious.

Adding a field to `StartBillInput` is safe. **Reacting to its absence with a different branch is not.** An old history decodes into the new struct with `CustomerID: ""` — legally, silently — and then:

```
        decode old input into the NEW struct
                        |
                        v
       StartBillInput{ ..., CustomerID: "" }
                        |
                        v
            b, err := bill.New(..., customerID)
                        |
          +-------------+-------------+
          |                           |
  New REQUIRES a customer     New TOLERATES empty
          |                           |
          v                           v
  err != nil                  proceeds normally
  return immediately                  |
          |                           v
          v                  NewTimer        -> StartTimer
  NO commands issued         ExecuteActivity -> ScheduleActivityTask
          |                           |
          v                           v
  commands: []                commands: [StartTimer, Schedule.., ..]
  history:  [StartTimer, ..]  history:   [StartTimer, Schedule.., ..]
          |                           |
          v                           v
  *** NonDeterministicError ***   replay OK
```

So `bill.New` must not gain a required-customer check. The requirement lives in the API layer, beside the currency and period validation that is already there, where it runs once per request and never on replay.

*Alternative considered — `workflow.GetVersion`.* Correct, and the right tool when a workflow's own logic must change: old histories return `DefaultVersion` and take the old branch, new executions take the new one. Rejected here because it is unnecessary. Versioning exists to let a workflow branch differently for old and new executions; if the workflow never branches on the customer at all, there is nothing to version, and a `GetVersion` marker is permanent complexity in the workflow for a decision the API already made.

*Alternative considered — re-recording the fixtures.* Not an option, and the replay test says so in its own failure message. It turns CI green while leaving every in-flight bill just as broken; the fixtures are the canary, not the patient.

*The consequence to accept:* the workflow will carry an empty customer for bills started before this change, for as long as those bills run. That is correct — it is what those bills actually had — and it is why the storage column cannot be `NOT NULL` without a decision about them (see Migration Plan).

### 4. The line item customer is a checksum, not an identity

Optional on the request; when present it must equal the bill's, else `422`.

The caller already chose the bill by id, so the field tells the system nothing about who is being charged — only whether the caller and the bill agree. A fee engine that computes the wrong bill id currently charges the wrong customer silently, and the error surfaces when a human reads an invoice.

This is the same argument, and the same status code, as the currency check the API already performs. It is deliberately not required: making it mandatory would force every caller to carry the customer alongside the bill id for a guarantee only some of them need.

### 5. Storage

`invoice` gains `customer_id TEXT`. `invoice_line_item` does not: the item's customer is the bill's, and duplicating it invites the two to disagree, which is the defect the checksum exists to catch rather than to enshrine.

Nullability is decided by the migration, not here.

## Risks / Trade-offs

- **The hash separator.** Concatenating customer and key without encoding a boundary makes `("ac","me:x")` and `("acme","x")` the same bill — reintroducing the collision this change exists to fix, in a form that is harder to spot. → Length-prefix the customer, or write the fields separately into the digest, and test the adjacent-boundary case explicitly.

- **Every new bill id is unlike every old one.** Ids are opaque so nothing should care, but anything that memorised a bill id derived from an unscoped key will not find it again by recomputing. → Nothing in this repo recomputes ids; an external caller that does must be told.

- **A required field at the boundary rejects callers that have not been updated.** There is no grace period: the first request without a customer gets `422`. → Acceptable for a service with a small, known set of callers; would need a deprecation window otherwise.

- **The lenient workflow means an empty customer is representable in the domain.** A bill can exist, in memory and in history, with no owner. → Only for bills that predate the change, and the API refuses to create new ones. The alternative is worse: a required check inside the workflow breaks replay.

- **The checksum is optional, so the guarantee is opt-in.** A caller that omits it keeps the current silent-mischarge behaviour. → Deliberate; see decision 4.

## Migration Plan

Deployment is a rolling replacement with no data migration for the workflow, because the command sequence is unchanged (decision 3). `TestReplayCommittedHistories` is the pre-deploy gate, and it must pass **without the fixtures being re-recorded**.

The column is the open part:

```
  bills created before    ->  no customer.  143 closed invoices exist
                              in the dev database today with no owner.
  bills created after     ->  customer required.
```

**Decided: accept the discontinuity and reset the store.** Every row in the database today is test residue — 143 closed invoices produced by the acceptance walkthrough, the end-to-end suite and the review reproductions. None of it is billing data, so there is nothing to preserve and no owner to invent.

That makes `customer_id NOT NULL` available immediately, which is the reason to prefer it: the alternatives both leave "an invoice with no payee" representable in the schema forever, and a nullable column is a permanent accommodation for a situation that lasts only as long as this decision is deferred.

Rejected, and why:

- **Nullable column.** Honest about bills that genuinely had no owner, but every reader handles absence forever in exchange for rows nobody wants to keep.
- **Backfill a sentinel.** Records something that was never true, and a sentinel customer tends to outlive the migration that introduced it.

This option is available *only* because no real data exists yet. It stops being available the first time this service bills anyone, and the migration then becomes a genuine one.

Rollback is a redeploy of the previous binary. Bills created under the new scheme keep their ids and remain readable; the only loss is that their idempotency keys revert to global scoping, so a subsequent retry could collide again.

## Open Questions

- Whether the checksum should later become required once every caller carries the customer anyway. Deferrable: tightening an optional field is a smaller change than loosening a required one.
