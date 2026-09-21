package paddle

import (
	"testing"
	"time"

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

// Relaxing a check is only safe if you show what it still catches.
func TestNormalizePaymentRefusesAnUnexplainedShortfall(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	event := Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(5 * time.Minute)}

	cases := []struct {
		name                                 string
		subtotal, total, discount, lineTotal string
		lineSubtotal, lineDiscount           string
	}{
		{
			name:     "collected less than quoted with no discount",
			subtotal: "100", total: "60", discount: "0", lineTotal: "60", lineSubtotal: "100", lineDiscount: "0",
		},
		{
			name:     "discount does not explain the gap",
			subtotal: "100", total: "20", discount: "30", lineTotal: "20", lineSubtotal: "100", lineDiscount: "30",
		},
		{
			name:     "line discount disagrees with the total",
			subtotal: "100", total: "50", discount: "50", lineTotal: "50", lineSubtotal: "100", lineDiscount: "10",
		},
		{
			name:     "discount larger than the list price",
			subtotal: "100", total: "0", discount: "150", lineTotal: "0", lineSubtotal: "100", lineDiscount: "150",
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
