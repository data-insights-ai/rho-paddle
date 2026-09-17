package paddle

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

const checkoutDispatchKind = "checkout.dispatch"

var ErrCheckoutUnresolved = errors.New("paddle: checkout outcome unresolved")

type CheckoutDispatchLine struct {
	QuoteLineID string
	Price       billing.Reference
	Quantity    int64
}

type CheckoutDispatch struct {
	CheckoutPageURL  string
	Account          billing.AccountID
	IntentID         string
	Operation        string
	QuoteFingerprint string
	Customer         billing.Reference
	Lines            []CheckoutDispatchLine
	Correlation      string
}

type CheckoutPlan struct {
	Intent  purchase.Intent
	Message integration.Message
}

type CheckoutServiceConfig struct {
	// CheckoutPageURL selects the public host Paddle.js page; empty uses the merchant default.
	CheckoutPageURL string
	Client          *Client
	Repository      integration.Repository
	Now             func() time.Time
}

type CheckoutService struct {
	checkoutPageURL string
	client          *Client
	repo            integration.Repository
	now             func() time.Time
}

func NewCheckoutService(config CheckoutServiceConfig) (*CheckoutService, error) {
	if !checkoutPageURL(config.CheckoutPageURL) || config.Client == nil || config.Client.http == nil || !config.Client.scope.Valid() || config.Repository == nil {
		return nil, ErrInvalid
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &CheckoutService{checkoutPageURL: config.CheckoutPageURL, client: config.Client, repo: config.Repository, now: config.Now}, nil
}

// PlanCheckout atomically creates or replays the intent and its one frozen
// outbound checkout message. It never contacts Paddle.
func (s *CheckoutService) PlanCheckout(ctx context.Context, in purchase.IntentInput, customer CustomerReference, lines []CheckoutDispatchLine) (CheckoutPlan, error) {
	if s == nil || s.repo == nil || s.client == nil || !s.client.ValidCustomer(customer) || in.Account != customer.Account || in.Scope != s.client.Scope() || len(lines) == 0 || len(lines) > 100 {
		return CheckoutPlan{}, ErrInvalid
	}
	canonical, err := canonicalDispatchLines(s.client.Scope(), lines)
	if err != nil {
		return CheckoutPlan{}, err
	}
	requestFP := paddlewire.Digest(in.Account, in.Operation, in.ID, in.QuoteFingerprint, customer.Reference.ID, canonical, s.checkoutPageURL)
	messageID, err := integration.ProviderMessageID(s.client.Scope().Provider, s.client.Scope().Merchant, s.client.Scope().Environment, in.Operation)
	if err != nil {
		return CheckoutPlan{}, err
	}
	payload := CheckoutDispatch{CheckoutPageURL: s.checkoutPageURL, Account: in.Account, IntentID: in.ID, Operation: in.Operation, QuoteFingerprint: in.QuoteFingerprint, Customer: customer.Reference, Lines: canonical, Correlation: requestFP}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > 1<<20 {
		return CheckoutPlan{}, ErrInvalid
	}
	message := integration.Message{Account: in.Account, ID: messageID, Scope: s.client.Scope(), Kind: checkoutDispatchKind, Direction: integration.Outbound, Payload: raw}
	var out CheckoutPlan
	err = s.repo.Atomic(ctx, in.Account, func(session integration.Session) error {
		service := purchase.New(session.Purchases(), s.now)
		intent, err := service.CreateIntent(ctx, in)
		if err != nil {
			return err
		}
		quote, err := service.Quote(ctx, intent.Account, intent.QuoteID)
		if err != nil {
			return err
		}
		if quote.Fingerprint() != intent.QuoteFingerprint || !dispatchMatchesQuote(canonical, quote) {
			return billing.ErrConflict
		}
		message.OccurredAt = intent.CreatedAt
		if err := message.Validate(); err != nil {
			return err
		}
		if err := session.Enqueue(ctx, message); err != nil {
			return err
		}
		out = CheckoutPlan{Intent: intent, Message: message}
		return nil
	})
	if err != nil {
		return CheckoutPlan{}, err
	}
	return out, nil
}

// Dispatch records the durable begin decision before making one provider call.
// Ambiguous responses are retained as unknown and never authorize a resend.
func (s *CheckoutService) Dispatch(ctx context.Context, claim integration.Claim) (Transaction, error) {
	if s == nil || s.client == nil || s.repo == nil || claim.Message.Direction != integration.Outbound || claim.Message.Kind != checkoutDispatchKind {
		return Transaction{}, ErrInvalid
	}
	payload, err := decodeDispatch(claim.Message.Payload)
	if err != nil || payload.Account != claim.Message.Account || payload.Customer.Scope != s.client.Scope() {
		return Transaction{}, ErrInvalid
	}
	expectedMessageID, err := integration.ProviderMessageID(s.client.Scope().Provider, s.client.Scope().Merchant, s.client.Scope().Environment, payload.Operation)
	if err != nil || claim.Message.ID != expectedMessageID || claim.Message.ProviderScope() != s.client.Scope() {
		return Transaction{}, billing.ErrConflict
	}
	var dispatched purchase.Intent
	send, err := s.repo.BeginOutbox(ctx, claim, func(session integration.Session) error {
		service := purchase.New(session.Purchases(), s.now)
		intent, err := service.Intent(ctx, payload.Account, payload.IntentID)
		if err != nil {
			return err
		}
		if intent.Account != payload.Account || intent.ID != payload.IntentID || intent.Scope != s.client.Scope() || intent.QuoteFingerprint != payload.QuoteFingerprint || intent.Operation != payload.Operation {
			return billing.ErrConflict
		}
		dispatched, err = service.RecordCommand(ctx, purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: dispatchOperation(claim.Message.ID, claim.Fence, "dispatch"), ExpectedRevision: intent.Revision, State: purchase.CommandDispatched, OccurredAt: intent.CreatedAt})
		return err
	})
	if err != nil {
		return Transaction{}, err
	}
	if !send {
		return Transaction{}, billing.ErrConflict
	}
	items, err := dispatchItems(s.client.Scope(), payload.Lines)
	if err != nil {
		return Transaction{}, err
	}
	tx, callErr := s.client.CreateCheckout(ctx, CheckoutInput{CheckoutPageURL: payload.CheckoutPageURL, Customer: CustomerReference{Account: payload.Account, Reference: payload.Customer}, Operation: payload.Operation, Correlation: &CheckoutCorrelation{Account: payload.Account, Operation: payload.Operation, IntentID: payload.IntentID, QuoteFingerprint: payload.QuoteFingerprint, RequestFingerprint: payload.Correlation, Schema: "rho.checkout.v1"}, Items: items})
	if callErr != nil {
		switch classifyDispatch(callErr) {
		case dispatchUnsent:
			releaseErr := s.releaseUnsent(ctx, claim, callErr)
			return Transaction{}, errors.Join(integration.ErrBackpressure, callErr, releaseErr)
		case dispatchUnknown:
			persistErr := s.finishObservation(ctx, claim, payload, dispatched, integration.OutboxUnknown, "provider_outcome_uncertain", "")
			return Transaction{}, errors.Join(ErrCheckoutUnresolved, callErr, persistErr)
		default:
			persistErr := s.finishObservation(ctx, claim, payload, dispatched, integration.OutboxRejected, "provider_rejected", "")
			return Transaction{}, errors.Join(callErr, persistErr)
		}
	}
	if err := validateCheckoutTransaction(tx, payload, dispatched.Currency); err != nil {
		persistErr := s.finishObservation(ctx, claim, payload, dispatched, integration.OutboxUnknown, "provider_response_unresolved", tx.Reference.ID)
		return Transaction{}, errors.Join(ErrCheckoutUnresolved, err, persistErr)
	}
	observation := checkoutObservation(claim.Message.ID, tx.Reference.ID, "accepted")
	err = s.repo.FinishOutbox(ctx, claim, integration.OutboxResult{ObservationID: observation, State: integration.OutboxCompleted, ProviderReference: tx.Reference.ID, Evidence: "accepted"}, func(session integration.Session) error {
		service := purchase.New(session.Purchases(), s.now)
		intent, err := service.Intent(ctx, payload.Account, payload.IntentID)
		if err != nil {
			return err
		}
		binding := collectionFromTransaction(intent, payload, tx, observation)
		if _, err := service.BindCollection(ctx, binding); err != nil {
			return err
		}
		_, err = service.RecordCommand(ctx, purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: dispatchOperation(claim.Message.ID, claim.Fence, "accepted"), ExpectedRevision: intent.Revision, State: purchase.CommandAccepted, ProviderReference: tx.Reference.ID, EvidenceReference: observation, OccurredAt: intent.CreatedAt})
		return err
	})
	if err != nil {
		unknownErr := s.finishObservation(ctx, claim, payload, dispatched, integration.OutboxUnknown, "provider_response_commit_uncertain", tx.Reference.ID)
		return Transaction{}, errors.Join(ErrCheckoutUnresolved, err, unknownErr)
	}
	return tx, nil
}

