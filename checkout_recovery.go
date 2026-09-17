package paddle

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"slices"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

// RecoverCheckout resumes a persisted unknown checkout. Each invocation makes
// at most 100 list requests; committed page checkpoints survive interruption.
// Neither an empty scan nor a provider error authorizes another create call.
func (s *CheckoutService) RecoverCheckout(ctx context.Context, account billing.AccountID, messageID, worker string) (Transaction, error) {
	if s == nil || s.repo == nil || s.client == nil || !billing.ValidID(string(account)) || !billing.ValidID(messageID) || !billing.ValidID(worker) {
		return Transaction{}, ErrInvalid
	}
	delivery, err := s.repo.Outbox(ctx, account, s.client.Scope(), messageID)
	if err != nil {
		return Transaction{}, err
	}
	if delivery.State != "unknown" && delivery.State != "completed" {
		return Transaction{}, billing.ErrState
	}
	payload, err := s.recoveryPayload(delivery.Message, account)
	if err != nil {
		return Transaction{}, err
	}
	intent, err := purchase.New(s.repoPurchases(), s.now).Intent(ctx, account, payload.IntentID)
	if err != nil {
		return Transaction{}, err
	}
	if intent.Scope != s.client.Scope() || intent.Operation != payload.Operation || intent.QuoteFingerprint != payload.QuoteFingerprint {
		return Transaction{}, billing.ErrConflict
	}
	claim := integration.Claim{Message: delivery.Message, Fence: delivery.Fence, Worker: worker, Attempt: delivery.Attempt}
	previous := ""
	if delivery.LastResult != nil {
		previous = delivery.LastResult.ObservationID
	}
	anchor := delivery.BegunAt
	if anchor.IsZero() {
		anchor = delivery.Message.OccurredAt
	}
	cp := scanCheckpoint{Version: 1, MessageID: messageID, Fence: delivery.Fence, From: anchor.Add(-time.Hour), Until: anchor.Add(time.Hour)}
	cp.Query = paddlewire.Digest("checkout-query-v1", messageID, delivery.Fence, payload.Correlation, cp.From, cp.Until)
	if delivery.LastResult != nil {
		switch delivery.LastResult.Evidence {
		case "provider_outcome_uncertain", "provider_response_unresolved", "provider_response_commit_uncertain", "accepted", "reconciled":
			// Initial dispatch outcomes and terminal results have no scan checkpoint.
		default:
			var old scanCheckpoint
			if json.Unmarshal([]byte(delivery.LastResult.Evidence), &old) != nil || !old.valid(cp, checkoutScanBounds) {
				return Transaction{}, errors.Join(ErrCheckoutUnresolved, ErrResponse)
			}
			cp = old
		}
	}
	if delivery.State == "completed" {
		tx, err := s.client.Transaction(ctx, billing.Reference{Scope: s.client.Scope(), ID: delivery.ProviderReference})
		if err != nil {
			return Transaction{}, err
		}
		if err := validateCheckoutTransaction(tx, payload, intent.Currency); err != nil {
			return Transaction{}, err
		}
		return tx, nil
	}
	stop := func(reason string, cause error) (Transaction, error) {
		cp.Reason = reason
		_, persistErr := s.saveCheckoutCheckpoint(ctx, claim, payload, cp, previous)
		return Transaction{}, errors.Join(ErrCheckoutUnresolved, cause, persistErr)
	}
	if delivery.ProviderReference != "" {
		tx, err := s.client.Transaction(ctx, billing.Reference{Scope: s.client.Scope(), ID: delivery.ProviderReference})
		if err != nil {
			return stop("known_lookup_failed", err)
		}
		if err := validateCheckoutTransaction(tx, payload, intent.Currency); err != nil {
			cp.Matches = nil
			cp.Conflicts = []string{tx.Reference.ID}
			return stop("known_candidate_conflict", err)
		}
		if err := s.resolveAccepted(ctx, claim, payload, tx, previous); err != nil {
			return Transaction{}, err
		}
		return tx, nil
	}
	if len(cp.Conflicts) > 0 || len(cp.Matches) > 1 {
		return stop("ambiguous_candidates", nil)
	}
	var matched Transaction
	// A stored identity is positive evidence, not a reason to ignore failed GETs.
	for _, id := range cp.Matches {
		tx, err := s.client.Transaction(ctx, billing.Reference{Scope: s.client.Scope(), ID: id})
		if err != nil {
			return stop("candidate_lookup_failed", err)
		}
		if err := validateCheckoutTransaction(tx, payload, intent.Currency); err != nil {
			cp.Matches = nil
			cp.Conflicts = []string{id}
			return stop("candidate_changed", err)
		}
		matched = tx
	}
	if cp.Complete && len(cp.Matches) == 1 {
		if err := s.resolveAccepted(ctx, claim, payload, matched, previous); err != nil {
			return Transaction{}, err
		}
		return matched, nil
	}
	// A completed empty scan is not negative proof. Re-scan the same frozen
	// window on a later invocation to allow for delayed provider visibility.
	if cp.Complete {
		cp.Cursor = ""
		cp.Pages = 0
		cp.Rows = 0
		cp.Complete = false
	}
	for range 100 {
		if cp.Pages >= 1_000_000 {
			return stop("scan_history_bound", nil)
		}
		page, err := s.client.Transactions(ctx, TransactionLookup{Customer: payload.Customer, CreatedAfter: cp.From, CreatedBefore: cp.Until, After: cp.Cursor, Limit: 30})
		if err != nil {
			return stop("page_lookup_failed", err)
		}
		for _, candidate := range page.Items {
			if !checkoutCorrelationMatches(candidate, payload) {
				continue
			}
			if err := validateCheckoutTransaction(candidate, payload, intent.Currency); err != nil {
				if !slices.Contains(cp.Conflicts, candidate.Reference.ID) && len(cp.Conflicts) < 2 {
					cp.Conflicts = append(cp.Conflicts, candidate.Reference.ID)
				}
				continue
			}
			if !slices.Contains(cp.Matches, candidate.Reference.ID) && len(cp.Matches) < 2 {
				cp.Matches = append(cp.Matches, candidate.Reference.ID)
			}
			matched = candidate
		}
		cp.Pages++
		cp.Rows += int64(len(page.Items))
		cp.Complete = !page.HasMore
		if page.HasMore {
			cp.Cursor = page.NextAfter
		}
		cp.Reason = "page"
		if len(cp.Conflicts) > 0 || len(cp.Matches) > 1 {
			return stop("ambiguous_candidates", nil)
		}
		previous, err = s.saveCheckoutCheckpoint(ctx, claim, payload, cp, previous)
		if err != nil {
			// The scan is abandoned mid-page, so the delivery is still
			// unresolved; callers dispatch on that sentinel.
			return Transaction{}, errors.Join(ErrCheckoutUnresolved, err)
		}
		if cp.Complete {
			if len(cp.Matches) == 0 {
				return Transaction{}, ErrCheckoutUnresolved
			}
			if err := s.resolveAccepted(ctx, claim, payload, matched, previous); err != nil {
				return Transaction{}, err
			}
			return matched, nil
		}
	}
	return stop("invocation_page_bound", nil)
}

