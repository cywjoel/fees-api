package billing

import "testing"

// A bill's identity is derived from its idempotency key. Derived globally, the
// key means the same bill for every caller, so two fee engines acting for
// different customers that both choose a natural key - "september-2026" - are
// handed the same bill, and the second accrues its customer's charges onto the
// first customer's invoice. No authentication is needed for that to be wrong.
func TestIdempotencyKeyIsScopedToTheCustomer(t *testing.T) {
	acme, _ := billIDForCustomer("acme", "september-2026")
	globex, _ := billIDForCustomer("globex", "september-2026")

	if acme == globex {
		t.Errorf("the same key for two customers produced one bill id (%s)\n\n"+
			"Whichever caller arrives second receives the other customer's bill and "+
			"accrues charges onto it.", acme)
	}
}

// Scoping by concatenation reintroduces the same defect in a form that is harder
// to see: a pair that spans the join identically hashes to one bill. Because the
// customer identifier is opaque, no character can be reserved as a delimiter, so
// the boundary has to be encoded rather than marked.
//
// Each pair below collides under one naive scheme and not the other, so a fix
// that merely swaps concatenation for a separator still fails here.
func TestCustomerScopingEncodesTheBoundary(t *testing.T) {
	tests := []struct {
		name            string
		aCustomer, aKey string
		bCustomer, bKey string
		collidesUnder   string
	}{
		{
			name: "plain concatenation", collidesUnder: `customer + key`,
			aCustomer: "acme", aKey: "x",
			bCustomer: "acm", bKey: "ex", // both -> "acmex"
		},
		{
			name: "colon separator", collidesUnder: `customer + ":" + key`,
			aCustomer: "ac:me", aKey: "x",
			bCustomer: "ac", bKey: "me:x", // both -> "ac:me:x"
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := billIDForCustomer(tc.aCustomer, tc.aKey)
			b, _ := billIDForCustomer(tc.bCustomer, tc.bKey)
			if a == b {
				t.Errorf("(%q, %q) and (%q, %q) produced one bill id (%s)\n\n"+
					"These collide under %s. Length-prefix the customer, or write the "+
					"fields separately into the digest.",
					tc.aCustomer, tc.aKey, tc.bCustomer, tc.bKey, a, tc.collidesUnder)
			}
		})
	}
}

// The same customer and key must keep producing the same bill, or retries stop
// being retries.
func TestSameCustomerAndKeyIsStable(t *testing.T) {
	first, keyed := billIDForCustomer("acme", "september-2026")
	second, _ := billIDForCustomer("acme", "september-2026")

	if first != second {
		t.Errorf("the same customer and key produced %s then %s", first, second)
	}
	if !keyed {
		t.Error("a supplied key must report as keyed, or the idempotent path is skipped")
	}
}

// Without a key the id is random, so a bill is created per request.
func TestNoKeyYieldsDistinctUnkeyedBills(t *testing.T) {
	first, keyed := billIDForCustomer("acme", "")
	second, _ := billIDForCustomer("acme", "")

	if keyed {
		t.Error("an absent key reported as keyed")
	}
	if first == second {
		t.Errorf("two unkeyed creations produced the same id (%s)", first)
	}
}

// Scoping the key changed how every keyed bill id is derived, so a retry issued
// before the change and repeated afterwards would look for an id its original
// request never used - and a second bill would be created for a period already
// being billed.
//
// No such bill exists: the customer field has only ever existed on this change,
// the store was reset when it was introduced, and the constraint on customer_id
// forbids one being created now. This records the arithmetic that makes the
// hazard real, so that anyone deploying this over a version that HAS been
// running recognises what they are looking at. See design.md, Risks.
func TestLegacyAndScopedIdsDiffer(t *testing.T) {
	legacy, _ := billIDFor("september-2026")
	scoped, _ := billIDForCustomer("acme", "september-2026")

	if legacy == scoped {
		t.Fatal("the derivations coincide; this test no longer describes the hazard")
	}
	t.Logf("a retry spanning the change looks for %s, its bill is at %s", scoped, legacy)
}
