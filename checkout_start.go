package paddle

import (
	"context"
	"errors"
	"fmt"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

// StartInput is everything needed to put a customer in front of a payment form.
type StartInput struct {
	// Account is the host account being charged.
	Account billing.AccountID
	// Operation is the idempotency key for this purchase. Replaying it replays
	// the same intent rather than creating a second one, which is what keeps a
	// double-clicked button from charging twice.
	Operation string
	// Email identifies the provider customer. Ignored when Customer is set.
	Email string
	// Name is optional customer-facing detail. Ignored when Customer is set.
	Name string
	// Customer is an already-linked provider customer. A host that persists its
	// own account-to-customer mapping — which it should — passes it here, and
	// no customer lookup happens at all.
	Customer *CustomerReference
	// Quote is the frozen commercial terms, from the billing core. Its lines
	// are what Lines must map onto.
	Quote purchase.Quote
	// Lines maps each quote line onto the provider price that settles it.
	Lines []CheckoutDispatchLine
	// Actor and Reason are the audit trail for the intent.
	Actor, Reason string
	// Worker names whoever holds the dispatch claim, for the outbox audit trail.
	Worker string
}

// Start links the customer, records the purchase intent and sends it, returning
// the provider transaction to hand to the browser.
//
// This is the whole first-capture flow. Doing it by hand means linking a
// customer, planning the checkout, claiming the outbox and dispatching, in that
// order, with the right error tolerated at each step — a replayed operation is
// a success, not a conflict, and a claim that is already held is not an error
// either. Getting that sequence subtly wrong is how a host ends up either
// charging twice or losing a purchase it told the customer had started.
//
// Dispatch happens inline because a customer is waiting on the answer. The
// outbox makes that safe: send permission is granted once, so a background
// worker that also picks the message up cannot send it a second time.
//
// The returned transaction has no redirect URL. The provider has no server-side
// API that completes a card payment, so first capture is a browser handoff: the
// host's own page loads the provider's client-side library and opens this
// transaction.
func (s *CheckoutService) Start(ctx context.Context, in StartInput) (Transaction, error) {
	if s == nil || s.client == nil {
		return Transaction{}, ErrInvalid
	}
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.Operation) ||
		!billing.ValidID(in.Actor) || in.Reason == "" || !billing.ValidID(in.Worker) ||
		len(in.Lines) == 0 {
		return Transaction{}, ErrInvalid
	}
	if in.Quote.Account != in.Account || in.Quote.ID == "" {
		return Transaction{}, ErrInvalid
	}
	// Every provider line must settle a line of this quote. Without this a host
	// can charge for something the customer was never quoted.
	quoted := make(map[string]struct{}, len(in.Quote.Lines))
	for _, line := range in.Quote.Lines {
		quoted[line.ID] = struct{}{}
	}
	for _, line := range in.Lines {
		if _, ok := quoted[line.QuoteLineID]; !ok {
			return Transaction{}, fmt.Errorf("%w: line %q is not in the quote", ErrInvalid, line.QuoteLineID)
		}
	}

	customer := in.Customer
	if customer == nil {
		linked, err := s.client.LinkCustomer(ctx, CustomerInput{
			Account: in.Account, Operation: "customer-" + in.Operation,
			Email: in.Email, Name: in.Name,
		})
		if err != nil {
			return Transaction{}, fmt.Errorf("link provider customer: %w", err)
		}
		customer = &linked
	}
	if !s.client.ValidCustomer(*customer) || customer.Account != in.Account {
		return Transaction{}, ErrInvalid
	}

	intent := purchase.IntentInput{
		Account: in.Account, ID: in.Operation, Operation: in.Operation,
		QuoteID: in.Quote.ID, QuoteFingerprint: in.Quote.Fingerprint(),
		Scope: s.client.Scope(), Actor: in.Actor, Reason: in.Reason,
		ExpiresAt: in.Quote.ValidUntil,
	}
	if _, err := s.PlanCheckout(ctx, intent, *customer, in.Lines); err != nil &&
		!errors.Is(err, billing.ErrConflict) {
		// A conflict is a replay of the same checkout: the message is already
		// queued, which is exactly the desired state.
		return Transaction{}, fmt.Errorf("plan checkout: %w", err)
	}

	claim, ok, err := s.repo.Claim(ctx, integration.Outbound, in.Worker, s.now().UTC(), time.Minute)
	if err != nil {
		return Transaction{}, fmt.Errorf("claim checkout dispatch: %w", err)
	}
	if !ok {
		// A worker holds it. The message is durable and will be sent; this
		// request simply cannot report the transaction, which is a different
		// thing from the purchase having failed.
		return Transaction{}, fmt.Errorf("%w: checkout is queued but held by another worker", billing.ErrConflict)
	}
	transaction, err := s.Dispatch(ctx, claim)
	if err != nil {
		return Transaction{}, fmt.Errorf("dispatch checkout: %w", err)
	}
	return transaction, nil
}
