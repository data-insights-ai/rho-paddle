package paddle

import (
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"math"
	"strconv"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestNormalizeAdjustmentAggregatesComponentsForSingleOwner(t *testing.T) {
	binding, funding := adjustmentNormalizationFixture(false)
	w := adjustmentNormalizationWire("partial", []paddlewire.AdjustmentItem{
		adjustmentItem(1, binding.Lines[0].ProviderLineID, "partial", 40, 10),
		adjustmentItem(2, binding.Lines[0].ProviderLineID, "tax", 0, 5),
	}, 40, 15)

	got, err := normalizeAdjustment(w, binding, funding, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := purchase.PaidLine{LineID: binding.Lines[0].Allocations[0].QuoteLineID, Gross: 55, Tax: 15}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("lines=%+v, want %+v", got, want)
	}
}

func TestNormalizeAdjustmentExpandsExactFullGroupedItem(t *testing.T) {
	binding, funding := adjustmentNormalizationFixture(true)
	w := adjustmentNormalizationWire("full", []paddlewire.AdjustmentItem{
		adjustmentItem(1, binding.Lines[0].ProviderLineID, "full", 80, 20),
	}, 80, 20)

	got, err := normalizeAdjustment(w, binding, funding, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != funding.Lines[0] || got[1] != funding.Lines[1] {
		t.Fatalf("expanded lines=%+v, want %+v", got, funding.Lines)
	}

	w.Items[0].Totals = adjustmentTotals(79, 20, "")
	w.Totals = adjustmentTotals(79, 20, "USD")
	if _, err := normalizeAdjustment(w, binding, funding, nil); !groupedAdjustmentUnresolved(err) {
		t.Fatalf("remaining grouped full error=%v, want unresolved", err)
	}
}

func TestNormalizeAdjustmentAcceptsRemainingFullForSingleOwnerAndNullableTopType(t *testing.T) {
	binding, funding := adjustmentNormalizationFixture(false)
	w := adjustmentNormalizationWire("", []paddlewire.AdjustmentItem{
		adjustmentItem(1, binding.Lines[0].ProviderLineID, "full", 40, 10),
	}, 40, 10)
	got, err := normalizeAdjustment(w, binding, funding, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := purchase.PaidLine{LineID: "line-a", Gross: 50, Tax: 10}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("lines=%+v, want %+v", got, want)
	}
}

func TestNormalizeAdjustmentBoundGroupedPartialUsesPlannedLines(t *testing.T) {
	binding, funding := adjustmentNormalizationFixture(true)
	w := adjustmentNormalizationWire("partial", []paddlewire.AdjustmentItem{
		adjustmentItem(1, binding.Lines[0].ProviderLineID, "partial", 40, 10),
	}, 40, 10)
	planned := &RefundDispatch{
		Account: binding.Account, TransactionID: binding.TransactionID,
		Customer: billing.Reference{Scope: binding.Scope, ID: binding.CustomerID},
		Lines:    []RefundDispatchLine{{QuoteLineID: "line-a", Gross: 50, Tax: 10}},
	}
	got, err := normalizeAdjustment(w, binding, funding, planned)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (purchase.PaidLine{LineID: "line-a", Gross: 50, Tax: 10}) {
		t.Fatalf("planned lines=%+v", got)
	}
	planned.Lines[0].Gross = 40
	if _, err := normalizeAdjustment(w, binding, funding, planned); !groupedAdjustmentUnresolved(err) {
		t.Fatalf("mismatched planned error=%v", err)
	}
}

func TestNormalizeAdjustmentRejectsGroupedPartialWithoutInferringOwnership(t *testing.T) {
	binding, funding := adjustmentNormalizationFixture(true)
	w := adjustmentNormalizationWire("partial", []paddlewire.AdjustmentItem{
		adjustmentItem(1, binding.Lines[0].ProviderLineID, "partial", 40, 10),
	}, 40, 10)

	_, err := normalizeAdjustment(w, binding, funding, nil)
	if !groupedAdjustmentUnresolved(err) {
		t.Fatalf("error=%v, want grouped unresolved", err)
	}

	w.Items[0].Type = "tax"
	w.Items[0].Totals = adjustmentTotals(0, 50, "")
	w.Totals = adjustmentTotals(0, 50, "USD")
	if _, err := normalizeAdjustment(w, binding, funding, nil); !groupedAdjustmentUnresolved(err) {
		t.Fatalf("tax error=%v, want grouped unresolved", err)
	}
}

func TestNormalizeAdjustmentRejectsIdentityOwnershipAndTotalsDefects(t *testing.T) {
	binding, funding := adjustmentNormalizationFixture(false)
	valid := adjustmentNormalizationWire("partial", []paddlewire.AdjustmentItem{
		adjustmentItem(1, binding.Lines[0].ProviderLineID, "partial", 40, 10),
	}, 40, 10)

	tests := []struct {
		name string
		edit func(*paddlewire.Adjustment)
		want error
	}{
		{"foreign item", func(w *paddlewire.Adjustment) { w.Items[0].ItemID = paddleTestID("txnitm_", 9) }, billing.ErrConflict},
		{"duplicate adjustment item", func(w *paddlewire.Adjustment) {
			w.Items = append(w.Items, w.Items[0])
			w.Totals = adjustmentTotals(80, 20, "USD")
		}, ErrResponse},
		{"bad adjustment item id", func(w *paddlewire.Adjustment) { w.Items[0].ID = "bad" }, ErrResponse},
		{"component arithmetic", func(w *paddlewire.Adjustment) { w.Items[0].Totals.Total = "49" }, ErrResponse},
		{"aggregate arithmetic", func(w *paddlewire.Adjustment) { w.Totals.Total = "51" }, ErrResponse},
		{"tax subtotal", func(w *paddlewire.Adjustment) { w.Items[0].Type = "tax" }, ErrResponse},
		{"currency", func(w *paddlewire.Adjustment) { w.Totals.Currency = "EUR" }, billing.ErrConflict},
		{"customer", func(w *paddlewire.Adjustment) { w.CustomerID = paddleTestID("ctm_", 9) }, billing.ErrConflict},
		{"exceeds funding tax", func(w *paddlewire.Adjustment) {
			w.Items[0].Totals = adjustmentTotals(29, 21, "")
			w.Totals = adjustmentTotals(29, 21, "USD")
		}, billing.ErrConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := valid
			w.Items = append([]paddlewire.AdjustmentItem(nil), valid.Items...)
			test.edit(&w)
			if _, err := normalizeAdjustment(w, binding, funding, nil); !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestNormalizeAdjustmentRejectsCheckedOverflow(t *testing.T) {
	binding, funding := adjustmentNormalizationFixture(false)
	max := strconv.FormatInt(math.MaxInt64, 10)
	w := adjustmentNormalizationWire("partial", []paddlewire.AdjustmentItem{
		{ID: paddleTestID("adjitm_", 1), ItemID: binding.Lines[0].ProviderLineID, Type: "partial", Totals: paddlewire.AdjustmentTotals{Subtotal: max, Tax: "0", Total: max}},
		adjustmentItem(2, binding.Lines[0].ProviderLineID, "partial", 1, 0),
	}, 1, 0)
	if _, err := normalizeAdjustment(w, binding, funding, nil); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow error=%v, want overflow", err)
	}
}

func groupedAdjustmentUnresolved(err error) bool {
	capability, ok := errors.AsType[*billing.CapabilityError](err)
	return ok && capability.Support == billing.SupportUnresolved && capability.Reason == "grouped_adjustment_allocation_unresolved"
}

func adjustmentNormalizationFixture(grouped bool) (purchase.CollectionBinding, purchase.Funding) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	scope := billing.Scope{Provider: "paddle", Merchant: "merchant", Environment: "sandbox"}
	allocations := []purchase.CollectionAllocation{{QuoteLineID: "line-a", Quantity: 1}}
	lines := []purchase.PaidLine{{LineID: "line-a", Gross: 100, Tax: 20}}
	quantity := int64(1)
	if grouped {
		lines[0] = purchase.PaidLine{LineID: "line-a", Gross: 40, Tax: 8}
		allocations = append(allocations, purchase.CollectionAllocation{QuoteLineID: "line-b", Quantity: 2})
		lines = append(lines, purchase.PaidLine{LineID: "line-b", Gross: 60, Tax: 12})
		quantity = 3
	}
	binding := purchase.CollectionBinding{CollectionInput: purchase.CollectionInput{
		Account: "account", Scope: scope, TransactionID: paddleTestID("txn_", 1), IntentID: "intent",
		QuoteFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CustomerID:       paddleTestID("ctm_", 1), Lines: []purchase.CollectionLine{{ProviderLineID: paddleTestID("txnitm_", 1), ProviderPriceID: paddleTestID("pri_", 1), Quantity: quantity, Allocations: allocations}},
		Actor: "actor", Reason: "checkout", EvidenceReference: "evidence",
	}, CreatedAt: now}
	var gross, tax int64
	for _, line := range lines {
		gross += line.Gross
		tax += line.Tax
	}
	return binding, purchase.Funding{Account: binding.Account, Scope: scope, TransactionID: binding.TransactionID, IntentID: binding.IntentID, Currency: "USD", Gross: gross, Tax: tax, PaidAt: now, Lines: lines}
}

func adjustmentNormalizationWire(kind string, items []paddlewire.AdjustmentItem, subtotal, tax int64) paddlewire.Adjustment {
	return paddlewire.Adjustment{
		ID: paddleTestID("adj_", 1), Action: "refund", Status: "approved", Type: kind,
		TransactionID: paddleTestID("txn_", 1), CustomerID: paddleTestID("ctm_", 1), Currency: "USD",
		Items: items, Totals: adjustmentTotals(subtotal, tax, "USD"),
	}
}

func adjustmentItem(id int, itemID, kind string, subtotal, tax int64) paddlewire.AdjustmentItem {
	return paddlewire.AdjustmentItem{ID: paddleTestID("adjitm_", id), ItemID: itemID, Type: kind, Totals: adjustmentTotals(subtotal, tax, "")}
}

func adjustmentTotals(subtotal, tax int64, currency string) paddlewire.AdjustmentTotals {
	return paddlewire.AdjustmentTotals{Subtotal: strconv.FormatInt(subtotal, 10), Tax: strconv.FormatInt(tax, 10), Total: strconv.FormatInt(subtotal+tax, 10), Currency: currency}
}

func paddleTestID(prefix string, suffix int) string {
	return prefix + "0000000000000000000000000" + strconv.Itoa(suffix)
}
