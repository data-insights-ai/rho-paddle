package paddle

import (
	"testing"

	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
)

func TestQuantityBoundsValidationAndMatching(t *testing.T) {
	if !(QuantityBounds{}).valid() || (QuantityBounds{}).Declared() {
		t.Fatal("undeclared bounds are valid and not declared")
	}
	for _, bad := range []QuantityBounds{{Min: 0, Max: 5}, {Min: 2, Max: 1}, {Min: -1, Max: -1}} {
		if bad.valid() {
			t.Errorf("bounds %+v accepted", bad)
		}
	}
	spec := PriceSpec{Key: "k", Quantity: QuantityBounds{Min: 1, Max: 1}}
	if quantityMatches(paddlewire.Price{}, spec) {
		t.Fatal("a price without bounds does not match a declaration")
	}
	if !quantityMatches(paddlewire.Price{Quantity: &paddlewire.Quantity{Minimum: 1, Maximum: 1}}, spec) {
		t.Fatal("equal bounds must match")
	}
	if quantityMatches(paddlewire.Price{Quantity: &paddlewire.Quantity{Minimum: 1, Maximum: 100}}, spec) {
		t.Fatal("the provider default must not match a declaration of 1")
	}
	if !quantityMatches(paddlewire.Price{Quantity: &paddlewire.Quantity{Minimum: 1, Maximum: 100}}, PriceSpec{Key: "k"}) {
		t.Fatal("an undeclared spec matches anything")
	}
	catalog := ProviderCatalog{
		Products: []ProductSpec{{Key: "p", Name: "P", TaxCategory: "saas"}},
		Prices:   []PriceSpec{{Key: "pr", ProductKey: "p", Description: "d", Currency: "EUR", Amount: 100, Tax: TaxExclusive, Quantity: QuantityBounds{Min: 3, Max: 1}}},
	}
	if err := catalog.validate(); err == nil {
		t.Fatal("inverted bounds must fail validation")
	}
}
