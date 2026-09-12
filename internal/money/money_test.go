package money_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"fees-api/internal/money"
)

var (
	usd = money.MustLookup("USD")
	gel = money.MustLookup("GEL")
)

// Spec: billing/money-representation - "Monetary amounts are exact".
//
// The guarantee is structural, not incidental: no field reachable from Money may
// be a floating-point type, so no amount can pick up binary rounding error no
// matter what path it travels.
func TestMoneyContainsNoFloatingPointField(t *testing.T) {
	var seen = map[reflect.Type]bool{}

	var walk func(t *testing.T, typ reflect.Type, path string)
	walk = func(t *testing.T, typ reflect.Type, path string) {
		if seen[typ] {
			return
		}
		seen[typ] = true

		switch typ.Kind() {
		case reflect.Float32, reflect.Float64:
			t.Errorf("%s is %s: monetary values must never be floating point", path, typ.Kind())
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				walk(t, f.Type, path+"."+f.Name)
			}
		case reflect.Ptr, reflect.Slice, reflect.Array:
			walk(t, typ.Elem(), path+"[]")
		case reflect.Map:
			walk(t, typ.Key(), path+"[key]")
			walk(t, typ.Elem(), path+"[value]")
		}
	}

	walk(t, reflect.TypeOf(money.Money{}), "Money")
}

// Spec: billing/money-representation - "Amount is expressed with its currency".
//
// The major-unit amount crosses the wire as a JSON string. Were it a JSON
// number, a consumer's decoder would be free to parse it into a float64, which
// is precisely the loss this package prevents.
func TestMoneyJSONCarriesAmountAsStringNotNumber(t *testing.T) {
	b, err := json.Marshal(money.New(1255, usd))
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("Unmarshal into fields returned error: %v", err)
	}

	amount, ok := fields["amount"]
	if !ok {
		t.Fatalf("encoded money %s has no amount field", b)
	}
	if !strings.HasPrefix(string(amount), `"`) {
		t.Errorf("amount encoded as %s, want a JSON string so consumers cannot parse it as a float", amount)
	}
	if _, ok := fields["currency"]; !ok {
		t.Errorf("encoded money %s has no currency field; an amount without its currency is meaningless", b)
	}
	if _, ok := fields["minorUnits"]; !ok {
		t.Errorf("encoded money %s has no minorUnits field", b)
	}
}

// Spec: billing/money-representation - "Repeated addition does not accumulate
// error". The canonical float trap: 0.1 added a hundred times.
func TestSumOfOneHundredDimesIsExactlyTenDollars(t *testing.T) {
	dimes := make([]money.Money, 100)
	for i := range dimes {
		dimes[i] = money.New(10, usd) // 0.10 USD
	}

	total, err := money.Sum(usd, dimes...)
	if err != nil {
		t.Fatalf("Sum returned error: %v", err)
	}
	if got, want := total.MinorUnits(), int64(1000); got != want {
		t.Fatalf("Sum of 100 x 0.10 USD = %d minor units, want %d", got, want)
	}
	if got, want := total.String(), "10.00 USD"; got != want {
		t.Errorf("Sum of 100 x 0.10 USD = %q, want %q", got, want)
	}
}

func TestSumOfNothingIsZeroInTheGivenCurrency(t *testing.T) {
	total, err := money.Sum(usd)
	if err != nil {
		t.Fatalf("Sum returned error: %v", err)
	}
	if !total.IsZero() {
		t.Errorf("Sum() = %s, want zero", total)
	}
	if total.Currency().Code != "USD" {
		t.Errorf("Sum() currency = %q, want USD", total.Currency().Code)
	}
}

// Spec: billing/money-representation - "A bill is denominated in exactly one
// currency". Arithmetic across currencies is an error, never an implicit
// conversion.
func TestAddRejectsCurrencyMismatch(t *testing.T) {
	_, err := money.New(1000, usd).Add(money.New(1000, gel))
	if !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("USD.Add(GEL) error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestSumRejectsCurrencyMismatch(t *testing.T) {
	_, err := money.Sum(usd, money.New(100, usd), money.New(100, gel))
	if !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("Sum(USD, USD, GEL) error = %v, want ErrCurrencyMismatch", err)
	}
}

