package paddle

import (
	"cmp"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"math/big"
	"slices"

	"github.com/data-insights-ai/rho-billing/purchase"
)

// allocateCollectionMoney preserves each explicitly owned quote line's exact
// commercial basis. Only indivisible tax cents are apportioned. Ownership never
// comes from matching amounts or provider response position.
// quoteLineIndex is built once per payment and reused for every collected
// line: this runs inside the account transaction, and rebuilding it per line
// made a 100-line payment do 100 map allocations while Postgres waited.
func quoteLineIndex(quote purchase.Quote) map[string]purchase.QuoteLine {
	byID := make(map[string]purchase.QuoteLine, len(quote.Lines))
	for _, q := range quote.Lines {
		byID[q.ID] = q
	}
	return byID
}

func allocateCollectionMoney(line purchase.CollectionLine, quote purchase.Quote, byID map[string]purchase.QuoteLine, gross, tax int64) ([]purchase.PaidLine, error) {
	type share struct {
		line      purchase.PaidLine
		base      int64
		remainder *big.Int
	}
	shares := make([]share, 0, len(line.Allocations))
	var base int64
	for _, allocation := range line.Allocations {
		q, ok := byID[allocation.QuoteLineID]
		if !ok || q.Quantity != allocation.Quantity {
			return nil, ErrResponse
		}
		var err error
		base, err = paddlewire.AddMoney(base, q.Amount)
		if err != nil {
			return nil, err
		}
		shares = append(shares, share{line: purchase.PaidLine{LineID: q.ID}, base: q.Amount})
	}
	expected := gross
	if quote.TaxTreatment == purchase.TaxExclusive {
		expected = gross - tax
	}
	if base != expected || tax < 0 || gross < tax {
		return nil, ErrResponse
	}
	if base == 0 {
		if gross != 0 || tax != 0 {
			return nil, ErrResponse
		}
		out := make([]purchase.PaidLine, len(shares))
		for i := range shares {
			out[i] = shares[i].line
		}
		slices.SortFunc(out, func(a, b purchase.PaidLine) int { return cmp.Compare(a.LineID, b.LineID) })
		return out, nil
	}
	var assigned int64
	for i := range shares {
		product := new(big.Int).Mul(big.NewInt(tax), big.NewInt(shares[i].base))
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(product, big.NewInt(base), remainder)
		shares[i].line.Tax = quotient.Int64()
		shares[i].remainder = remainder
		assigned += shares[i].line.Tax
	}
	slices.SortFunc(shares, func(a, b share) int {
		if c := b.remainder.Cmp(a.remainder); c != 0 {
			return c
		}
		return cmp.Compare(a.line.LineID, b.line.LineID)
	})
	for i := int64(0); i < tax-assigned; i++ {
		shares[i].line.Tax++
	}
	out := make([]purchase.PaidLine, len(shares))
	for i, s := range shares {
		s.line.Gross = s.base
		if quote.TaxTreatment == purchase.TaxExclusive {
			var err error
			s.line.Gross, err = paddlewire.AddMoney(s.base, s.line.Tax)
			if err != nil {
				return nil, err
			}
		}
		if s.line.Tax > s.line.Gross {
			return nil, ErrResponse
		}
		out[i] = s.line
	}
	slices.SortFunc(out, func(a, b purchase.PaidLine) int { return cmp.Compare(a.LineID, b.LineID) })
	return out, nil
}
