package paddle_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestTransactionProcessorNonpaidConvergenceAndPaidFence(t *testing.T) {
	f := newPaymentFixture(t, "nonpaid-convergence", true)
	actionTime := webhookNow.Add(-40 * time.Second)
	action := workflowPaymentBody(workflowPayment{
		EventID: "evt_00000000000000000000000001", EventType: "transaction.ready", OccurredAt: actionTime,
		Status: "ready", AccountCustomer: f.binding.CustomerID,
		Attempts: []workflowAttempt{{ID: "11111111-1111-4111-8111-111111111111", Status: "action_required", Amount: "100", CreatedAt: actionTime.Add(-time.Second)}},
	})
	processWorkflowBody(t, &f, action, "action-worker")
	assertWorkflowPaymentState(t, &f, purchase.PaymentActionRequired)

	failedTime := webhookNow.Add(-30 * time.Second)
	failed := workflowPaymentBody(workflowPayment{
		EventID: "evt_00000000000000000000000002", EventType: "transaction.payment_failed", OccurredAt: failedTime,
		Status: "ready", AccountCustomer: f.binding.CustomerID,
		Attempts: []workflowAttempt{{ID: "22222222-2222-4222-8222-222222222222", Status: "error", Amount: "100", CreatedAt: failedTime.Add(-time.Second)}},
	})
	processWorkflowBody(t, &f, failed, "failed-worker")
	assertWorkflowPaymentState(t, &f, purchase.PaymentFailed)

	paidTime := webhookNow.Add(-10 * time.Second)
	paid := workflowPaymentBody(workflowPayment{
		EventID: "evt_00000000000000000000000003", EventType: "transaction.completed", OccurredAt: paidTime,
		Status: "completed", AccountCustomer: f.binding.CustomerID,
		Lines: []workflowLine{{ID: "txnitm_abcdefghijklmnopqrstuvwxyz", PriceID: "pri_abcdefghijklmnopqrstuvwxyz", Quantity: 1, Total: "100", Tax: "0"}},
		Total: "100", Tax: "0", Attempts: []workflowAttempt{{ID: "33333333-3333-4333-8333-333333333333", Status: "captured", Amount: "100", CreatedAt: paidTime.Add(-2 * time.Second), CapturedAt: paidTime.Add(-time.Second)}},
	})
	processWorkflowBody(t, &f, paid, "paid-worker")
	assertWorkflowPaymentState(t, &f, purchase.PaymentPaid)
	older := workflowPaymentBody(workflowPayment{
		EventID: "evt_00000000000000000000000008", EventType: "transaction.payment_failed", OccurredAt: paidTime.Add(-time.Second),
		Status: "past_due", AccountCustomer: f.binding.CustomerID,
		Attempts: []workflowAttempt{{ID: "88888888-8888-4888-8888-888888888888", Status: "error", Amount: "100", CreatedAt: paidTime.Add(-2 * time.Second)}},
	})
	processWorkflowBody(t, &f, older, "older-stale-worker")
	assertWorkflowPaymentState(t, &f, purchase.PaymentPaid)
	assertWorkflowPaymentRejection(t, &f, "evt_00000000000000000000000008", purchase.RejectStaleObservation)

	staleTime := webhookNow
	stale := workflowPaymentBody(workflowPayment{
		EventID: "evt_00000000000000000000000004", EventType: "transaction.payment_failed", OccurredAt: staleTime,
		Status: "past_due", AccountCustomer: f.binding.CustomerID,
		Attempts: []workflowAttempt{{ID: "44444444-4444-4444-8444-444444444444", Status: "error", Amount: "100", CreatedAt: staleTime.Add(-time.Second)}},
	})
	processWorkflowBody(t, &f, stale, "stale-worker")
	assertWorkflowPaymentState(t, &f, purchase.PaymentPaid)
	assertWorkflowPaymentRejection(t, &f, "evt_00000000000000000000000004", purchase.RejectStaleObservation)
}

