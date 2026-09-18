package paddle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	corepostgres "github.com/data-insights-ai/rho-billing/postgres"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

type paymentFixture struct {
	store     *corepostgres.Store
	db        *sql.DB
	account   billing.AccountID
	intent    purchase.Intent
	binding   purchase.CollectionInput
	processor *paddle.TransactionProcessor
	verifier  *paddle.WebhookVerifier
	clock     time.Time
}

func TestTransactionProcessorPaidCompletedReplayAndCaptureTimestamp(t *testing.T) {
	f := newPaymentFixture(t, "paid-replay", true)
	if err := f.process(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	if err := f.process(t, "evt_zyxwvutsrqponmlkjihgfedcba", "transaction.paid", "paid", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	if err := f.receiveOnly(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "replay-worker", time.Now().UTC().Add(time.Minute), time.Minute); err != nil || ok {
		t.Fatalf("duplicate claim ok=%v err=%v, want no pending duplicate", ok, err)
	}
	assertPaymentEffects(t, &f)
	var fundingCount, lotCount int
	if err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_purchase_funding WHERE account_id=$1`, f.account).Scan(&fundingCount); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_lots WHERE account_id=$1`, f.account).Scan(&lotCount); err != nil {
		t.Fatal(err)
	}
	if fundingCount != 1 || lotCount != 1 {
		t.Fatalf("funding=%d lots=%d, want one each", fundingCount, lotCount)
	}
	funding, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).Funding(t.Context(), f.account, f.intent.Scope, f.binding.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if !funding.PaidAt.Equal(f.clock) {
		t.Fatalf("PaidAt=%v, want capture timestamp %v", funding.PaidAt, f.clock)
	}
}

func TestTransactionProcessorMissingBindingRetriesAfterBind(t *testing.T) {
	f := newPaymentFixture(t, "binding-retry", false)
	if err := f.receiveOnly(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "binding-worker", time.Now().UTC(), 20*time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := f.processor.Process(t.Context(), claim); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("missing binding error=%v, want not found", err)
	}
	assertNoPaymentEffects(t, &f)
	if _, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock }).BindCollection(t.Context(), f.binding); err != nil {
		t.Fatal(err)
	}
	expireInboxClaim(t, f.db, "evt_abcdefghijklmnopqrstuvwxyz")
	if err := f.processClaimed(t, "binding-worker-retry"); err != nil {
		t.Fatal(err)
	}
	assertPaymentEffects(t, &f)
}

func TestTransactionProcessorMismatchesRollbackInboxEffects(t *testing.T) {
	tests := []struct {
		name, currency, customer, line, price string
	}{
		{name: "customer", currency: "USD", customer: "ctm_zyxwvutsrqponmlkjihgfedcba", line: "txnitm_abcdefghijklmnopqrstuvwxyz", price: "pri_abcdefghijklmnopqrstuvwxyz"},
		{name: "currency", currency: "EUR", customer: "ctm_abcdefghijklmnopqrstuvwxyz", line: "txnitm_abcdefghijklmnopqrstuvwxyz", price: "pri_abcdefghijklmnopqrstuvwxyz"},
		// A line id alone is not evidence of a mismatch: the provider re-issues
		// line ids (see TestTransactionProcessorAcceptsReissuedLineIDsAndRekeysBinding).
		// A different price under the bound id, or under a new one, is.
		{name: "price", currency: "USD", customer: "ctm_abcdefghijklmnopqrstuvwxyz", line: "txnitm_abcdefghijklmnopqrstuvwxyz", price: "pri_zyxwvutsrqponmlkjihgfedcba"},
		{name: "price under new line id", currency: "USD", customer: "ctm_abcdefghijklmnopqrstuvwxyz", line: "txnitm_zyxwvutsrqponmlkjihgfedcba", price: "pri_zyxwvutsrqponmlkjihgfedcba"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPaymentFixture(t, "mismatch-"+tt.name, true)
			if err := f.receiveOnly(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", tt.currency, tt.customer, tt.line, tt.price); err != nil {
				t.Fatal(err)
			}
			claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "mismatch-worker", time.Now().UTC(), time.Minute)
			if err != nil || !ok {
				t.Fatalf("claim ok=%v err=%v", ok, err)
			}
			if err := f.processor.Process(t.Context(), claim); !errors.Is(err, billing.ErrConflict) {
				t.Fatalf("mismatched provider evidence error=%v, want ErrConflict", err)
			}
			assertNoPaymentEffects(t, &f)
		})
	}
}

