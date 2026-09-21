package paddle

import (
	"math"
	"slices"
	"testing"

	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestCollectionTaxAllocationPreservesCommercialBasis(t *testing.T) {
	for _, treatment := range []purchase.TaxTreatment{purchase.TaxInclusive, purchase.TaxExclusive} {
		t.Run(string(treatment), func(t *testing.T) {
			quote := purchase.Quote{TaxTreatment: treatment, Lines: []purchase.QuoteLine{
				{QuoteLineInput: purchase.QuoteLineInput{ID: "a", Quantity: 1}, Amount: 125},
				{QuoteLineInput: purchase.QuoteLineInput{ID: "b", Quantity: 2}, Amount: 250},
			}}
			line := purchase.CollectionLine{Quantity: 3, Allocations: []purchase.CollectionAllocation{{QuoteLineID: "b", Quantity: 2}, {QuoteLineID: "a", Quantity: 1}}}
			gross := int64(375)
			want := []purchase.PaidLine{{LineID: "a", Gross: 125, Tax: 10}, {LineID: "b", Gross: 250, Tax: 21}}
			if treatment == purchase.TaxExclusive {
				gross = 406
				want[0].Gross = 135
				want[1].Gross = 271
			}
			got, err := allocateCollectionMoney(line, quote, quoteLineIndex(quote), gross, 31, 0)
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("allocation=%+v err=%v want=%+v", got, err, want)
			}
			slices.Reverse(line.Allocations)
			again, err := allocateCollectionMoney(line, quote, quoteLineIndex(quote), gross, 31, 0)
			if err != nil || !slices.Equal(again, want) {
				t.Fatalf("reordered=%+v err=%v", again, err)
			}
		})
	}
}

func TestCollectionTaxAllocationUsesExactLargeProductsAndStableTies(t *testing.T) {
	quote := purchase.Quote{TaxTreatment: purchase.TaxInclusive, Lines: []purchase.QuoteLine{
		{QuoteLineInput: purchase.QuoteLineInput{ID: "a", Quantity: 1}, Amount: math.MaxInt64 / 2},
		{QuoteLineInput: purchase.QuoteLineInput{ID: "b", Quantity: 1}, Amount: math.MaxInt64 / 2},
	}}
	line := purchase.CollectionLine{Quantity: 2, Allocations: []purchase.CollectionAllocation{{QuoteLineID: "b", Quantity: 1}, {QuoteLineID: "a", Quantity: 1}}}
	got, err := allocateCollectionMoney(line, quote, quoteLineIndex(quote), math.MaxInt64-1, math.MaxInt64-2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Tax != math.MaxInt64/2 || got[1].Tax != math.MaxInt64/2-1 {
		t.Fatalf("large exact/tied allocation=%+v", got)
	}
	if _, err := allocateCollectionMoney(line, quote, quoteLineIndex(quote), math.MaxInt64-2, 1, 0); err == nil {
		t.Fatal("changed commercial total accepted")
	}
}