// A quote expires so that its price cannot be claimed forever. It does not
// expire the money: a customer who opens the checkout, is asked by their
// bank to confirm, and comes back twenty minutes later has paid, and the
// provider has the money. Refusing that left them paying for nothing,
// which is the one outcome worth avoiding.
func TestTransactionProcessorAcceptsCollectionAfterTheQuoteExpires(t *testing.T) {
	f := newPaymentFixture(t, "expired-collection", true)
	collected := f.intent.ExpiresAt.Add(time.Second)
	body := workflowPaymentBody(workflowPayment{
		EventID: "evt_00000000000000000000000005", EventType: "transaction.completed", OccurredAt: collected.Add(time.Second),
		Status: "completed", AccountCustomer: f.binding.CustomerID,
		Lines: []workflowLine{{ID: "txnitm_abcdefghijklmnopqrstuvwxyz", PriceID: "pri_abcdefghijklmnopqrstuvwxyz", Quantity: 1, Total: "100", Tax: "0"}},
		Total: "100", Tax: "0", Attempts: []workflowAttempt{{ID: "55555555-5555-4555-8555-555555555555", Status: "captured", Amount: "100", CreatedAt: collected.Add(-time.Second), CapturedAt: collected}},
	})
	processWorkflowBody(t, &f, body, "expired-worker")
	assertWorkflowPaymentState(t, &f, purchase.PaymentPaid)
	var state string
	if err := f.db.QueryRowContext(t.Context(), `SELECT state FROM billing_inbox WHERE account_id=$1 AND message_id=$2`, f.account, "evt_00000000000000000000000005").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "processed" {
		t.Fatalf("inbox state=%q, want processed", state)
	}
}

// The one collection time still refused is one that cannot have happened:
// money taken before the purchase it pays for existed. That is not a late
// customer, it is an event belonging to something else.
func TestTransactionProcessorRejectsCollectionBeforeTheIntent(t *testing.T) {
	f := newPaymentFixture(t, "impossible-collection", true)
	collected := f.intent.CreatedAt.Add(-time.Hour)
	body := workflowPaymentBody(workflowPayment{
		EventID: "evt_00000000000000000000000009", EventType: "transaction.completed", OccurredAt: collected.Add(time.Second),
		Status: "completed", AccountCustomer: f.binding.CustomerID,
		Lines: []workflowLine{{ID: "txnitm_abcdefghijklmnopqrstuvwxyz", PriceID: "pri_abcdefghijklmnopqrstuvwxyz", Quantity: 1, Total: "100", Tax: "0"}},
		Total: "100", Tax: "0", Attempts: []workflowAttempt{{ID: "99999999-9999-4999-8999-999999999999", Status: "captured", Amount: "100", CreatedAt: collected.Add(-time.Second), CapturedAt: collected}},
	})
	processWorkflowBody(t, &f, body, "impossible-worker")
	assertWorkflowPaymentRejection(t, &f, "evt_00000000000000000000000009", purchase.RejectCollectionTime)
	assertNoPaymentEffects(t, &f)
}