// Spec: billing/money-representation - "Line item amounts are whole minor units"
// and "Fee falling exactly halfway rounds away from zero".
func TestFromMinorUnitsDecimalRoundsHalfUp(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int64
	}{
		{name: "exact value is untouched", input: "4", want: 4},
		{name: "below the tie rounds down", input: "4.4", want: 4},
		{name: "just below the tie rounds down", input: "4.499999", want: 4},
		{name: "the tie rounds away from zero", input: "4.5", want: 5},
		{name: "just above the tie rounds up", input: "4.500001", want: 5},
		{name: "above the tie rounds up", input: "4.6", want: 5},
		{name: "negative below the tie rounds toward zero", input: "-4.4", want: -4},
		{name: "negative tie rounds away from zero", input: "-4.5", want: -5},
		{name: "negative above the tie rounds away from zero", input: "-4.6", want: -5},
		{name: "the tie at an even value still rounds up, unlike half-even", input: "2.5", want: 3},
		{name: "the tie at an odd value rounds up", input: "3.5", want: 4},
		{name: "zero", input: "0", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := decimal.RequireFromString(tc.input)
			got := money.FromMinorUnitsDecimal(d, usd)
			if got.MinorUnits() != tc.want {
				t.Errorf("FromMinorUnitsDecimal(%s) = %d minor units, want %d", tc.input, got.MinorUnits(), tc.want)
			}
			if got.Currency().Code != "USD" {
				t.Errorf("FromMinorUnitsDecimal(%s) currency = %q, want USD", tc.input, got.Currency().Code)
			}
		})
	}
}

// Half-even would round 2.5 to 2 and 4.5 to 4. Asserting the difference pins the
// policy so a later switch of rounding mode cannot pass unnoticed.
func TestRoundingIsHalfUpNotHalfEven(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{in: "2.5", want: 3},
		{in: "4.5", want: 5},
	} {
		got := money.FromMinorUnitsDecimal(decimal.RequireFromString(tc.in), usd).MinorUnits()
		if got != tc.want {
			t.Errorf("FromMinorUnitsDecimal(%s) = %d, want %d (half-up); half-even would give %d",
				tc.in, got, tc.want, tc.want-1)
		}
	}
}

func TestFromMajorUnitsDecimalRoundsToMinorUnits(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int64
	}{
		{name: "exact major units", input: "12.55", want: 1255},
		{name: "third decimal below the tie is dropped", input: "12.554", want: 1255},
		{name: "third decimal at the tie rounds up", input: "12.555", want: 1256},
		{name: "third decimal above the tie rounds up", input: "12.556", want: 1256},
		{name: "whole units", input: "12", want: 1200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := money.FromMajorUnitsDecimal(decimal.RequireFromString(tc.input), usd)
			if got.MinorUnits() != tc.want {
				t.Errorf("FromMajorUnitsDecimal(%s) = %d minor units, want %d", tc.input, got.MinorUnits(), tc.want)
			}
		})
	}
}

// A percentage fee is the derivation the rounding policy exists for: the rate
// carries the precision, the product is rounded once, and the rounded value is
// what gets charged.
func TestApplyRateRoundsTheDerivedFeeOnce(t *testing.T) {
	tests := []struct {
		name string
		base money.Money
		rate string
		want int64
	}{
		{name: "0.35% of 12.34 USD is 4.319 minor units, rounded to 4", base: money.New(1234, usd), rate: "0.0035", want: 4},
		{name: "0.45% of 10.00 USD is exactly 4.5 minor units, rounded to 5", base: money.New(1000, usd), rate: "0.0045", want: 5},
		{name: "1% of 100.00 USD is exactly 100 minor units", base: money.New(10000, usd), rate: "0.01", want: 100},
		{name: "a refund fee on a negative base rounds away from zero", base: money.New(-1000, usd), rate: "0.0045", want: -5},
		{name: "a zero rate yields nothing", base: money.New(123456, usd), rate: "0", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := money.ApplyRate(tc.base, decimal.RequireFromString(tc.rate))
			if got.MinorUnits() != tc.want {
				t.Errorf("ApplyRate(%s, %s) = %d minor units, want %d", tc.base, tc.rate, got.MinorUnits(), tc.want)
			}
			if got.Currency().Code != tc.base.Currency().Code {
				t.Errorf("ApplyRate changed currency to %q, want %q", got.Currency().Code, tc.base.Currency().Code)
			}
		})
	}
}

func TestParseExactRejectsExcessPrecision(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		want    int64
	}{
		{name: "two decimals is exact for USD", input: "12.55", want: 1255},
		{name: "one decimal is exact", input: "12.5", want: 1250},
		{name: "no decimals is exact", input: "12", want: 1200},
		{name: "negative is exact", input: "-12.55", want: -1255},
		{name: "three decimals exceeds USD precision", input: "12.555", wantErr: true},
		{name: "many decimals exceeds USD precision", input: "0.123456", wantErr: true},
		{name: "not a number", input: "twelve", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := money.ParseExact(tc.input, usd)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseExact(%q) = %s, want an error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseExact(%q) returned error: %v", tc.input, err)
			}
			if got.MinorUnits() != tc.want {
				t.Errorf("ParseExact(%q) = %d minor units, want %d", tc.input, got.MinorUnits(), tc.want)
			}
		})
	}
}