// dispatchOutcome classifies a failed provider call into the three outcomes the
// outbox distinguishes. Unsent means the provider refused before the mutation
// could take effect, so the send permission may be released and retried;
// unknown means it may have taken effect and must never be retried blindly.
type dispatchOutcome int

const (
	dispatchRejected dispatchOutcome = iota
	dispatchUnknown
	dispatchUnsent
)

func classifyDispatch(err error) dispatchOutcome {
	if errors.Is(err, ErrUncertain) || errors.Is(err, ErrResponse) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return dispatchUnknown
	}
	api, ok := errors.AsType[*APIError](err)
	if !ok {
		return dispatchRejected
	}
	switch {
	case api.StatusCode >= 500 || api.StatusCode == 0:
		return dispatchUnknown
	case api.StatusCode == http.StatusTooManyRequests,
		api.StatusCode == http.StatusUnauthorized,
		api.StatusCode == http.StatusForbidden,
		api.StatusCode == http.StatusRequestTimeout:
		// Refused before processing: rate limited, or a credential the merchant
		// can repair. Retrying is safe and dead-lettering it would strand a
		// financial operation on a transient condition.
		return dispatchUnsent
	default:
		return dispatchRejected
	}
}

// releaseDelay honours a provider Retry-After when one was supplied.
func releaseDelay(err error) time.Duration {
	if api, ok := errors.AsType[*APIError](err); ok && api.RetryAfter > 0 {
		return min(api.RetryAfter, time.Hour)
	}
	return time.Minute
}