func TestTransactionProcessorReorderedTaxInclusiveLines(t *testing.T) {
	f := newWorkflowMoneyFixture(t)
	body := workflowPaymentBody(workflowPayment{
		EventID: "evt_00000000000000000000000006", EventType: "transaction.completed", OccurredAt: webhookNow,
		Status: "completed", AccountCustomer: f.binding.CustomerID, Total: "480", Tax: "80",
		Lines: []workflowLine{
			{ID: "txnitm_zyxwvutsrqponmlkjihgfedcba", PriceID: "pri_zyxwvutsrqponmlkjihgfedcba", Quantity: 1, Total: "240", Tax: "40"},
			{ID: "txnitm_abcdefghijklmnopqrstuvwxyz", PriceID: "pri_abcdefghijklmnopqrstuvwxyz", Quantity: 2, Total: "240", Tax: "40"},
		},
		Attempts: []workflowAttempt{{ID: "66666666-6666-4666-8666-666666666666", Status: "captured", Amount: "480", CreatedAt: webhookNow.Add(-2 * time.Second), CapturedAt: webhookNow.Add(-time.Second)}},
	})
	processWorkflowBody(t, &f, body, "multiline-worker")
	service := purchase.New(f.store.Purchases(), func() time.Time { return webhookNow.Add(time.Minute) })
	funding, err := service.Funding(t.Context(), f.account, f.intent.Scope, f.binding.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if funding.Gross != 480 || funding.Tax != 80 || len(funding.Lines) != 2 || funding.Lines[0].LineID != "line-a" || funding.Lines[0].Gross != 240 || funding.Lines[0].Tax != 40 || funding.Lines[1].LineID != "line-b" || funding.Lines[1].Gross != 240 || funding.Lines[1].Tax != 40 {
		t.Fatalf("funding=%+v", funding)
	}
	intent, err := service.Intent(t.Context(), f.account, f.intent.ID)
	if err != nil || intent.Payment != purchase.PaymentPaid || intent.Fulfillment != purchase.FulfillmentComplete {
		t.Fatalf("intent=%+v err=%v", intent, err)
	}
	balance, err := credit.New(f.store.Credits(), func() time.Time { return webhookNow.Add(time.Minute) }).Balance(t.Context(), f.account, "credits", "")
	if err != nil || balance.Available != 300 {
		t.Fatalf("credit balance=%+v err=%v, want 300", balance, err)
	}
	var fulfillments int
	if err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_purchase_fulfillments WHERE account_id=$1 AND intent_id=$2 AND state='complete'`, f.account, f.intent.ID).Scan(&fulfillments); err != nil {
		t.Fatal(err)
	}
	if fulfillments != 2 {
		t.Fatalf("complete fulfillments=%d, want 2", fulfillments)
	}
}

func TestTransactionProcessorRejectsForeignBindingOwner(t *testing.T) {
	owner := newPaymentFixture(t, "binding-owner", true)
	foreign := billing.AccountID("paddle-binding-foreign")
	if err := owner.store.CreateAccount(t.Context(), foreign, string(foreign)); err != nil {
		t.Fatal(err)
	}
	verifier := newTestWebhookVerifier(t, resolverFunc(func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error) { return foreign, nil }))
	body := workflowPaymentBody(workflowPayment{
		EventID: "evt_00000000000000000000000007", EventType: "transaction.completed", OccurredAt: webhookNow,
		Status: "completed", AccountCustomer: owner.binding.CustomerID, Total: "100", Tax: "0",
		Lines:    []workflowLine{{ID: "txnitm_abcdefghijklmnopqrstuvwxyz", PriceID: "pri_abcdefghijklmnopqrstuvwxyz", Quantity: 1, Total: "100", Tax: "0"}},
		Attempts: []workflowAttempt{{ID: "77777777-7777-4777-8777-777777777777", Status: "captured", Amount: "100", CreatedAt: webhookNow.Add(-2 * time.Second), CapturedAt: webhookNow.Add(-time.Second)}},
	})
	if _, err := integration.ReceiveWebhook(t.Context(), owner.store.Queue(), verifier, body, signedWebhookHeaders(body), webhookScope()); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := owner.store.Queue().Claim(t.Context(), integration.Inbound, "foreign-worker", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := owner.processor.Process(t.Context(), claim); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("foreign binding error=%v, want conflict", err)
	}
	var funding, events int
	if err := owner.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM billing_purchase_funding WHERE account_id=$1),(SELECT count(*) FROM billing_purchase_payment_events WHERE account_id=$1)`, foreign).Scan(&funding, &events); err != nil {
		t.Fatal(err)
	}
	if funding != 0 || events != 0 {
		t.Fatalf("foreign account funding=%d events=%d", funding, events)
	}
	var state string
	if err := owner.db.QueryRowContext(t.Context(), `SELECT state FROM billing_inbox WHERE account_id=$1 AND message_id=$2`, foreign, "evt_00000000000000000000000007").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "processing" {
		t.Fatalf("foreign inbox state=%q, want processing", state)
	}
}

