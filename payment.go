package paddle

import (
	"context"
	"encoding/json"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

// TransactionProcessor applies previously authenticated inbox events using the
// core's account transaction. It performs no provider requests.
type TransactionProcessor struct {
	scope      billing.Scope
	repository integration.Repository
	now        func() time.Time
	observe    integration.Observer
}

type TransactionProcessorConfig struct {
	Merchant    string
	Environment Environment
	Repository  integration.Repository
	Now         func() time.Time
	// Observer receives a financial-drift event when the provider collected
	// money that the core refused to apply. That outcome is immutable evidence
	// and is committed, so without an observer it is silent.
	Observer integration.Observer
}

// NewTransactionProcessor performs no I/O or worker startup. The host schedules
// claims; missing collection bindings remain retryable rather than guessed.
func NewTransactionProcessor(config TransactionProcessorConfig) (*TransactionProcessor, error) {
	scope, err := providerScope(config.Merchant, config.Environment)
	if err != nil || config.Repository == nil {
		return nil, ErrInvalid
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &TransactionProcessor{scope: scope, repository: config.Repository, now: config.Now, observe: config.Observer}, nil
}

// Process applies funding and product effects atomically with inbox completion.
// A failure, including COMMIT failure, leaves no successful processing result.
// Hosts must only claim messages received through authenticated verification.
func (p *TransactionProcessor) Process(ctx context.Context, claim integration.Claim) error {
	if p == nil || p.repository == nil || p.now == nil || claim.Message.ProviderScope() != p.scope || claim.Message.Direction != integration.Inbound {
		return ErrInvalid
	}
	if err := claim.Message.Validate(); err != nil {
		return err
	}
	event, err := decodeEvent(claim.Message.Payload)
	if err != nil {
		return err
	}
	if event.ID != claim.Message.ID || event.Type != claim.Message.Kind || !event.OccurredAt.Equal(claim.Message.OccurredAt) {
		return billing.ErrConflict
	}
	// Filter the event type before the claim is processed. Returning it from
	// inside ProcessInbox would roll the transaction back and retry an event
	// that can never succeed until it dead-letters.
	if !supportedTransactionEvent(event.Type) {
		return &billing.CapabilityError{Capability: billing.Capability{Operation: "process_transaction_event", Support: billing.SupportUnsupported, Reason: "event_type_not_supported"}}
	}
	var wire paddlewire.Payment
	if err := json.Unmarshal(event.data, &wire); err != nil || !paddlewire.ID(wire.ID, "txn_") {
		return ErrResponse
	}
	var rejected string
	err = p.repository.ProcessInbox(ctx, claim, func(session integration.Session) error {
		service := purchase.New(session.Purchases(), p.now)
		binding, err := service.CollectionBinding(ctx, claim.Message.Account, p.scope, wire.ID)
		if err != nil {
			return err
		}
		intent, err := service.Intent(ctx, claim.Message.Account, binding.IntentID)
		if err != nil {
			return err
		}
		quote, err := service.Quote(ctx, intent.Account, intent.QuoteID)
		if err != nil {
			return err
		}
		fact, rekeyed, err := normalizePayment(event, wire, binding, intent, quote)
		if err != nil {
			return err
		}
		if rekeyed != nil {
			// The provider re-issued its line ids since the checkout was
			// bound. The paid transaction is the evidence refunds and
			// adjustments will reference, so the binding follows it, in the
			// same transaction as the payment fact.
			if _, err := service.RekeyCollectionLines(ctx, purchase.RekeyInput{
				Account: intent.Account, Scope: intent.Scope, TransactionID: wire.ID, Lines: rekeyed,
				Actor: "paddle-transaction-processor", Reason: "provider re-issued line ids on " + event.ID,
			}); err != nil {
				return err
			}
		}
		result, err := service.ApplyPayment(ctx, fact)
		if err != nil {
			return err
		}
		if !result.Applied {
			// A recorded rejection is immutable evidence and commits with the
			// inbox result; retrying would replay the same rejection forever.
			// Money may have been collected without any effect being granted,
			// so it is reported as financial drift rather than swallowed.
			rejected = result.Rejection
		}
		return nil
	})
	if err != nil {
		return err
	}
	if rejected != "" && p.observe != nil {
		p.observe(integration.Event{Kind: integration.EventDrift, Account: claim.Message.Account, Reason: "payment_rejected:" + rejected})
	}
	return nil
}

// normalizePayment turns a provider transaction into the payment fact the
// purchase service applies. For a paid or completed transaction it also
// reports, as rekeyed, the bound lines under the provider's current line ids
// when those differ from the ids bound at checkout; nil when they match.
func normalizePayment(event Event, wire paddlewire.Payment, binding purchase.CollectionBinding, intent purchase.Intent, quote purchase.Quote) (purchase.PaymentFact, []purchase.CollectionLine, error) {
	if binding.Validate() != nil || intent.Validate() != nil || binding.Account != intent.Account || binding.IntentID != intent.ID || binding.Scope != intent.Scope || binding.QuoteFingerprint != intent.QuoteFingerprint || binding.TransactionID != wire.ID || !paddlewire.ID(wire.CustomerID, "ctm_") || binding.CustomerID != wire.CustomerID || wire.Currency != intent.Currency {
		return purchase.PaymentFact{}, nil, billing.ErrConflict
	}
	if quote.Validate() != nil || quote.Account != intent.Account || quote.ID != intent.QuoteID || quote.Fingerprint() != intent.QuoteFingerprint {
		return purchase.PaymentFact{}, nil, billing.ErrConflict
	}
	occurredAt := billing.CanonicalTime(event.OccurredAt)
	status, err := paymentStatus(event.Type, wire, occurredAt)
	if err != nil {
		return purchase.PaymentFact{}, nil, err
	}
	fact := purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, EventID: event.ID, TransactionID: wire.ID, IntentID: intent.ID, Status: status, Currency: wire.Currency, OccurredAt: occurredAt}
	if status != purchase.FactPaid && status != purchase.FactCompleted {
		return fact, nil, nil
	}
	totals := wire.Details.Totals
	if totals.Currency != wire.Currency || len(wire.Details.Lines) != len(binding.Lines) || len(wire.Payments) > 1000 {
		return purchase.PaymentFact{}, nil, ErrResponse
	}
	gross, err := paddlewire.MinorUnits(totals.Total)
	if err != nil {
		return purchase.PaymentFact{}, nil, err
	}
	tax, err := paddlewire.MinorUnits(totals.Tax)
	if err != nil || tax > gross {
		return purchase.PaymentFact{}, nil, ErrResponse
	}
	credit, err := paddlewire.MinorUnits(totals.Credit)
	if err != nil || credit > gross {
		return purchase.PaymentFact{}, nil, ErrResponse
	}
	if credit != 0 {
		return purchase.PaymentFact{}, nil, unresolvedPayment("provider_credit_allocation_unresolved")
	}
	grand, err := paddlewire.MinorUnits(totals.GrandTotal)
	if err != nil || gross-credit != grand {
		return purchase.PaymentFact{}, nil, ErrResponse
	}
	if totals.CreditToBalance != "0" || totals.Balance != "0" {
		return purchase.PaymentFact{}, nil, unresolvedPayment("nonzero_balance_or_generated_credit")
	}
	if grand == 0 {
		return purchase.PaymentFact{}, nil, unresolvedPayment("collection_timestamp_missing_for_credit_only_payment")
	}
	// Paddle re-issues line item ids whenever it recomputes a transaction
	// (an address arrives, tax is applied), so the id bound at checkout is
	// not a stable identity. A provider line is matched to a bound line by
	// its id when that still holds, otherwise by price and quantity among
	// the bound lines not yet matched; lines that agree on both are
	// commercially interchangeable, so which one is taken changes nothing.
	byID := make(map[string]purchase.CollectionLine, len(binding.Lines))
	for _, line := range binding.Lines {
		byID[line.ProviderLineID] = line
	}
	unmatched := make([]purchase.CollectionLine, 0, len(binding.Lines))
	matchLine := func(id, priceID string, quantity int64) (purchase.CollectionLine, bool) {
		if mapped, ok := byID[id]; ok && mapped.ProviderPriceID == priceID && mapped.Quantity == quantity {
			delete(byID, id)
			return mapped, true
		}
		for i, mapped := range unmatched {
			if mapped.ProviderPriceID == priceID && mapped.Quantity == quantity {
				unmatched = append(unmatched[:i], unmatched[i+1:]...)
				return mapped, true
			}
		}
		return purchase.CollectionLine{}, false
	}
	var sumGross, sumTax int64
	type collectedLine struct {
		binding    purchase.CollectionLine
		gross, tax int64
	}
	var collectedLines []collectedLine
	var rekeyed []purchase.CollectionLine
	renamed := false
	// Lines whose bound id no longer appears become candidates for matching
	// by content once every id match has been taken.
	present := make(map[string]struct{}, len(wire.Details.Lines))
	for _, line := range wire.Details.Lines {
		present[line.ID] = struct{}{}
	}
	for _, line := range binding.Lines {
		if _, ok := present[line.ProviderLineID]; !ok {
			unmatched = append(unmatched, line)
			delete(byID, line.ProviderLineID)
		}
	}
	for _, line := range wire.Details.Lines {
		if !paddlewire.ID(line.ID, "txnitm_") || !paddlewire.ID(line.PriceID, "pri_") {
			return purchase.PaymentFact{}, nil, billing.ErrConflict
		}
		mapped, ok := matchLine(line.ID, line.PriceID, line.Quantity)
		if !ok {
			return purchase.PaymentFact{}, nil, billing.ErrConflict
		}
		if mapped.ProviderLineID != line.ID {
			renamed = true
		}
		current := mapped
		current.ProviderLineID = line.ID
		rekeyed = append(rekeyed, current)
		lineGross, err := paddlewire.MinorUnits(line.Totals.Total)
		if err != nil {
			return purchase.PaymentFact{}, nil, err
		}
		lineTax, err := paddlewire.MinorUnits(line.Totals.Tax)
		if err != nil || lineTax > lineGross {
			return purchase.PaymentFact{}, nil, ErrResponse
		}
		sumGross, err = paddlewire.AddMoney(sumGross, lineGross)
		if err != nil {
			return purchase.PaymentFact{}, nil, err
		}
		sumTax, err = paddlewire.AddMoney(sumTax, lineTax)
		if err != nil {
			return purchase.PaymentFact{}, nil, err
		}
		collectedLines = append(collectedLines, collectedLine{mapped, lineGross, lineTax})
	}
	if sumGross != gross || sumTax != tax {
		return purchase.PaymentFact{}, nil, ErrResponse
	}
	byQuoteLine := quoteLineIndex(quote)
	for _, line := range collectedLines {
		allocated, err := allocateCollectionMoney(line.binding, quote, byQuoteLine, line.gross, line.tax)
		if err != nil {
			return purchase.PaymentFact{}, nil, err
		}
		fact.Lines = append(fact.Lines, allocated...)
	}
	var captured int64
	for _, attempt := range wire.Payments {
		if attempt.Status != "captured" {
			continue
		}
		// paymentStatus has validated every attempt's identity and chronology.
		capturedAt := billing.CanonicalTime(*attempt.CapturedAt)
		amount, err := paddlewire.MinorUnits(attempt.Amount)
		if err != nil || amount == 0 {
			return purchase.PaymentFact{}, nil, ErrResponse
		}
		captured, err = paddlewire.AddMoney(captured, amount)
		if err != nil {
			return purchase.PaymentFact{}, nil, err
		}
		if capturedAt.After(fact.CollectedAt) {
			fact.CollectedAt = capturedAt
		}
	}
	if captured != grand || fact.CollectedAt.IsZero() {
		return purchase.PaymentFact{}, nil, unresolvedPayment("captured_payment_evidence_incomplete")
	}
	fact.Gross, fact.Tax = gross, tax
	if !renamed {
		rekeyed = nil
	}
	return fact, rekeyed, nil
}

// supportedTransactionEvent is the single list of transaction events this
// adapter maps. paymentStatus cross-checks each one against its status.
func supportedTransactionEvent(kind string) bool {
	switch kind {
	case "transaction.created", "transaction.ready", "transaction.billed", "transaction.updated",
		"transaction.paid", "transaction.completed", "transaction.payment_failed",
		"transaction.past_due", "transaction.canceled":
		return true
	default:
		return false
	}
}

func paymentStatus(kind string, wire paddlewire.Payment, occurredAt time.Time) (purchase.PaymentFactStatus, error) {
	if !supportedTransactionEvent(kind) {
		return "", &billing.CapabilityError{Capability: billing.Capability{Operation: "process_transaction_event", Support: billing.SupportUnsupported, Reason: "event_type_not_supported"}}
	}
	if err := validatePaymentAttempts(wire, occurredAt); err != nil {
		return "", err
	}
	if wire.Status != "draft" && wire.Status != "ready" && wire.Status != "billed" && wire.Status != "past_due" && wire.Status != "canceled" && wire.Status != "paid" && wire.Status != "completed" {
		return "", ErrResponse
	}
	switch kind {
	case "transaction.ready":
		if wire.Status != "ready" {
			return "", ErrResponse
		}
	case "transaction.billed":
		if wire.Status != "billed" {
			return "", ErrResponse
		}
	case "transaction.past_due":
		if wire.Status != "past_due" {
			return "", ErrResponse
		}
	case "transaction.canceled":
		if wire.Status != "canceled" {
			return "", ErrResponse
		}
	case "transaction.paid":
		if wire.Status != "paid" {
			return "", ErrResponse
		}
	case "transaction.completed":
		if wire.Status != "completed" {
			return "", ErrResponse
		}
	}
	if kind == "transaction.payment_failed" && (wire.Status == "paid" || wire.Status == "completed") {
		return "", ErrResponse
	}
	if kind == "transaction.payment_failed" {
		state, err := latestPaymentAttempt(wire)
		if err != nil {
			return "", err
		}
		// A soft decline that issues an SCA challenge is recoverable. Reporting
		// it as terminal would abandon a checkout the customer can still finish.
		if state == "action_required" {
			return purchase.FactActionRequired, nil
		}
		return purchase.FactFailed, nil
	}
	switch wire.Status {
	case "paid":
		return purchase.FactPaid, nil
	case "completed":
		return purchase.FactCompleted, nil
	case "canceled", "past_due":
		if _, err := latestPaymentAttempt(wire); err != nil {
			return "", err
		}
		return purchase.FactFailed, nil
	case "draft", "ready", "billed":
	default:
		return "", ErrResponse
	}
	state, err := latestPaymentAttempt(wire)
	if err != nil {
		return "", err
	}
	switch state {
	case "action_required":
		return purchase.FactActionRequired, nil
	case "error", "dropped", "canceled":
		return purchase.FactFailed, nil
	case "", "created", "unknown", "authorized", "authorized_flagged", "pending_no_action_required", "captured":
		return purchase.FactPending, nil
	default:
		return "", ErrResponse
	}
}

func latestPaymentAttempt(wire paddlewire.Payment) (string, error) {
	var latest time.Time
	var state string
	var ambiguous bool
	for _, attempt := range wire.Payments {
		createdAt := billing.CanonicalTime(attempt.CreatedAt)
		if createdAt.Equal(latest) && attempt.Status != state {
			ambiguous = true
		}
		if createdAt.After(latest) {
			latest, state = createdAt, attempt.Status
			ambiguous = false
		}
	}
	if ambiguous {
		return "", billing.ErrConflict
	}
	return state, nil
}

func validatePaymentAttempts(wire paddlewire.Payment, occurredAt time.Time) error {
	if len(wire.Payments) > 1000 {
		return ErrResponse
	}
	seen := make(map[string]struct{}, len(wire.Payments))
	for _, attempt := range wire.Payments {
		if !paymentAttemptID(attempt.ID) || attempt.CreatedAt.IsZero() {
			return ErrResponse
		}
		identity := strings.ToLower(attempt.ID)
		if _, ok := seen[identity]; ok {
			return ErrResponse
		}
		seen[identity] = struct{}{}
		createdAt := billing.CanonicalTime(attempt.CreatedAt)
		if createdAt.IsZero() || createdAt.After(occurredAt) {
			return ErrResponse
		}
		switch attempt.Status {
		case "error", "dropped", "canceled", "action_required", "created", "unknown", "authorized", "authorized_flagged", "pending_no_action_required":
			if attempt.CapturedAt != nil {
				return ErrResponse
			}
		case "captured":
			if attempt.CapturedAt == nil {
				return ErrResponse
			}
			capturedAt := billing.CanonicalTime(*attempt.CapturedAt)
			if capturedAt.IsZero() || createdAt.After(capturedAt) || capturedAt.After(occurredAt) {
				return ErrResponse
			}
		default:
			return ErrResponse
		}
	}
	return nil
}

func paymentAttemptID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, ch := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if ch != '-' {
				return false
			}
			continue
		}
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') && (ch < 'A' || ch > 'F') {
			return false
		}
	}
	return true
}

func unresolvedPayment(reason string) error {
	return &billing.CapabilityError{Capability: billing.Capability{Operation: "normalize_payment", Support: billing.SupportUnresolved, Reason: reason}}
}
