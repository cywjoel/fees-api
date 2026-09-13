-- NOT NULL does not mean "has an owner": an empty string is not NULL.
--
-- The previous migration chose NOT NULL over a nullable column specifically so
-- that "an invoice with no payee" would stop being representable. It did not
-- achieve that. A bill started before bills had customers replays with an empty
-- one - bill.New tolerates that deliberately, to protect replay - and when it
-- closes, the empty string satisfies NOT NULL and a permanently ownerless
-- invoice is written.
--
-- The consequence of this constraint is deliberate and worth stating: such a
-- bill can no longer be persisted, so its close fails loudly and visibly instead
-- of quietly recording an invoice nobody can be billed for. Failing is the
-- better of the two, and it is available here only because no such bill exists -
-- the store was reset when customer_id was introduced.
ALTER TABLE invoice ADD CONSTRAINT invoice_customer_id_not_empty
    CHECK (customer_id <> '');