// repoPurchases adapts the shared account transaction without assuming a
// PostgreSQL concrete type or opening nested database transactions.
func (s *CheckoutService) repoPurchases() purchase.Repository { return checkoutPurchases{s.repo} }

type checkoutPurchases struct{ repo integration.Repository }

func (r checkoutPurchases) WithinAccount(ctx context.Context, account billing.AccountID, fn func(purchase.Tx) error) error {
	return r.repo.Atomic(ctx, account, func(session integration.Session) error { return session.Purchases().WithinAccount(ctx, account, fn) })
}

func (s *CheckoutService) recoveryPayload(message integration.Message, account billing.AccountID) (CheckoutDispatch, error) {
	if message.Validate() != nil || message.Account != account || message.ProviderScope() != s.client.Scope() || message.Direction != integration.Outbound || message.Kind != checkoutDispatchKind {
		return CheckoutDispatch{}, ErrInvalid
	}
	payload, err := decodeDispatch(message.Payload)
	if err != nil || payload.Account != account || payload.Customer.Scope != s.client.Scope() {
		return CheckoutDispatch{}, ErrResponse
	}
	id, err := integration.ProviderMessageID(s.client.Scope().Provider, s.client.Scope().Merchant, s.client.Scope().Environment, payload.Operation)
	if err != nil || id != message.ID {
		return CheckoutDispatch{}, billing.ErrConflict
	}
	return payload, nil
}

func checkoutCorrelationMatches(tx Transaction, p CheckoutDispatch) bool {
	return tx.Reference.Scope == p.Customer.Scope && tx.Customer == p.Customer && tx.Correlation == (CheckoutCorrelation{Account: p.Account, Operation: p.Operation, IntentID: p.IntentID, QuoteFingerprint: p.QuoteFingerprint, RequestFingerprint: p.Correlation, Schema: "rho.checkout.v1"})
}

func (s *CheckoutService) saveCheckoutCheckpoint(ctx context.Context, claim integration.Claim, payload CheckoutDispatch, cp scanCheckpoint, previous string) (string, error) {
	return saveScanCheckpoint(ctx, s.repo, claim, cp, checkoutScanBounds, previous, s.now(), func(observation string) func(integration.Session) error {
		return s.commandCallback(ctx, claim.Fence, payload, purchase.CommandUnknown, "", observation)
	})
}
