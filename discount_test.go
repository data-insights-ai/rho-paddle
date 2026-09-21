package paddle

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
)

// A discount is the one legitimate reason for the provider to collect less
// than we quoted. This is the case that broke in production: a full
// discount code, a transaction of zero, no capture to date the collection
// from, and an active subscription at the provider that never reached us.
func TestNormalizePaymentAcceptsADiscountedTransaction(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	event := Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(5 * time.Minute)}

	cases := []struct {
		name                     string
		total, discount, grand   string
		captured                 string
		wantGross, wantDiscount  int64
		wantCollectedAtEventTime bool
	}{
		{
			name: "half off", total: "50", discount: "50", grand: "50", captured: "50",
			wantGross: 50, wantDiscount: 50,
		},
		{
			name: "free", total: "0", discount: "100", grand: "0", captured: "",
			wantGross: 0, wantDiscount: 100, wantCollectedAtEventTime: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			binding, intent, quote := paymentNormalizationFixture(t, now, 1)
			wire := paymentWireFixture(binding, "completed", c.total, "0", "0", "0", c.grand, "0", "USD",
				[]paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, c.total, "0", "100", c.discount}})
			wire.Details.Totals.Subtotal, wire.Details.Totals.Discount = "100", c.discount
			if c.captured != "" {
				wire.Payments = append(wire.Payments, paymentAttemptFixture(
					"11111111-1111-4111-8111-111111111111", "captured", c.captured,
					now.Add(time.Minute), new(now.Add(2*time.Minute))))
			}

			fact, _, err := normalizePayment(event, wire, binding, intent, quote)
			if err != nil {
				t.Fatal(err)
			}
			if fact.Gross != c.wantGross || fact.Discount != c.wantDiscount {
				t.Fatalf("gross %d discount %d, want %d and %d", fact.Gross, fact.Discount, c.wantGross, c.wantDiscount)
			}
			if len(fact.Lines) != 1 || fact.Lines[0].Gross != c.wantGross || fact.Lines[0].Discount != c.wantDiscount {
				t.Fatalf("lines = %+v", fact.Lines)
			}
			// Nothing captured means nothing was owed, so the collection is
			// dated by the event that reported the transaction paid.
			if c.wantCollectedAtEventTime && !fact.CollectedAt.Equal(event.OccurredAt) {
				t.Fatalf("collected at %v, want the event time %v", fact.CollectedAt, event.OccurredAt)
			}
			if err := fact.Validate(); err != nil {
				t.Fatalf("the fact this produces must be valid: %v", err)
			}
		})
	}
}

// The exact shape that failed in production: a full discount, a zero
// total, and a payment attempt that was authorised and never captured
// because there was nothing to capture. Written from the stored payload
// rather than from imagination.
func TestNormalizePaymentAcceptsTheTransactionThatFailedInProduction(t *testing.T) {
	now := time.Date(2026, 9, 21, 17, 51, 15, 0, time.UTC)
	event := Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(2 * time.Second)}
	binding, intent, quote := paymentNormalizationFixture(t, now, 1)

	wire := paymentWireFixture(binding, "completed", "0", "0", "0", "0", "0", "0", "USD",
		[]paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "0", "0", "100", "100"}})
	wire.Details.Totals.Subtotal, wire.Details.Totals.Discount = "100", "100"
	// Authorised, never captured: Paddle records the attempt even when the
	// amount owed is nothing.
	wire.Payments = append(wire.Payments, paymentAttemptFixture(
		"11111111-1111-4111-8111-111111111111", "authorized", "0", now.Add(time.Second), nil))

	fact, _, err := normalizePayment(event, wire, binding, intent, quote)
	if err != nil {
		t.Fatalf("the transaction that failed in production still fails: %v", err)
	}
	if fact.Gross != 0 || fact.Discount != 100 {
		t.Fatalf("gross %d discount %d", fact.Gross, fact.Discount)
	}
	if !fact.CollectedAt.Equal(event.OccurredAt) {
		t.Fatalf("collected at %v, want the event time", fact.CollectedAt)
	}
	if err := fact.Validate(); err != nil {
		t.Fatalf("fact invalid: %v", err)
	}
}

