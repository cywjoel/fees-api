package money_test

import (
	"errors"
	"reflect"
	"testing"

	"fees-api/internal/money"
)

func TestLookupSupportedCurrencies(t *testing.T) {
	tests := []struct {
		name         string
		code         string
		wantCode     string
		wantExponent int32
	}{
		{name: "USD", code: "USD", wantCode: "USD", wantExponent: 2},
		{name: "GEL", code: "GEL", wantCode: "GEL", wantExponent: 2},
		{name: "lowercase is normalised", code: "usd", wantCode: "USD", wantExponent: 2},
		{name: "surrounding space is trimmed", code: "  GEL ", wantCode: "GEL", wantExponent: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := money.Lookup(tc.code)
			if err != nil {
				t.Fatalf("Lookup(%q) returned error: %v", tc.code, err)
			}
			if got.Code != tc.wantCode {
				t.Errorf("Lookup(%q).Code = %q, want %q", tc.code, got.Code, tc.wantCode)
			}
			if got.Exponent != tc.wantExponent {
				t.Errorf("Lookup(%q).Exponent = %d, want %d", tc.code, got.Exponent, tc.wantExponent)
			}
		})
	}
}

// Spec: billing/money-representation - "Unsupported currency is rejected".
func TestLookupRejectsUnsupportedCurrency(t *testing.T) {
	for _, code := range []string{"EUR", "GBP", "JPY", "BTC", "", "US", "USDD"} {
		t.Run(code, func(t *testing.T) {
			_, err := money.Lookup(code)
			if !errors.Is(err, money.ErrUnsupportedCurrency) {
				t.Fatalf("Lookup(%q) error = %v, want ErrUnsupportedCurrency", code, err)
			}
		})
	}
}

func TestSupportedIsExactlyGELAndUSD(t *testing.T) {
	got := money.Supported()
	want := []string{"GEL", "USD"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Supported() = %v, want %v", got, want)
	}
}
