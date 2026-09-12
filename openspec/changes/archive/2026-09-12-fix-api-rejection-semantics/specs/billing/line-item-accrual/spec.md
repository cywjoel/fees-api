## MODIFIED Requirements

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