func TestTransactionProcessorDeferredCommitRollsBackAndRetries(t *testing.T) {
	f := newPaymentFixture(t, "commit-retry", true)
	if _, err := f.db.ExecContext(t.Context(), `CREATE FUNCTION paddle_payment_commit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'paddle payment commit failure'; END $$; CREATE CONSTRAINT TRIGGER paddle_payment_commit_failure AFTER INSERT ON billing_purchase_payment_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION paddle_payment_commit_failure()`); err != nil {
		t.Fatal(err)
	}
	if err := f.receiveOnly(t, "evt_abcdefghijklmnopqrstuvwxyz", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_abcdefghijklmnopqrstuvwxyz", "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, "commit-worker", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := f.processor.Process(t.Context(), claim); err == nil || !strings.Contains(err.Error(), "paddle payment commit failure") {
		t.Fatalf("deferred commit error=%v, want injected trigger failure", err)
	}
	assertNoPaymentEffects(t, &f)
	var state string
	if err := f.db.QueryRowContext(t.Context(), `SELECT state FROM billing_inbox WHERE account_id=$1 AND message_id=$2`, f.account, "evt_abcdefghijklmnopqrstuvwxyz").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "processing" {
		t.Fatalf("inbox state=%s after rollback, want processing claim retained for lease recovery", state)
	}
	if _, err := f.db.ExecContext(t.Context(), `DROP TRIGGER paddle_payment_commit_failure ON billing_purchase_payment_events; DROP FUNCTION paddle_payment_commit_failure()`); err != nil {
		t.Fatal(err)
	}
	expireInboxClaim(t, f.db, "evt_abcdefghijklmnopqrstuvwxyz")
	if err := f.processClaimed(t, "commit-worker-retry"); err != nil {
		t.Fatal(err)
	}
	assertPaymentEffects(t, &f)
}

func newPaymentFixture(t *testing.T, suffix string, bind bool) paymentFixture {
	t.Helper()
	store, db := webhookPostgresStore(t)
	ctx := t.Context()
	account := billing.AccountID("paddle-" + suffix)
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1) ON CONFLICT (unit_code) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	clock := webhookNow.Add(-time.Minute)
	service := purchase.New(store.Purchases(), func() time.Time { return clock })
	offer, err := service.PublishOffer(ctx, purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer-" + suffix, Version: 1}, Name: "Paddle payment", Effects: []purchase.Effect{{Key: "credits", Credit: &purchase.CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 100}}}, PublishedAt: clock})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(ctx, purchase.Price{Account: account, Revision: purchase.Revision{ID: "pri_abcdefghijklmnopqrstuvwxyz", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive, PublishedAt: clock})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, purchase.QuoteInput{Account: account, ID: "quote-" + suffix, ValidUntil: clock.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "paddle", Merchant: "merchant-test", Environment: "sandbox"}
	intent, err := service.CreateIntent(ctx, purchase.IntentInput{Account: account, ID: "intent-" + suffix, Operation: "operation-" + suffix, QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "host", Reason: "paddle test", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	binding := purchase.CollectionInput{Account: account, Scope: scope, TransactionID: "txn_abcdefghijklmnopqrstuvwxyz", IntentID: intent.ID, QuoteFingerprint: quote.Fingerprint(), CustomerID: "ctm_abcdefghijklmnopqrstuvwxyz", Lines: []purchase.CollectionLine{{ProviderLineID: "txnitm_abcdefghijklmnopqrstuvwxyz", ProviderPriceID: price.Revision.ID, Quantity: 1, Allocations: []purchase.CollectionAllocation{{QuoteLineID: "line", Quantity: 1}}}}, Actor: "host", Reason: "verified checkout", EvidenceReference: "checkout-evidence"}
	if bind {
		if _, err := service.BindCollection(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}
	processor, err := paddle.NewTransactionProcessor(paddle.TransactionProcessorConfig{Merchant: "merchant-test", Environment: paddle.Sandbox, Repository: store.Queue(), Now: func() time.Time { return webhookNow.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	verifier := newTestWebhookVerifier(t, resolverFunc(func(context.Context, billing.Scope, paddle.Event) (billing.AccountID, error) { return account, nil }))
	return paymentFixture{store: store, db: db, account: account, intent: intent, binding: binding, processor: processor, verifier: verifier, clock: clock}
}

func (f *paymentFixture) receiveOnly(t *testing.T, eventID, eventType, status, currency, customer, line, price string) error {
	t.Helper()
	body := paddlePaymentBody(eventID, eventType, status, currency, customer, line, price, webhookNow, webhookNow.Add(-time.Minute))
	_, err := integration.ReceiveWebhook(t.Context(), f.store.Queue(), f.verifier, body, signedWebhookHeaders(body), webhookScope())
	return err
}

func (f *paymentFixture) process(t *testing.T, eventID, eventType, status, currency, customer, line, price string) error {
	if err := f.receiveOnly(t, eventID, eventType, status, currency, customer, line, price); err != nil {
		return err
	}
	return f.processClaimed(t, "paddle-worker")
}

func (f *paymentFixture) processClaimed(t *testing.T, worker string) error {
	t.Helper()
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Inbound, worker, time.Now().UTC().Add(time.Minute), time.Minute)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no inbound claim")
	}
	return f.processor.Process(t.Context(), claim)
}

func expireInboxClaim(t *testing.T, db *sql.DB, eventID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `UPDATE billing_inbox SET lease_deadline=clock_timestamp()-interval '1 second' WHERE message_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
}

func assertNoPaymentEffects(t *testing.T, f *paymentFixture) {
	t.Helper()
	var funding, lots int
	if err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_purchase_funding WHERE account_id=$1`, f.account).Scan(&funding); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_lots WHERE account_id=$1`, f.account).Scan(&lots); err != nil {
		t.Fatal(err)
	}
	if funding != 0 || lots != 0 {
		t.Fatalf("failed payment left funding=%d lots=%d", funding, lots)
	}
}

func assertPaymentEffects(t *testing.T, f *paymentFixture) {
	t.Helper()
	balance, err := credit.New(f.store.Credits(), func() time.Time { return f.clock.Add(time.Minute) }).Balance(t.Context(), f.account, "credits", "")
	if err != nil || balance.Available != 100 {
		t.Fatalf("payment balance=%+v err=%v", balance, err)
	}
}

func paddlePaymentBody(eventID, eventType, status, currency, customer, line, price string, occurred, captured time.Time) []byte {
	return []byte(fmt.Sprintf(`{"event_id":%q,"event_type":%q,"occurred_at":%q,"data":{"id":%q,"customer_id":%q,"status":%q,"currency_code":%q,"details":{"totals":{"total":"100","tax":"0","credit":"0","credit_to_balance":"0","grand_total":"100","balance":"0","currency_code":%q},"line_items":[{"id":%q,"price_id":%q,"quantity":1,"totals":{"total":"100","tax":"0"}}]},"payments":[{"payment_attempt_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status":"captured","amount":"100","created_at":%q,"captured_at":%q}]}}`, eventID, eventType, occurred.Format(time.RFC3339Nano), "txn_abcdefghijklmnopqrstuvwxyz", customer, status, currency, currency, line, price, captured.Add(-time.Second).Format(time.RFC3339Nano), captured.Format(time.RFC3339Nano)))
}

// Paddle re-issues transaction line item ids when it recomputes a transaction
// (an address arrives, tax is applied), so the id bound at checkout is not the
// id on the paid transaction. The payment must still apply, matched by price
// and quantity, and the binding must follow the paid transaction's ids because
// refunds and adjustments will reference those.
func TestTransactionProcessorAcceptsReissuedLineIDsAndRekeysBinding(t *testing.T) {
	f := newPaymentFixture(t, "rekey", true)
	ctx := t.Context()
	const reissued = "txnitm_zyxwvutsrqponmlkjihgfedcba"
	if err := f.process(t, "evt_rekeypaidabcdefghijklmnopq", "transaction.paid", "paid", "USD", f.binding.CustomerID, reissued, "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatalf("paid with re-issued line id: %v", err)
	}
	service := purchase.New(f.store.Purchases(), func() time.Time { return f.clock })
	intent, err := service.Intent(ctx, f.account, f.intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Payment != purchase.PaymentPaid {
		t.Fatalf("intent payment = %q, want paid", intent.Payment)
	}
	binding, err := service.CollectionBinding(ctx, f.account, f.binding.Scope, f.binding.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(binding.Lines) != 1 || binding.Lines[0].ProviderLineID != reissued || binding.Lines[0].ProviderPriceID != "pri_abcdefghijklmnopqrstuvwxyz" || binding.Lines[0].Quantity != 1 {
		t.Fatalf("binding after payment = %+v, want the re-issued id with unchanged content", binding.Lines)
	}
	if binding.IntentID != f.binding.IntentID || binding.QuoteFingerprint != f.binding.QuoteFingerprint || len(binding.Lines[0].Allocations) != 1 {
		t.Fatalf("rekey changed more than the line id: %+v", binding.CollectionInput)
	}
	// The completed event then carries the same re-issued id: an ordinary
	// replay, nothing to rekey.
	if err := f.process(t, "evt_rekeycompletedabcdefghijkl", "transaction.completed", "completed", "USD", f.binding.CustomerID, reissued, "pri_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatalf("completed after rekey: %v", err)
	}
	// A different price under a new id is not a re-issue; it is a different
	// purchase and is refused.
	if err := f.process(t, "evt_rekeyotherpriceabcdefghijk", "transaction.completed", "completed", "USD", f.binding.CustomerID, "txnitm_otherabcdefghijklmnopqrstu", "pri_otherabcdefghijklmnopqrstu"); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("different price under new id: %v, want ErrConflict", err)
	}
}
