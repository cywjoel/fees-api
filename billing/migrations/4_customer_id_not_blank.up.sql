-- "Not empty" is not the same as "has an owner".
--
-- The previous constraint rejected only the empty string, so a customer
-- identifier consisting entirely of whitespace satisfied it: ' ' <> '' is true.
-- A caller whose upstream trims badly, or emits an absent field as a space,
-- created bills owned by nothing, and they closed and persisted normally.
--
-- The identifier stays opaque. This requires that it contain at least one
-- non-whitespace character; it does not constrain what those characters are,
-- and it does not alter what is stored. An identifier that meaningfully carries
-- surrounding whitespace is kept exactly as given.
ALTER TABLE invoice DROP CONSTRAINT invoice_customer_id_not_empty;

ALTER TABLE invoice ADD CONSTRAINT invoice_customer_id_not_blank
    CHECK (customer_id ~ '[^[:space:]]');