// The provider is the authority on money, so its total is recorded as
// given even when it differs from the quote we showed beforehand. What is
// still refused is a payload that contradicts itself: those numbers we
// cannot read at all, and reading them wrongly would put a figure in the
// ledger that nobody ever charged.
func TestNormalizePaymentRefusesOnlyAContradictoryPayload(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	event := Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(5 * time.Minute)}

	cases := []struct {
		name                                 string
		subtotal, total, discount, lineTotal string
		lineSubtotal, lineDiscount           string
	}{
		{
			name:     "line discount disagrees with the transaction discount",
			subtotal: "100", total: "50", discount: "50", lineTotal: "50", lineSubtotal: "100", lineDiscount: "10",
		},
		{
			name:     "discount larger than the list price",
			subtotal: "100", total: "0", discount: "150", lineTotal: "0", lineSubtotal: "100", lineDiscount: "150",
		},
		{
			name:     "totals do not follow the provider's own arithmetic",
			subtotal: "100", total: "20", discount: "30", lineTotal: "20", lineSubtotal: "100", lineDiscount: "30",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			binding, intent, quote := paymentNormalizationFixture(t, now, 1)
			wire := paymentWireFixture(binding, "completed", c.total, "0", "0", "0", c.total, "0", "USD",
				[]paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, c.lineTotal, "0", c.lineSubtotal, c.lineDiscount}})
			wire.Details.Totals.Subtotal, wire.Details.Totals.Discount = c.subtotal, c.discount
			if c.total != "0" {
				wire.Payments = append(wire.Payments, paymentAttemptFixture(
					"11111111-1111-4111-8111-111111111111", "captured", c.total,
					now.Add(time.Minute), new(now.Add(2*time.Minute))))
			}
			if _, _, err := normalizePayment(event, wire, binding, intent, quote); err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
		})
	}
}

// A discount that cannot be verified is refused rather than trusted: the
// subtotal is what makes the arithmetic checkable.
func TestNormalizePaymentNeedsASubtotalToVerifyADiscount(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	event := Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(5 * time.Minute)}
	binding, intent, quote := paymentNormalizationFixture(t, now, 1)
	wire := paymentWireFixture(binding, "completed", "50", "0", "0", "0", "50", "0", "USD",
		[]paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "50", "0", "100", "50"}})
	// Subtotal deliberately absent.
	wire.Details.Totals.Subtotal, wire.Details.Totals.Discount = "", "50"
	wire.Payments = append(wire.Payments, paymentAttemptFixture(
		"11111111-1111-4111-8111-111111111111", "captured", "50", now.Add(time.Minute), new(now.Add(2*time.Minute))))
	if _, _, err := normalizePayment(event, wire, binding, intent, quote); err == nil {
		t.Fatal("an unverifiable discount was accepted")
	}
}

// A transaction without any discount must behave exactly as before, which
// is what keeps every payload that predates this field working.
func TestNormalizePaymentIsUnchangedWithoutADiscount(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	event := Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(5 * time.Minute)}
	binding, intent, quote := paymentNormalizationFixture(t, now, 1)
	wire := paymentWireFixture(binding, "completed", "100", "20", "0", "0", "100", "0", "USD",
		[]paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "100", "20", "", ""}})
	wire.Payments = append(wire.Payments, paymentAttemptFixture(
		"11111111-1111-4111-8111-111111111111", "captured", "100", now.Add(time.Minute), new(now.Add(2*time.Minute))))
	fact, _, err := normalizePayment(event, wire, binding, intent, quote)
	if err != nil {
		t.Fatal(err)
	}
	if fact.Gross != 100 || fact.Tax != 20 || fact.Discount != 0 {
		t.Fatalf("fact = %+v", fact)
	}
	if len(fact.Lines) != 1 || fact.Lines[0].Discount != 0 {
		t.Fatalf("lines = %+v", fact.Lines)
	}
	var zero paddlewire.PaymentLine
	if zero.Totals.Discount != "" {
		t.Fatal("an unset discount must read as absent, not as a number")
	}
}

// The case an EU seller actually bills: a tax-exclusive price with VAT
// added on top, and a partial discount. Paddle discounts the net, then
// charges tax on what is left, and every one of those numbers has to
// reconcile against a quote that knows nothing about the discount.
func TestNormalizePaymentHandlesAPartialDiscountWithTaxOnTop(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	event := Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(time.Minute)}
	binding, intent, quote := exclusivePaymentFixture(t, now)

	// Quoted 100 net. Half off leaves 50 net, 20% VAT on that is 10, so
	// the customer owes 60.
	wire := paymentWireFixture(binding, "completed", "60", "10", "0", "0", "60", "0", "USD",
		[]paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "60", "10", "100", "50"}})
	wire.Details.Totals.Subtotal, wire.Details.Totals.Discount = "100", "50"
	wire.Payments = append(wire.Payments, paymentAttemptFixture(
		"11111111-1111-4111-8111-111111111111", "captured", "60", now, new(now.Add(time.Second))))

	fact, _, err := normalizePayment(event, wire, binding, intent, quote)
	if err != nil {
		t.Fatal(err)
	}
	if fact.Gross != 60 || fact.Tax != 10 || fact.Discount != 50 {
		t.Fatalf("gross %d tax %d discount %d, want 60, 10 and 50", fact.Gross, fact.Tax, fact.Discount)
	}
	if len(fact.Lines) != 1 {
		t.Fatalf("lines = %+v", fact.Lines)
	}
	line := fact.Lines[0]
	// The quote said 100 net. Collected net is gross less tax, 50; the
	// discount accounts for the other 50.
	if line.Gross-line.Tax+line.Discount != quote.Lines[0].Amount {
		t.Fatalf("line %+v does not reconcile against a quote of %d", line, quote.Lines[0].Amount)
	}
	if err := fact.Validate(); err != nil {
		t.Fatalf("fact invalid: %v", err)
	}
}