func TestParseExactPrecisionErrorIsIdentifiable(t *testing.T) {
	_, err := money.ParseExact("12.555", usd)
	if !errors.Is(err, money.ErrPrecisionExceeded) {
		t.Fatalf("ParseExact(\"12.555\") error = %v, want ErrPrecisionExceeded", err)
	}
}

// Spec: billing/money-representation - "Amount round-trips without drift".
func TestMoneyJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   money.Money
	}{
		{name: "a dime, the canonical float trap", in: money.New(10, usd)},
		{name: "zero", in: money.Zero(usd)},
		{name: "a large amount", in: money.New(999999999999, usd)},
		{name: "a negative amount", in: money.New(-1255, gel)},
		{name: "GEL", in: money.New(3393, gel)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal returned error: %v", err)
			}
			var out money.Money
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("Unmarshal(%s) returned error: %v", b, err)
			}
			if !out.Equal(tc.in) {
				t.Errorf("round-trip of %s produced %s (encoded as %s)", tc.in, out, b)
			}
		})
	}
}

func TestMoneyUnmarshalFromAmountAlone(t *testing.T) {
	var m money.Money
	if err := json.Unmarshal([]byte(`{"amount":"0.10","currency":"USD"}`), &m); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if got, want := m.MinorUnits(), int64(10); got != want {
		t.Errorf("minor units = %d, want %d", got, want)
	}
}

func TestMoneyUnmarshalFromMinorUnitsAlone(t *testing.T) {
	var m money.Money
	if err := json.Unmarshal([]byte(`{"minorUnits":10,"currency":"USD"}`), &m); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if got, want := m.MinorUnits(), int64(10); got != want {
		t.Errorf("minor units = %d, want %d", got, want)
	}
}

// Two statements of the same charge that disagree is a defect worth surfacing,
// not something to resolve by field precedence.
func TestMoneyUnmarshalRejectsDisagreeingAmountAndMinorUnits(t *testing.T) {
	var m money.Money
	err := json.Unmarshal([]byte(`{"amount":"0.10","currency":"USD","minorUnits":11}`), &m)
	if err == nil {
		t.Fatalf("Unmarshal of disagreeing amount and minorUnits succeeded as %s, want an error", m)
	}
}

func TestMoneyUnmarshalRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "unsupported currency", in: `{"amount":"1.00","currency":"EUR"}`},
		{name: "missing currency", in: `{"amount":"1.00"}`},
		{name: "no amount at all", in: `{"currency":"USD"}`},
		{name: "excess precision", in: `{"amount":"1.005","currency":"USD"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var m money.Money
			if err := json.Unmarshal([]byte(tc.in), &m); err == nil {
				t.Fatalf("Unmarshal(%s) succeeded as %s, want an error", tc.in, m)
			}
		})
	}
}

func TestStringRendersAmountWithCurrency(t *testing.T) {
	tests := []struct {
		in   money.Money
		want string
	}{
		{in: money.New(1255, usd), want: "12.55 USD"},
		{in: money.New(10, usd), want: "0.10 USD"},
		{in: money.New(0, gel), want: "0.00 GEL"},
		{in: money.New(-1255, gel), want: "-12.55 GEL"},
		{in: money.New(100000000, usd), want: "1000000.00 USD"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A zero Money has no currency, so it denominates nothing. Encoding it produced
// {"amount":"0","currency":"","minorUnits":0}, which this package's own decoder
// rejects - so the value crossed the wire and failed on the far side, where the
// failure looks like the receiver being broken rather than the value being
// malformed.
func TestMarshalRefusesAnAmountWithNoCurrency(t *testing.T) {
	if _, err := json.Marshal(money.Money{}); err == nil {
		t.Fatal("encoding a zero Money succeeded; it must be refused, not sent as an empty currency")
	}

	type lineItem struct {
		ID     string      `json:"id"`
		Amount money.Money `json:"amount"`
	}
	if _, err := json.Marshal(lineItem{ID: "txn_1"}); err == nil {
		t.Error("encoding a struct holding an unset Money succeeded; the defect is that it used to travel")
	}

	if _, err := json.Marshal(money.Zero(usd)); err != nil {
		t.Errorf("a genuine zero amount in a real currency must still encode: %v", err)
	}
}
