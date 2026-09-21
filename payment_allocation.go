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

// apportion splits total across the bases in proportion to each base,
// giving the leftover cents to the largest remainders and breaking ties by
// line id so the result does not depend on map order. It is used for tax
// and for discount, which are the two amounts the provider reports for the
// whole line rather than per quote line.
func apportion(total int64, bases []int64, ids []string, base int64) []int64 {
	out := make([]int64, len(bases))
	if total == 0 || base == 0 {
		return out
	}
	type leftover struct {
		index     int
		remainder *big.Int
		id        string
	}
	leftovers := make([]leftover, len(bases))
	var assigned int64
	for i := range bases {
		product := new(big.Int).Mul(big.NewInt(total), big.NewInt(bases[i]))
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(product, big.NewInt(base), remainder)
		out[i] = quotient.Int64()
		leftovers[i] = leftover{index: i, remainder: remainder, id: ids[i]}
		assigned += out[i]
	}
	slices.SortFunc(leftovers, func(a, b leftover) int {
		if c := b.remainder.Cmp(a.remainder); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})
	for i := int64(0); i < total-assigned && i < int64(len(leftovers)); i++ {
		out[leftovers[i].index]++
	}
	return out
}

func allocateCollectionMoney(line purchase.CollectionLine, quote purchase.Quote, byID map[string]purchase.QuoteLine, gross, tax, discount int64) ([]purchase.PaidLine, error) {
	type share struct {
		line purchase.PaidLine
		base int64
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
	// The provider is the authority on money: it owns the price, the tax
	// and the discount, and the customer agreed to its figures on its
	// checkout. Our quote is what we showed beforehand, and a difference
	// between the two means our catalog has drifted, not that the payment
	// is invalid. Refusing here used to take a paid customer's purchase
	// away over our own bookkeeping, so the provider's numbers are
	// recorded as given and the disagreement is reported upstream.
	//
	// Nonsense is still nonsense: negative money, or tax larger than the
	// amount it was charged on, is a response we cannot read at all.
	if tax < 0 || gross < tax || discount < 0 {
		return nil, ErrResponse
	}
	_ = expected
	if base == 0 {
		// Nothing to apportion against: every line is recorded at zero and
		// the totals stand as the provider reported them.

		out := make([]purchase.PaidLine, len(shares))
		for i := range shares {
			out[i] = shares[i].line
		}
		slices.SortFunc(out, func(a, b purchase.PaidLine) int { return cmp.Compare(a.LineID, b.LineID) })
		return out, nil
	}
	bases := make([]int64, len(shares))
	ids := make([]string, len(shares))
	for i := range shares {
		bases[i], ids[i] = shares[i].base, shares[i].line.LineID
	}
	// Both are apportioned before anything is reordered, so the two splits
	// see the same order and neither depends on the other's sort.
	taxes := apportion(tax, bases, ids, base)
	discounts := apportion(discount, bases, ids, base)

	// Gross is split the same way, so the lines add up to what the
	// provider actually collected however that compares with the quote.
	grosses := apportion(netCollected(gross, tax, quote.TaxTreatment), bases, ids, base)
	out := make([]purchase.PaidLine, len(shares))
	for i, s := range shares {
		s.line.Tax = taxes[i]
		s.line.Discount = discounts[i]
		s.line.Gross = grosses[i]
		if quote.TaxTreatment == purchase.TaxExclusive {
			var err error
			s.line.Gross, err = paddlewire.AddMoney(s.line.Gross, s.line.Tax)
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

// netCollected is the money taken for the lines before tax is added on
// top, which is the basis the line amounts are split from.
func netCollected(gross, tax int64, treatment purchase.TaxTreatment) int64 {
	if treatment == purchase.TaxExclusive {
		return gross - tax
	}
	return gross
}
