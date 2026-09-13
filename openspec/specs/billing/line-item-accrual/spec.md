# billing/line-item-accrual Specification

## Purpose
Defines how fees accrue progressively onto an open bill as individual line items, and the guarantees that keep a charge from being lost, duplicated, or silently altered.

## Requirements

### Requirement: Add a line item to an open bill

The system SHALL add a line item to a bill in `OPEN`. A line item MUST carry a caller-supplied identifier, an amount, a currency, and a description. The system SHALL record the time at which the item was accrued.

A line item MAY additionally state the customer it is intended to charge. Where it does, the system SHALL reject the addition if that customer is not the one the bill belongs to.

This is a checksum rather than an identity claim. The caller already chose the bill by its identifier, so the system does not learn who is being charged from this field; it learns whether the caller and the bill agree. A fee engine that computes the wrong bill identifier otherwise charges the wrong customer in silence, and the charge is discovered only when someone reads an invoice. It is the same reason the system rejects a line item denominated in a currency the bill is not.

#### Scenario: Line item is added successfully

- **WHEN** a client adds a line item with a previously unused identifier to a bill in `OPEN`
- **THEN** the system responds `201 Created` with the stored line item, including its identifier, amount, currency, description, and accrual timestamp

#### Scenario: Line item stating the bill's customer is accepted

- **WHEN** a client adds a line item that states the same customer the bill belongs to
- **THEN** the addition is accepted exactly as one that states no customer at all

#### Scenario: Line item stating a different customer is rejected

- **WHEN** a client adds a line item stating a customer other than the one the bill belongs to
- **THEN** the system responds `422 Unprocessable Content`, no line item is added, and the bill's total is unchanged
- **AND** the rejection identifies the disagreement as the reason, distinctly from a malformed charge or one arriving too late

#### Scenario: Retry of an accrued item naming a different customer is rejected

- **WHEN** a client retries an addition whose item identifier is already on the bill, but states a customer other than the one the bill belongs to
- **THEN** the system responds `422 Unprocessable Content` rather than reporting the charge as already accrued
- **AND** it does so because the assertion is checked before the identifier is looked up: a caller that reached the wrong bill must not be told its charge is present there

#### Scenario: Customer disagreement outranks the bill's state

- **WHEN** a client adds a line item naming a customer other than the one the bill belongs to, to a bill that is no longer `OPEN`
- **THEN** the system responds `422 Unprocessable Content` for the disagreement, not `409 Conflict` for the state
- **AND** this differs from a currency mismatch on the same bill, which is answered `409`, because a wrong currency asks whether the charge suits the bill while a wrong customer asks whether it is the right bill at all

#### Scenario: Line item is missing required detail

- **WHEN** a client adds a line item without an amount, without a currency, or without an identifier
- **THEN** the system responds `422 Unprocessable Content` and no line item is added

### Requirement: Adding a line item reports the running total

The response to a successful line item addition SHALL include the bill's running total after that item was applied, so that progressive accrual is observable without a further request.

#### Scenario: Running total accompanies each addition

- **WHEN** a client adds a line item of `5.00 USD` to a bill in `OPEN` whose running total is `12.55 USD`
- **THEN** the response reports the created line item together with a running total of `17.55 USD`

#### Scenario: First line item on an empty bill

- **WHEN** a client adds a line item of `5.00 USD` to a bill in `OPEN` that has no line items
- **THEN** the response reports a running total of `5.00 USD`

### Requirement: Adding a line item is idempotent by identifier

The caller-supplied line item identifier SHALL be unique within a bill and SHALL act as the deduplication key for the addition. A repeated addition carrying an identifier already present on the bill, with identical detail, SHALL NOT create a second line item and SHALL NOT change the bill's total.

This guarantee SHALL hold regardless of how many times the request is retried, so that a client that retries after a timeout cannot cause a duplicate charge.

#### Scenario: Retried addition does not duplicate a charge

- **WHEN** a client adds a line item and then retries the identical request after a timeout
- **THEN** the system responds `200 OK` with the already-stored line item, the bill carries exactly one line item with that identifier, and the total is unchanged

#### Scenario: Repeated retries remain safe

- **WHEN** an identical line item addition is delivered any number of times
- **THEN** the bill carries exactly one line item with that identifier and its total reflects that item exactly once

#### Scenario: Retry of an accrued item is honoured after the bill closes