type workflowPayment struct {
	EventID, EventType, Status, AccountCustomer string
	OccurredAt                                  time.Time
	Total, Tax                                  string
	Lines                                       []workflowLine
	Attempts                                    []workflowAttempt
}

type workflowLine struct {
	ID, PriceID, Total, Tax string
	Quantity                int64
}

type workflowAttempt struct {
	ID, Status, Amount string
	CreatedAt          time.Time
	CapturedAt         time.Time
}

func workflowPaymentBody(in workflowPayment) []byte {
	type wireAttempt struct {
		ID         string     `json:"payment_attempt_id"`
		Status     string     `json:"status"`
		Amount     string     `json:"amount"`
		CreatedAt  time.Time  `json:"created_at"`
		CapturedAt *time.Time `json:"captured_at"`
	}
	type wireLine struct {
		Quantity        int64  `json:"quantity"`
		ProviderID      string `json:"id"`
		ProviderPriceID string `json:"price_id"`
		Totals          struct {
			Total string `json:"total"`
			Tax   string `json:"tax"`
		} `json:"totals"`
	}
	lines := make([]wireLine, len(in.Lines))
	for i, line := range in.Lines {
		lines[i] = wireLine{Quantity: line.Quantity, ProviderID: line.ID, ProviderPriceID: line.PriceID}
		lines[i].Totals.Total, lines[i].Totals.Tax = line.Total, line.Tax
	}
	attempts := make([]wireAttempt, len(in.Attempts))
	for i, attempt := range in.Attempts {
		attempts[i] = wireAttempt{ID: attempt.ID, Status: attempt.Status, Amount: attempt.Amount, CreatedAt: attempt.CreatedAt}
		if !attempt.CapturedAt.IsZero() {
			attempts[i].CapturedAt = new(attempt.CapturedAt)
		}
	}
	payload := struct {
		EventID    string    `json:"event_id"`
		EventType  string    `json:"event_type"`
		OccurredAt time.Time `json:"occurred_at"`
		Data       struct {
			ID         string `json:"id"`
			CustomerID string `json:"customer_id"`
			Status     string `json:"status"`
			Currency   string `json:"currency_code"`
			Details    struct {
				Totals struct {
					Total           string `json:"total"`
					Tax             string `json:"tax"`
					Credit          string `json:"credit"`
					CreditToBalance string `json:"credit_to_balance"`
					GrandTotal      string `json:"grand_total"`
					Balance         string `json:"balance"`
					Currency        string `json:"currency_code"`
				} `json:"totals"`
				LineItems []wireLine `json:"line_items"`
			} `json:"details"`
			Payments []wireAttempt `json:"payments"`
		} `json:"data"`
	}{}
	payload.EventID, payload.EventType, payload.OccurredAt = in.EventID, in.EventType, in.OccurredAt
	payload.Data.ID, payload.Data.CustomerID, payload.Data.Status, payload.Data.Currency = "txn_abcdefghijklmnopqrstuvwxyz", in.AccountCustomer, in.Status, "USD"
	payload.Data.Details.Totals.Total, payload.Data.Details.Totals.Tax = in.Total, in.Tax
	payload.Data.Details.Totals.Credit, payload.Data.Details.Totals.CreditToBalance = "0", "0"
	payload.Data.Details.Totals.GrandTotal, payload.Data.Details.Totals.Balance, payload.Data.Details.Totals.Currency = in.Total, "0", "USD"
	payload.Data.Details.LineItems, payload.Data.Payments = lines, attempts
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return body
}

