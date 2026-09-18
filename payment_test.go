package paddle

import (
	"errors"
	"fmt"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"math"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestNormalizePaymentTaxCreditEquationAndLatestCapture(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	event := Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(5 * time.Minute)}
	binding, intent, quote := paymentNormalizationFixture(t, now, 1)
	wire := paymentWireFixture(binding, "completed", "100", "20", "0", "0", "100", "0", "USD", []paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "100", "20"}})
	wire.Payments = append(wire.Payments, paymentAttemptFixture("11111111-1111-4111-8111-111111111111", "captured", "45", now.Add(time.Minute), new(now.Add(2*time.Minute))))
	wire.Payments = append(wire.Payments, paymentAttemptFixture("22222222-2222-4222-8222-222222222222", "captured", "55", now.Add(2*time.Minute), new(now.Add(4*time.Minute))))
	wire.Payments = append(wire.Payments, paymentAttemptFixture("33333333-3333-4333-8333-333333333333", "captured", "0", now.Add(2*time.Minute), new(now.Add(4*time.Minute))))
	// The zero-valued third attempt is rejected; captured amounts must be real.
	wire.Payments[2].Amount = "0"
	if _, _, err := normalizePayment(event, wire, binding, intent, quote); err == nil {
		t.Fatal("zero captured attempt was accepted")
	}
	wire.Payments = wire.Payments[:2]
	fact, _, err := normalizePayment(event, wire, binding, intent, quote)
	if err != nil {
		t.Fatal(err)
	}
	if fact.Gross != 100 || fact.Tax != 20 || fact.CollectedAt != now.Add(4*time.Minute) {
		t.Fatalf("normalized fact=%+v", fact)
	}

	bad := wire
	bad.Details.Totals.GrandTotal = "91"
	if _, _, err := normalizePayment(event, bad, binding, intent, quote); err == nil {
		t.Fatal("gross-credit equation mismatch was accepted")
	}
}

func TestPaymentStatusLatestAttemptDoesNotDependOnArrayOrder(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	wire := paddlewire.Payment{Status: "ready"}
	wire.Payments = append(wire.Payments,
		paymentAttemptFixture("11111111-1111-4111-8111-111111111111", "error", "100", now, nil),
		paymentAttemptFixture("22222222-2222-4222-8222-222222222222", "created", "100", now, nil),
		paymentAttemptFixture("33333333-3333-4333-8333-333333333333", "action_required", "100", now.Add(time.Second), nil),
	)
	status, err := paymentStatus("transaction.updated", wire, now.Add(time.Minute))
	if err != nil || status != purchase.FactActionRequired {
		t.Fatalf("latest status=%s error=%v", status, err)
	}
	wire.Payments[1].CreatedAt = now.Add(time.Second)
	if _, err := paymentStatus("transaction.updated", wire, now.Add(time.Minute)); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("ambiguous latest attempts error=%v, want conflict", err)
	}
}

func TestNormalizePaymentUsesLatestCaptureAndRejectsOverflow(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	event := Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(10 * time.Minute)}
	binding, intent, quote := paymentNormalizationFixture(t, now, 2)
	wire := paymentWireFixture(binding, "completed", "101", "0", "0", "0", "101", "0", "USD", []paymentLineFixture{
		{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "9223372036854775807", "0"},
		{"txnitm_zyxwvutsrqponmlkjihgfedcba", "pri_zyxwvutsrqponmlkjihgfedcba", 1, "1", "0"},
	})
	wire.Payments = append(wire.Payments, paymentAttemptFixture("99999999-9999-4999-8999-999999999999", "captured", "101", now, new(now.Add(time.Minute))))
	// One predicate, one error: an arithmetic overflow is billing.ErrOverflow
	// wherever it happens. It used to be ErrResponse here and ErrOverflow in
	// the adjustment path, so matching on either caught only half the cases.
	if _, _, err := normalizePayment(event, wire, binding, intent, quote); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("line sum overflow error=%v, want ErrOverflow", err)
	}
	if _, err := paddlewire.MinorUnits("9999999999999999999"); !errors.Is(err, ErrResponse) {
		t.Fatalf("minor-unit overflow error=%v, want ErrResponse", err)
	}
	if _, err := paddlewire.AddMoney(math.MaxInt64, 1); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("money overflow error=%v, want ErrOverflow", err)
	}
}