- **WHEN** a client retries an identical addition for a line item that was accrued while the bill was open, and the bill has since left `OPEN`
- **THEN** the system responds `200 OK` with the already-accrued line item rather than rejecting it, and the bill's frozen total is unchanged
- **AND** the charge is reported as present because it is genuinely on the invoice, so the client does not compensate for a charge that in fact succeeded

### Requirement: Accrued line items are immutable

Once accrued, a line item SHALL NOT be altered. A request bearing an identifier already present on the bill but carrying different detail SHALL be rejected as a conflict, and SHALL NOT overwrite the stored item or change the bill's total.

#### Scenario: Same identifier with a different amount is rejected

- **WHEN** a client adds a line item whose identifier is already present on the bill but whose amount differs from the stored item
- **THEN** the system responds `409 Conflict`, the stored line item is unchanged, and the bill's total is unchanged

#### Scenario: Same identifier with a different description is rejected

- **WHEN** a client adds a line item whose identifier is already present on the bill but whose description differs from the stored item
- **THEN** the system responds `409 Conflict` and the stored line item is unchanged

### Requirement: Line items are rejected once a bill leaves OPEN

The system SHALL reject any addition that would accrue a **new** line item onto a bill that is not in `OPEN`. Rejection SHALL leave the bill's frozen total and line item collection unchanged.

This applies to a bill in `CLOSING` as well as one in `CLOSED`: once totals are frozen, no further charge may join them.

An identical retry of a line item already accrued while the bill was open is not a new accrual and SHALL NOT be rejected on this basis; see "Adding a line item is idempotent by identifier".

The rejection SHALL report that the bill is not open, and SHALL do so for the whole of a bill's closed life — including after the process holding its live state has finished. A closed bill is not an absent one: if the system answers a retrievable bill's charge with "no such bill", a caller may conclude the charge was never recorded and compensate for money that is genuinely on the invoice.

#### Scenario: Addition to a closed bill is rejected

- **WHEN** a client adds a line item to a bill in `CLOSED`
- **THEN** the system responds `409 Conflict`, no line item is added, and the bill's total is unchanged

#### Scenario: Addition to a closing bill is rejected

- **WHEN** a client adds a line item to a bill in `CLOSING` whose invoice hand-off is still pending
- **THEN** the system responds `409 Conflict`, no line item is added, and the bill's frozen total is unchanged

#### Scenario: Addition racing a close does not join a frozen total

- **WHEN** a line item addition is applied after a bill has entered `CLOSING`, however narrowly
- **THEN** the addition is rejected and the total reported at closure is the same total reported by every later retrieval

#### Scenario: Addition to a bill whose closing process has finished

- **WHEN** a client adds a new line item to a bill in `CLOSED` whose invoice hand-off completed some time ago, so that no live process remains to answer for it
- **THEN** the system responds `409 Conflict` with the same reason it gives while the bill is closing
- **AND** the system SHALL NOT respond `404 Not Found` for a bill that retrieval answers `200 OK` for

#### Scenario: Retry of an invoiced charge after the closing process has finished

- **WHEN** a client retries an identical addition for a line item already on a closed bill's invoice, after no live process remains to answer for it
- **THEN** the system responds `200 OK` with the already-accrued line item, consistent with the idempotency guarantee, and the frozen total is unchanged

### Requirement: Concurrent additions are all accounted for

The system SHALL apply concurrent line item additions to the same open bill without loss. Where several distinct line items are added at the same time, every one of them SHALL appear on the bill and the total SHALL equal the sum of all of them.

#### Scenario: Simultaneous additions are all retained

- **WHEN** several clients concurrently add line items with distinct identifiers to the same bill in `OPEN`
- **THEN** every submitted line item appears on the bill exactly once and the bill's total equals the sum of all of them

### Requirement: Line items are retrievable with their bill

The system SHALL report a bill's line items whenever the bill is retrieved, in both open and closed states, with each item's identifier, amount, currency, description, and accrual time.

#### Scenario: Line items accompany an open bill

- **WHEN** a client retrieves a bill in `OPEN` that has accrued line items
- **THEN** the response lists every accrued line item with its full detail

#### Scenario: Line items accompany a closed bill

- **WHEN** a client retrieves a bill in `CLOSED`
- **THEN** the response lists every line item included in the final total, with the same detail reported at closure
