package paddle

import (
	"cmp"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"math"
	"slices"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

type adjustmentComponent struct {
	gross, tax, subtotal int64
	allFull              bool
}

// normalizeAdjustment maps authoritative Paddle adjustment-item totals through
// the immutable collection binding. It never infers ownership for a partial
// adjustment of a provider item that owns more than one quote line.
func normalizeAdjustment(w paddlewire.Adjustment, binding purchase.CollectionBinding, funding purchase.Funding, planned *RefundDispatch) ([]purchase.PaidLine, error) {
	if binding.Validate() != nil || funding.Validate() != nil ||
		binding.Account != funding.Account || binding.Scope != funding.Scope ||
		binding.IntentID != funding.IntentID || binding.TransactionID != funding.TransactionID ||
		w.TransactionID != binding.TransactionID || w.CustomerID != binding.CustomerID ||
		w.Currency != funding.Currency || w.Totals.Currency != funding.Currency ||
		!paddlewire.ID(w.ID, "adj_") || !paddlewire.ID(w.TransactionID, "txn_") ||
		!paddlewire.ID(w.CustomerID, "ctm_") || len(w.Items) == 0 || len(w.Items) > 100 ||
		(w.Type != "" && w.Type != "full" && w.Type != "partial") {
		return nil, billing.ErrConflict
	}
	if planned != nil && (planned.Account != binding.Account || planned.TransactionID != w.TransactionID || planned.Customer.ID != w.CustomerID || planned.Customer.Scope != binding.Scope) {
		return nil, billing.ErrConflict
	}

	bound := make(map[string]purchase.CollectionLine, len(binding.Lines))
	funded := make(map[string]purchase.PaidLine, len(funding.Lines))
	for _, line := range binding.Lines {
		bound[line.ProviderLineID] = line
	}
	for _, line := range funding.Lines {
		funded[line.LineID] = line
	}
	// Collection and funding must describe exactly the same quote-line owners.
	covered := make(map[string]struct{}, len(funding.Lines))
	for _, line := range binding.Lines {
		for _, allocation := range line.Allocations {
			if _, ok := funded[allocation.QuoteLineID]; !ok {
				return nil, billing.ErrConflict
			}
			covered[allocation.QuoteLineID] = struct{}{}
		}
	}
	if len(covered) != len(funded) {
		return nil, billing.ErrConflict
	}

	seenItems := make(map[string]struct{}, len(w.Items))
	components := make(map[string]adjustmentComponent, len(w.Items))
	for _, item := range w.Items {
		if !paddlewire.ID(item.ID, "adjitm_") || !paddlewire.ID(item.ItemID, "txnitm_") {
			return nil, ErrResponse
		}
		if _, exists := seenItems[item.ID]; exists {
			return nil, ErrResponse
		}
		seenItems[item.ID] = struct{}{}
		if _, ok := bound[item.ItemID]; !ok {
			return nil, billing.ErrConflict
		}
		if item.Type != "full" && item.Type != "partial" && item.Type != "tax" && item.Type != "proration" {
			return nil, ErrResponse
		}
		if item.Totals.Currency != "" && item.Totals.Currency != funding.Currency {
			return nil, billing.ErrConflict
		}
		subtotal, tax, total, err := adjustmentMoney(item.Totals)
		if err != nil {
			return nil, err
		}
		if item.Type == "tax" && subtotal != 0 {
			return nil, ErrResponse
		}
		component, exists := components[item.ItemID]
		if !exists {
			component.allFull = true
		}
		component.subtotal, err = paddlewire.AddMoney(component.subtotal, subtotal)
		if err != nil {
			return nil, err
		}
		component.tax, err = paddlewire.AddMoney(component.tax, tax)
		if err != nil {
			return nil, err
		}
		component.gross, err = paddlewire.AddMoney(component.gross, total)
		if err != nil {
			return nil, err
		}
		component.allFull = component.allFull && item.Type == "full"
		components[item.ItemID] = component
	}

	subtotal, tax, total, err := adjustmentMoney(w.Totals)
	if err != nil {
		return nil, err
	}
	var componentSubtotal, componentTax, componentTotal int64
	for _, component := range components {
		componentSubtotal, err = paddlewire.AddMoney(componentSubtotal, component.subtotal)
		if err != nil {
			return nil, err
		}
		componentTax, err = paddlewire.AddMoney(componentTax, component.tax)
		if err != nil {
			return nil, err
		}
		componentTotal, err = paddlewire.AddMoney(componentTotal, component.gross)
		if err != nil {
			return nil, err
		}
	}
	if componentSubtotal != subtotal || componentTax != tax || componentTotal != total {
		return nil, ErrResponse
	}

	lines := make([]purchase.PaidLine, 0, len(funding.Lines))
	for providerLineID, component := range components {
		collection := bound[providerLineID]
		var originalGross, originalTax int64
		for _, allocation := range collection.Allocations {
			line := funded[allocation.QuoteLineID]
			originalGross, err = paddlewire.AddMoney(originalGross, line.Gross)
			if err != nil {
				return nil, err
			}
			originalTax, err = paddlewire.AddMoney(originalTax, line.Tax)
			if err != nil {
				return nil, err
			}
		}
		originalSubtotal := originalGross - originalTax
		if len(collection.Allocations) > 1 {
			if planned != nil {
				allocated, err := plannedLinesForProviderItem(*planned, collection, component)
				if err != nil {
					return nil, err
				}
				lines = append(lines, allocated...)
				continue
			}
			if !component.allFull || component.gross != originalGross || component.tax != originalTax || component.subtotal != originalSubtotal {
				return nil, unresolvedAdjustment("grouped_adjustment_allocation_unresolved")
			}
			for _, allocation := range collection.Allocations {
				lines = append(lines, funded[allocation.QuoteLineID])
			}
			continue
		}
		if component.gross > originalGross || component.tax > originalTax || component.subtotal > originalSubtotal {
			return nil, billing.ErrConflict
		}
		lines = append(lines, purchase.PaidLine{LineID: collection.Allocations[0].QuoteLineID, Gross: component.gross, Tax: component.tax})
	}
	slices.SortFunc(lines, func(a, b purchase.PaidLine) int { return cmp.Compare(a.LineID, b.LineID) })
	return lines, nil
}

func adjustmentMoney(t paddlewire.AdjustmentTotals) (subtotal, tax, total int64, err error) {
	subtotal, err = paddlewire.MinorUnits(t.Subtotal)
	if err != nil {
		return 0, 0, 0, err
	}
	tax, err = paddlewire.MinorUnits(t.Tax)
	if err != nil {
		return 0, 0, 0, err
	}
	total, err = paddlewire.MinorUnits(t.Total)
	if err != nil {
		return 0, 0, 0, err
	}
	if subtotal > math.MaxInt64-tax {
		return 0, 0, 0, billing.ErrOverflow
	}
	// A bundled zero-value price produces a legitimate zero-total line; only
	// an inconsistent or negative total is a malformed response.
	want := subtotal + tax
	if want != total || subtotal < 0 || tax < 0 {
		return 0, 0, 0, ErrResponse
	}
	return subtotal, tax, total, nil
}

func plannedLinesForProviderItem(planned RefundDispatch, collection purchase.CollectionLine, component adjustmentComponent) ([]purchase.PaidLine, error) {
	owners := make(map[string]struct{}, len(collection.Allocations))
	for _, allocation := range collection.Allocations {
		owners[allocation.QuoteLineID] = struct{}{}
	}
	var lines []purchase.PaidLine
	var gross, tax, subtotal int64
	var err error
	for _, line := range planned.Lines {
		if _, ok := owners[line.QuoteLineID]; !ok {
			continue
		}
		if line.Gross < 0 || line.Tax < 0 || line.Tax > line.Gross {
			return nil, billing.ErrConflict
		}
		gross, err = paddlewire.AddMoney(gross, line.Gross)
		if err != nil {
			return nil, err
		}
		tax, err = paddlewire.AddMoney(tax, line.Tax)
		if err != nil {
			return nil, err
		}
		subtotal, err = paddlewire.AddMoney(subtotal, line.Gross-line.Tax)
		if err != nil {
			return nil, err
		}
		lines = append(lines, purchase.PaidLine{LineID: line.QuoteLineID, Gross: line.Gross, Tax: line.Tax})
	}
	if len(lines) == 0 || gross != component.gross || tax != component.tax || subtotal != component.subtotal {
		return nil, unresolvedAdjustment("grouped_adjustment_allocation_unresolved")
	}
	return lines, nil
}

func unresolvedAdjustment(reason string) error {
	return &billing.CapabilityError{Capability: billing.Capability{Operation: "normalize_adjustment", Support: billing.SupportUnresolved, Reason: reason}}
}