func TestNormalizePaymentActionRequiredAndCreditOnlyAreUnresolved(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	binding, intent, quote := paymentNormalizationFixture(t, now, 1)
	action := paddlewire.Payment{ID: binding.TransactionID, CustomerID: binding.CustomerID, Status: "ready", Currency: "USD"}
	action.Payments = append(action.Payments, paymentAttemptFixture("44444444-4444-4444-8444-444444444444", "action_required", "", now, nil))
	fact, _, err := normalizePayment(Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.ready", OccurredAt: now.Add(time.Minute)}, action, binding, intent, quote)
	if err != nil || fact.Status != purchase.FactActionRequired {
		t.Fatalf("action-required fact=%+v err=%v", fact, err)
	}
	creditOnly := paymentWireFixture(binding, "completed", "100", "0", "100", "0", "0", "0", "USD", []paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "100", "0"}})
	if _, _, err := normalizePayment(Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(time.Minute)}, creditOnly, binding, intent, quote); err == nil {
		t.Fatal("credit-only payment without capture evidence was accepted")
	} else if capability, ok := errors.AsType[*billing.CapabilityError](err); !ok || capability.Capability.Support != billing.SupportUnresolved || capability.Capability.Reason != "provider_credit_allocation_unresolved" {
		t.Fatalf("credit-only error=%v, want unresolved capability", err)
	}
}

func TestNormalizePaymentProviderCreditIsUnresolved(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	binding, intent, quote := paymentNormalizationFixture(t, now, 1)
	for _, tt := range []struct {
		name, credit, grand string
	}{
		{name: "partial", credit: "1", grand: "99"},
		{name: "full", credit: "100", grand: "0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wire := paymentWireFixture(binding, "completed", "100", "0", tt.credit, "0", tt.grand, "0", "USD", []paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "100", "0"}})
			_, _, err := normalizePayment(Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(time.Minute)}, wire, binding, intent, quote)
			capability, ok := errors.AsType[*billing.CapabilityError](err)
			if !ok || capability.Capability.Support != billing.SupportUnresolved || capability.Capability.Reason != "provider_credit_allocation_unresolved" {
				t.Fatalf("error=%v, want provider credit unresolved", err)
			}
		})
	}
}

func TestPaymentStatusAttemptAndEventCoherence(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		kind, status string
		want         purchase.PaymentFactStatus
	}{
		{"transaction.ready", "ready", purchase.FactPending},
		{"transaction.billed", "billed", purchase.FactPending},
		{"transaction.past_due", "past_due", purchase.FactFailed},
		{"transaction.canceled", "canceled", purchase.FactFailed},
		{"transaction.created", "draft", purchase.FactPending},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			wire := paddlewire.Payment{Status: tt.status}
			wire.Payments = append(wire.Payments, paymentAttemptFixture("55555555-5555-4555-8555-555555555555", "created", "", now, nil))
			got, err := paymentStatus(tt.kind, wire, now.Add(time.Minute))
			if err != nil || got != tt.want {
				t.Fatalf("status=%q got=%q err=%v", tt.status, got, err)
			}
		})
	}
	for _, attemptStatus := range []string{"error", "dropped", "canceled"} {
		t.Run(attemptStatus, func(t *testing.T) {
			wire := paddlewire.Payment{Status: "ready"}
			wire.Payments = append(wire.Payments, paymentAttemptFixture("66666666-6666-4666-8666-666666666666", attemptStatus, "", now, nil))
			got, err := paymentStatus("transaction.ready", wire, now.Add(time.Minute))
			if err != nil || got != purchase.FactFailed {
				t.Fatalf("got=%q err=%v, want failed", got, err)
			}
		})
	}
	wire := paddlewire.Payment{Status: "ready"}
	wire.Payments = append(wire.Payments, paymentAttemptFixture("77777777-7777-4777-8777-777777777777", "provider_new", "", now, nil))
	if _, err := paymentStatus("transaction.ready", wire, now.Add(time.Minute)); !errors.Is(err, ErrResponse) {
		t.Fatalf("unknown attempt status error=%v, want ErrResponse", err)
	}
	if _, err := paymentStatus("transaction.ready", paddlewire.Payment{Status: "billed"}, now.Add(time.Minute)); !errors.Is(err, ErrResponse) {
		t.Fatalf("incoherent event error=%v, want ErrResponse", err)
	}
}

