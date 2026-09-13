## MODIFIED Requirements

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