// exclusivePaymentFixture is paymentNormalizationFixture with tax charged
// on top of the price rather than included in it, which is how a European
// seller quotes.
func exclusivePaymentFixture(t *testing.T, now time.Time) (purchase.CollectionBinding, purchase.Intent, purchase.Quote) {
	t.Helper()
	scope := billing.Scope{Provider: "paddle", Merchant: "merchant-test", Environment: "sandbox"}
	service := purchase.New(purchase.NewMemoryRepository(purchase.ReferenceAccount{Account: "account"}), func() time.Time { return now })
	offer, err := service.PublishOffer(t.Context(), purchase.Offer{Account: "account", Revision: purchase.Revision{ID: "offer", Version: 1}, Name: "Payment"})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(t.Context(), purchase.Price{Account: "account", Revision: purchase.Revision{ID: "price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quoteLines := []purchase.QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 1}}
	quote, err := service.CreateQuote(t.Context(), purchase.QuoteInput{Account: "account", ID: "quote", ValidUntil: now.Add(time.Hour), Lines: quoteLines})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.CreateIntent(t.Context(), purchase.IntentInput{Account: "account", ID: "intent", Operation: "operation", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "actor", Reason: "reason", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	binding := purchase.CollectionBinding{CollectionInput: purchase.CollectionInput{Account: intent.Account, Scope: scope, TransactionID: "txn_abcdefghijklmnopqrstuvwxyz", IntentID: intent.ID, QuoteFingerprint: quote.Fingerprint(), CustomerID: "ctm_abcdefghijklmnopqrstuvwxyz", Actor: "actor", Reason: "reason", EvidenceReference: "evidence"}, CreatedAt: now}
	binding.Lines = append(binding.Lines, purchase.CollectionLine{
		ProviderLineID: "txnitm_abcdefghijklmnopqrstuvwxyz", ProviderPriceID: "pri_abcdefghijklmnopqrstuvwxyz",
		Quantity: 1, Allocations: []purchase.CollectionAllocation{{QuoteLineID: "line", Quantity: 1}},
	})
	return binding, intent, quote
}

// A provider total that simply differs from our quote is recorded, not
// refused. Our quote is what we showed beforehand; the provider is what
// actually charged, and the customer agreed to its figure. Taking their
// purchase away over our own bookkeeping is the one outcome nobody wants.
func TestNormalizePaymentRecordsATotalThatDiffersFromTheQuote(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	event := Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(time.Minute)}
	binding, intent, quote := paymentNormalizationFixture(t, now, 1)
	if quote.Lines[0].Amount != 100 {
		t.Fatalf("fixture quote is %d", quote.Lines[0].Amount)
	}

	// Quoted 100, the provider collected 60 and reports no discount.
	wire := paymentWireFixture(binding, "completed", "60", "0", "0", "0", "60", "0", "USD",
		[]paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "60", "0", "", ""}})
	wire.Payments = append(wire.Payments, paymentAttemptFixture(
		"11111111-1111-4111-8111-111111111111", "captured", "60", now, new(now.Add(time.Second))))

	fact, _, err := normalizePayment(event, wire, binding, intent, quote)
	if err != nil {
		t.Fatalf("a total that differs from the quote must still be recorded: %v", err)
	}
	if fact.Gross != 60 {
		t.Fatalf("gross = %d, want the 60 the provider collected", fact.Gross)
	}
	if len(fact.Lines) != 1 || fact.Lines[0].Gross != 60 {
		t.Fatalf("lines = %+v, want the collected amount", fact.Lines)
	}
	if err := fact.Validate(); err != nil {
		t.Fatalf("fact invalid: %v", err)
	}
}