// releaseUnsent returns the message to the queue after the provider refused it
// before any mutation. The write must outlive a cancelled dispatch context.
func (s *CheckoutService) releaseUnsent(ctx context.Context, claim integration.Claim, callErr error) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.repo.ReleaseOutbox(releaseCtx, claim, "provider_refused_before_send", s.now().Add(releaseDelay(callErr)))
}

func (s *CheckoutService) finishObservation(ctx context.Context, claim integration.Claim, payload CheckoutDispatch, dispatched purchase.Intent, state integration.OutboxState, evidence, providerReference string) error {
	observation := checkoutObservation(claim.Message.ID, providerReference, string(state)+":"+evidence)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.repo.FinishOutbox(finishCtx, claim, integration.OutboxResult{ObservationID: observation, State: state, ProviderReference: providerReference, Evidence: evidence}, func(session integration.Session) error {
		service := purchase.New(session.Purchases(), s.now)
		intent, err := service.Intent(finishCtx, payload.Account, payload.IntentID)
		if err != nil {
			return err
		}
		commandState := purchase.CommandUnknown
		if state == integration.OutboxRejected {
			commandState = purchase.CommandRejected
		}
		_, err = service.RecordCommand(finishCtx, purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: dispatchOperation(claim.Message.ID, claim.Fence, string(commandState)+":"+observation), ExpectedRevision: intent.Revision, State: commandState, ProviderReference: providerReference, EvidenceReference: observation, OccurredAt: intent.CreatedAt})
		return err
	})
}