func processWorkflowBody(t *testing.T, f *paymentFixture, body []byte, worker string) {
	t.Helper()
	if _, err := integration.ReceiveWebhook(t.Context(), f.store.Queue(), f.verifier, body, signedWebhookHeaders(body), webhookScope()); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, worker, time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := f.processor.Process(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
}

func assertWorkflowPaymentState(t *testing.T, f *paymentFixture, want purchase.PaymentState) {
	t.Helper()
	got, err := purchase.New(f.store.Purchases(), func() time.Time { return webhookNow.Add(time.Minute) }).Intent(t.Context(), f.account, f.intent.ID)
	if err != nil || got.Payment != want {
		t.Fatalf("payment=%q err=%v, want %q", got.Payment, err, want)
	}
}

func assertWorkflowPaymentRejection(t *testing.T, f *paymentFixture, eventID, want string) {
	t.Helper()
	var got string
	if err := f.db.QueryRowContext(t.Context(), `SELECT result->>'Rejection' FROM billing_purchase_payment_events WHERE account_id=$1 AND event_id=$2`, f.account, eventID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("rejection=%q, want %q", got, want)
	}
}

func newWorkflowMoneyFixture(t *testing.T) paymentFixture {
	t.Helper()
	store, db := webhookPostgresStore(t)
	account := billing.AccountID("paddle-multiline-inclusive")
	if err := store.CreateAccount(t.Context(), account, string(account)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1) ON CONFLICT (unit_code) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	clock := webhookNow.Add(-time.Minute)
	service := purchase.New(store.Purchases(), func() time.Time { return clock })
	offer, err := service.PublishOffer(t.Context(), purchase.Offer{Account: account, Revision: purchase.Revision{ID: "credit-offer", Version: 1}, Name: "Credits", Effects: []purchase.Effect{{Key: "credits", Credit: &purchase.CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 100}}}})
	if err != nil {
		t.Fatal(err)
	}
	priceA, err := service.PublishPrice(t.Context(), purchase.Price{Account: account, Revision: purchase.Revision{ID: "pri_abcdefghijklmnopqrstuvwxyz", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 120, TaxTreatment: purchase.TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	priceB, err := service.PublishPrice(t.Context(), purchase.Price{Account: account, Revision: purchase.Revision{ID: "pri_zyxwvutsrqponmlkjihgfedcba", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 240, TaxTreatment: purchase.TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(t.Context(), purchase.QuoteInput{Account: account, ID: "quote-multiline", ValidUntil: clock.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line-a", Price: priceA.Revision, Quantity: 2}, {ID: "line-b", Price: priceB.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "paddle", Merchant: "merchant-test", Environment: "sandbox"}
	intent, err := service.CreateIntent(t.Context(), purchase.IntentInput{Account: account, ID: "intent-multiline", Operation: "operation-multiline", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "host", Reason: "paddle test", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	binding := purchase.CollectionInput{Account: account, Scope: scope, TransactionID: "txn_abcdefghijklmnopqrstuvwxyz", IntentID: intent.ID, QuoteFingerprint: quote.Fingerprint(), CustomerID: "ctm_abcdefghijklmnopqrstuvwxyz", Lines: []purchase.CollectionLine{{ProviderLineID: "txnitm_abcdefghijklmnopqrstuvwxyz", ProviderPriceID: priceA.Revision.ID, Quantity: 2, Allocations: []purchase.CollectionAllocation{{QuoteLineID: "line-a", Quantity: 2}}}, {ProviderLineID: "txnitm_zyxwvutsrqponmlkjihgfedcba", ProviderPriceID: priceB.Revision.ID, Quantity: 1, Allocations: []purchase.CollectionAllocation{{QuoteLineID: "line-b", Quantity: 1}}}}, Actor: "host", Reason: "verified checkout", EvidenceReference: "checkout-evidence"}
	if _, err := service.BindCollection(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	processor, err := paddle.NewTransactionProcessor(paddle.TransactionProcessorConfig{Merchant: "merchant-test", Environment: paddle.Sandbox, Repository: store.Queue(), Now: func() time.Time { return webhookNow.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	verifier := newTestWebhookVerifier(t, resolverFunc(func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error) { return account, nil }))
	return paymentFixture{store: store, db: db, account: account, intent: intent, binding: binding, processor: processor, verifier: verifier, clock: clock}
}