func TestNormalizePaymentCanonicalizesCaptureAndCreatedTimes(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	binding, intent, quote := paymentNormalizationFixture(t, now, 1)
	eventAt := now.Add(5*time.Minute + 500*time.Nanosecond)
	captureAt := now.Add(4*time.Minute + 500*time.Nanosecond)
	wire := paymentWireFixture(binding, "completed", "100", "0", "0", "0", "100", "0", "USD", []paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "100", "0"}})
	wire.Payments = append(wire.Payments, paymentAttemptFixture("88888888-8888-4888-8888-888888888888", "captured", "100", now.Add(3*time.Minute+500*time.Nanosecond), new(captureAt)))
	fact, _, err := normalizePayment(Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: eventAt}, wire, binding, intent, quote)
	if err != nil {
		t.Fatal(err)
	}
	if !fact.OccurredAt.Equal(billing.CanonicalTime(eventAt)) || !fact.CollectedAt.Equal(billing.CanonicalTime(captureAt)) {
		t.Fatalf("fact times occurred=%v collected=%v", fact.OccurredAt, fact.CollectedAt)
	}
}

func TestNormalizePaymentRejectsMalformedAttemptEvidence(t *testing.T) {
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	binding, intent, quote := paymentNormalizationFixture(t, now, 1)
	base := func() paddlewire.Payment {
		wire := paymentWireFixture(binding, "completed", "100", "0", "0", "0", "100", "0", "USD", []paymentLineFixture{{"txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz", 1, "100", "0"}})
		wire.Payments = append(wire.Payments, paymentAttemptFixture("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "captured", "100", now, new(now.Add(time.Minute))))
		return wire
	}
	for _, tt := range []struct {
		name string
		edit func(*paddlewire.Payment)
	}{
		{name: "duplicate case insensitive id", edit: func(w *paddlewire.Payment) {
			w.Payments = append(w.Payments, paymentAttemptFixture("AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "captured", "1", now, new(now.Add(time.Minute))))
		}},
		{name: "created after event", edit: func(w *paddlewire.Payment) {
			w.Payments[0].CreatedAt = now.Add(2 * time.Minute)
		}},
		{name: "capture before created", edit: func(w *paddlewire.Payment) {
			w.Payments[0].CapturedAt = new(now.Add(-time.Minute))
		}},
		{name: "noncaptured has capture", edit: func(w *paddlewire.Payment) {
			w.Payments[0].Status = "created"
		}},
		{name: "captured missing capture", edit: func(w *paddlewire.Payment) {
			w.Payments[0].CapturedAt = nil
		}},
		{name: "unknown status", edit: func(w *paddlewire.Payment) {
			w.Payments[0].Status = "provider_new"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wire := base()
			tt.edit(&wire)
			if _, _, err := normalizePayment(Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(time.Minute)}, wire, binding, intent, quote); !errors.Is(err, ErrResponse) {
				t.Fatalf("error=%v, want ErrResponse", err)
			}
		})
	}
	wire := base()
	for i := 0; i < 1000; i++ {
		wire.Payments = append(wire.Payments, paymentAttemptFixture(fmt.Sprintf("%08d-aaaa-4aaa-8aaa-aaaaaaaaaaaa", i), "created", "", now, nil))
	}
	if _, _, err := normalizePayment(Event{ID: "evt_abcdefghijklmnopqrstuvwxyz", Type: "transaction.completed", OccurredAt: now.Add(time.Minute)}, wire, binding, intent, quote); !errors.Is(err, ErrResponse) {
		t.Fatalf("oversized attempts error=%v, want ErrResponse", err)
	}
}

type paymentLineFixture struct {
	id, price  string
	quantity   int64
	total, tax string
}

func paymentWireFixture(binding purchase.CollectionBinding, status, total, tax, credit, creditToBalance, grandTotal, balance, currency string, lines []paymentLineFixture) paddlewire.Payment {
	out := paddlewire.Payment{ID: binding.TransactionID, CustomerID: binding.CustomerID, Status: status, Currency: currency}
	out.Details.Totals.Total, out.Details.Totals.Tax, out.Details.Totals.Credit = total, tax, credit
	out.Details.Totals.CreditToBalance, out.Details.Totals.GrandTotal, out.Details.Totals.Balance, out.Details.Totals.Currency = creditToBalance, grandTotal, balance, currency
	for _, line := range lines {
		out.Details.Lines = append(out.Details.Lines, struct {
			ID       string `json:"id"`
			PriceID  string `json:"price_id"`
			Quantity int64  `json:"quantity"`
			Totals   struct {
				Total string `json:"total"`
				Tax   string `json:"tax"`
			} `json:"totals"`
		}{ID: line.id, PriceID: line.price, Quantity: line.quantity, Totals: struct {
			Total string `json:"total"`
			Tax   string `json:"tax"`
		}{Total: line.total, Tax: line.tax}})
	}
	return out
}

func paymentAttemptFixture(id, status, amount string, created time.Time, captured *time.Time) struct {
	ID         string     `json:"payment_attempt_id"`
	Status     string     `json:"status"`
	Amount     string     `json:"amount"`
	CreatedAt  time.Time  `json:"created_at"`
	CapturedAt *time.Time `json:"captured_at"`
} {
	return struct {
		ID         string     `json:"payment_attempt_id"`
		Status     string     `json:"status"`
		Amount     string     `json:"amount"`
		CreatedAt  time.Time  `json:"created_at"`
		CapturedAt *time.Time `json:"captured_at"`
	}{ID: id, Status: status, Amount: amount, CreatedAt: created, CapturedAt: captured}
}

func paymentNormalizationFixture(t *testing.T, now time.Time, lines int) (purchase.CollectionBinding, purchase.Intent, purchase.Quote) {
	t.Helper()
	scope := billing.Scope{Provider: "paddle", Merchant: "merchant-test", Environment: "sandbox"}
	service := purchase.New(purchase.NewMemoryRepository(purchase.ReferenceAccount{Account: "account"}), func() time.Time { return now })
	offer, err := service.PublishOffer(t.Context(), purchase.Offer{Account: "account", Revision: purchase.Revision{ID: "offer", Version: 1}, Name: "Payment"})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(t.Context(), purchase.Price{Account: "account", Revision: purchase.Revision{ID: "price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	var quoteLines []purchase.QuoteLineInput
	for i := range lines {
		id := "line"
		if i == 1 {
			id = "line-two"
		}
		quoteLines = append(quoteLines, purchase.QuoteLineInput{ID: id, Price: price.Revision, Quantity: 1})
	}
	quote, err := service.CreateQuote(t.Context(), purchase.QuoteInput{Account: "account", ID: "quote", ValidUntil: now.Add(time.Hour), Lines: quoteLines})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.CreateIntent(t.Context(), purchase.IntentInput{Account: "account", ID: "intent", Operation: "operation", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "actor", Reason: "reason", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	binding := purchase.CollectionBinding{CollectionInput: purchase.CollectionInput{Account: intent.Account, Scope: scope, TransactionID: "txn_abcdefghijklmnopqrstuvwxyz", IntentID: intent.ID, QuoteFingerprint: quote.Fingerprint(), CustomerID: "ctm_abcdefghijklmnopqrstuvwxyz", Actor: "actor", Reason: "reason", EvidenceReference: "evidence"}, CreatedAt: now}
	for i, q := range quoteLines {
		id, priceID := "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"
		if i == 1 {
			id, priceID = "txnitm_zyxwvutsrqponmlkjihgfedcba", "pri_zyxwvutsrqponmlkjihgfedcba"
		}
		binding.Lines = append(binding.Lines, purchase.CollectionLine{ProviderLineID: id, ProviderPriceID: priceID, Quantity: 1, Allocations: []purchase.CollectionAllocation{{QuoteLineID: q.ID, Quantity: 1}}})
	}
	return binding, intent, quote
}