func (s *CheckoutService) resolveAccepted(ctx context.Context, claim integration.Claim, payload CheckoutDispatch, tx Transaction, expectedPrevious string) error {
	observation := checkoutObservation(claim.Message.ID, tx.Reference.ID, "reconciled:"+expectedPrevious)
	return s.repo.ResolveOutbox(ctx, claim, integration.OutboxResult{ObservationID: observation, ExpectedPrevious: expectedPrevious, State: integration.OutboxCompleted, ProviderReference: tx.Reference.ID, Evidence: "reconciled"}, func(session integration.Session) error {
		service := purchase.New(session.Purchases(), s.now)
		intent, err := service.Intent(ctx, payload.Account, payload.IntentID)
		if err != nil {
			return err
		}
		if _, err = service.BindCollection(ctx, collectionFromTransaction(intent, payload, tx, observation)); err != nil {
			return err
		}
		_, err = service.RecordCommand(ctx, purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: dispatchOperation(claim.Message.ID, claim.Fence, "reconciled:"+observation), ExpectedRevision: intent.Revision, State: purchase.CommandReconciled, ProviderReference: tx.Reference.ID, EvidenceReference: observation, OccurredAt: intent.CreatedAt})
		return err
	})
}

func (s *CheckoutService) commandCallback(ctx context.Context, fence int64, payload CheckoutDispatch, state purchase.CommandState, reference, evidence string) func(integration.Session) error {
	return func(session integration.Session) error {
		service := purchase.New(session.Purchases(), s.now)
		intent, err := service.Intent(ctx, payload.Account, payload.IntentID)
		if err != nil {
			return err
		}
		_, err = service.RecordCommand(ctx, purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: dispatchOperation(payload.IntentID, fence, string(state)+":"+evidence), ExpectedRevision: intent.Revision, State: state, ProviderReference: reference, EvidenceReference: evidence, OccurredAt: intent.CreatedAt})
		return err
	}
}

func canonicalDispatchLines(scope billing.Scope, lines []CheckoutDispatchLine) ([]CheckoutDispatchLine, error) {
	out := slices.Clone(lines)
	slices.SortFunc(out, func(a, b CheckoutDispatchLine) int { return strings.Compare(a.QuoteLineID, b.QuoteLineID) })
	seen := make(map[string]struct{}, len(out))
	totals := make(map[string]int64, len(out))
	for _, line := range out {
		if !billing.ValidID(line.QuoteLineID) || line.Price.Scope != scope || !paddlewire.ID(line.Price.ID, "pri_") || line.Quantity <= 0 {
			return nil, ErrInvalid
		}
		if _, ok := seen[line.QuoteLineID]; ok {
			return nil, billing.ErrConflict
		}
		seen[line.QuoteLineID] = struct{}{}
		if totals[line.Price.ID] > math.MaxInt64-line.Quantity {
			return nil, billing.ErrOverflow
		}
		totals[line.Price.ID] += line.Quantity
	}
	return out, nil
}

func dispatchMatchesQuote(lines []CheckoutDispatchLine, quote purchase.Quote) bool {
	if len(lines) != len(quote.Lines) {
		return false
	}
	for _, line := range lines {
		found := false
		for _, q := range quote.Lines {
			if q.ID == line.QuoteLineID && q.Quantity == line.Quantity {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func decodeDispatch(raw []byte) (CheckoutDispatch, error) {
	var out CheckoutDispatch
	if json.Unmarshal(raw, &out) != nil || !checkoutPageURL(out.CheckoutPageURL) || !billing.ValidID(string(out.Account)) || !billing.ValidID(out.IntentID) || !billing.ValidID(out.Operation) || !out.Customer.Scope.Valid() || !paddlewire.ID(out.Customer.ID, "ctm_") || !paddlewire.Fingerprint(out.QuoteFingerprint) || !paddlewire.Fingerprint(out.Correlation) || len(out.Lines) == 0 || len(out.Lines) > 100 {
		return CheckoutDispatch{}, ErrResponse
	}
	lines, err := canonicalDispatchLines(out.Customer.Scope, out.Lines)
	if err != nil || !slices.Equal(lines, out.Lines) || paddlewire.Digest(out.Account, out.Operation, out.IntentID, out.QuoteFingerprint, out.Customer.ID, lines, out.CheckoutPageURL) != out.Correlation {
		return CheckoutDispatch{}, ErrResponse
	}
	return out, nil
}

// dispatchItems reports an overflow rather than returning nil: a nil item list
// would be dispatched as a checkout with no items at all. The canonical payload
// already rejects this, so it is a guard, not a reachable path.
func dispatchItems(scope billing.Scope, lines []CheckoutDispatchLine) ([]CheckoutItem, error) {
	grouped := make(map[string]CheckoutItem, len(lines))
	for _, line := range lines {
		item := grouped[line.Price.ID]
		item.Price = line.Price
		if item.Quantity > math.MaxInt64-line.Quantity {
			return nil, billing.ErrOverflow
		}
		item.Quantity += line.Quantity
		grouped[line.Price.ID] = item
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]CheckoutItem, 0, len(keys))
	for _, key := range keys {
		out = append(out, grouped[key])
	}
	return out, nil
}

func validateCheckoutTransaction(tx Transaction, payload CheckoutDispatch, currency string) error {
	if tx.Customer.ID != payload.Customer.ID || tx.Customer.Scope != payload.Customer.Scope || tx.Reference.Scope != payload.Customer.Scope || tx.Origin != "api" || tx.CollectionMode != CollectionAutomatic || tx.Correlation.Schema != "rho.checkout.v1" || tx.Correlation.Account != payload.Account || tx.Correlation.Operation != payload.Operation || tx.Correlation.IntentID != payload.IntentID || tx.Correlation.QuoteFingerprint != payload.QuoteFingerprint || tx.Correlation.RequestFingerprint != payload.Correlation || currency != "" && tx.Currency != currency {
		return billing.ErrConflict
	}
	byPrice := make(map[string]TransactionItem, len(tx.Items))
	for _, item := range tx.Items {
		if _, ok := byPrice[item.Price.ID]; ok {
			return billing.ErrConflict
		}
		byPrice[item.Price.ID] = item
	}
	expectedTotals := make(map[string]int64, len(payload.Lines))
	for _, line := range payload.Lines {
		if expectedTotals[line.Price.ID] > math.MaxInt64-line.Quantity {
			return billing.ErrConflict
		}
		expectedTotals[line.Price.ID] += line.Quantity
	}
	if len(expectedTotals) != len(byPrice) {
		return billing.ErrConflict
	}
	for price, total := range expectedTotals {
		item, ok := byPrice[price]
		if !ok || item.Quantity != total {
			return billing.ErrConflict
		}
	}
	return nil
}

func collectionFromTransaction(intent purchase.Intent, payload CheckoutDispatch, tx Transaction, evidence string) purchase.CollectionInput {
	byPrice := make(map[string][]CheckoutDispatchLine, len(payload.Lines))
	for _, line := range payload.Lines {
		byPrice[line.Price.ID] = append(byPrice[line.Price.ID], line)
	}
	lines := make([]purchase.CollectionLine, 0, len(tx.Items))
	for _, item := range tx.Items {
		allocations := byPrice[item.Price.ID]
		outAllocations := make([]purchase.CollectionAllocation, len(allocations))
		for i, line := range allocations {
			outAllocations[i] = purchase.CollectionAllocation{QuoteLineID: line.QuoteLineID, Quantity: line.Quantity}
		}
		lines = append(lines, purchase.CollectionLine{ProviderLineID: item.Reference.ID, ProviderPriceID: item.Price.ID, Quantity: item.Quantity, Allocations: outAllocations})
	}
	return purchase.CollectionInput{Account: intent.Account, Scope: intent.Scope, TransactionID: tx.Reference.ID, IntentID: intent.ID, QuoteFingerprint: intent.QuoteFingerprint, CustomerID: tx.Customer.ID, Lines: lines, Actor: intent.Actor, Reason: intent.Reason, EvidenceReference: evidence}
}

func dispatchOperation(messageID string, fence int64, phase string) string {
	return "checkout-dispatch-" + paddlewire.Digest(messageID, fence, phase)
}

func checkoutObservation(messageID, providerReference, state string) string {
	return paddlewire.Digest("checkout-observation-v1", messageID, providerReference, state)
}
