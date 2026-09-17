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
)

// RecoverRefund resumes a persisted unknown refund create. An empty scan is
// not rejection; a second create is never authorized from this path.
func (s *RefundService) RecoverRefund(ctx context.Context, account billing.AccountID, messageID, worker string) (Adjustment, error) {
	if s == nil || s.repo == nil || s.client == nil || !billing.ValidID(string(account)) || !billing.ValidID(messageID) || !billing.ValidID(worker) {
		return Adjustment{}, ErrInvalid
	}
	delivery, err := s.repo.Outbox(ctx, account, s.client.Scope(), messageID)
	if err != nil {
		return Adjustment{}, err
	}
	if delivery.State != "unknown" && delivery.State != "completed" {
		return Adjustment{}, billing.ErrState
	}
	payload, err := s.recoveryPayload(delivery.Message, account)
	if err != nil {
		return Adjustment{}, err
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
	cp.Query = paddlewire.Digest("refund-query-v1", messageID, delivery.Fence, payload.Correlation, cp.From, cp.Until)
	if delivery.LastResult != nil {
		switch delivery.LastResult.Evidence {
		case "provider_outcome_uncertain", "provider_response_unresolved", "provider_response_commit_uncertain", "accepted", "reconciled":
		default:
			var old scanCheckpoint
			if json.Unmarshal([]byte(delivery.LastResult.Evidence), &old) != nil || !old.valid(cp, refundScanBounds) {
				return Adjustment{}, errors.Join(ErrRefundUnresolved, ErrResponse)
			}
			cp = old
		}
	}
	if delivery.State == "completed" {
		got, _, err := s.lookupKnown(ctx, delivery.ProviderReference, payload)
		return got, err
	}
	stop := func(reason string, cause error) (Adjustment, error) {
		cp.Reason = reason
		_, persistErr := s.saveCheckpoint(ctx, claim, payload, cp, previous)
		return Adjustment{}, errors.Join(ErrRefundUnresolved, cause, persistErr)
	}
	if delivery.ProviderReference != "" {
		got, changed, err := s.lookupKnown(ctx, delivery.ProviderReference, payload)
		if err != nil {
			if changed {
				cp.Matches = nil
				cp.Conflicts = []string{delivery.ProviderReference}
				return stop("candidate_changed", err)
			}
			return stop("known_lookup_failed", err)
		}
		if err := s.resolveAccepted(ctx, claim, payload, got, previous); err != nil {
			return Adjustment{}, err
		}
		return got, nil
	}
	if len(cp.Conflicts) > 0 || len(cp.Matches) > 1 {
		return stop("ambiguous_candidates", nil)
	}
	var matched Adjustment
	// A stored identity is positive evidence, not a reason to ignore failed GETs.
	for _, id := range cp.Matches {
		got, changed, err := s.lookupKnown(ctx, id, payload)
		if err != nil {
			if changed {
				cp.Matches = nil
				cp.Conflicts = []string{id}
				return stop("candidate_changed", err)
			}
			return stop("candidate_lookup_failed", err)
		}
		matched = got
	}
	if cp.Complete && len(cp.Matches) == 1 {
		if err := s.resolveAccepted(ctx, claim, payload, matched, previous); err != nil {
			return Adjustment{}, err
		}
		return matched, nil
	}
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
		page, err := s.client.Adjustments(ctx, AdjustmentLookup{Transaction: billing.Reference{Scope: s.client.Scope(), ID: payload.TransactionID}, Customer: payload.Customer, Action: AdjustmentRefund, After: cp.Cursor, Limit: 50})
		if err != nil {
			return stop("page_lookup_failed", err)
		}
		for _, candidate := range page.Items {
			if candidate.Reason != payload.Operation {
				continue
			}
			if err := validateRefundAdjustment(candidate, payload); err != nil {
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
		previous, err = s.saveCheckpoint(ctx, claim, payload, cp, previous)
		if err != nil {
			// The scan is abandoned mid-page, so the delivery is still
			// unresolved; callers dispatch on that sentinel.
			return Adjustment{}, errors.Join(ErrRefundUnresolved, err)
		}
		if cp.Complete {
			if len(cp.Matches) == 0 {
				return Adjustment{}, ErrRefundUnresolved
			}
			if err := s.resolveAccepted(ctx, claim, payload, matched, previous); err != nil {
				return Adjustment{}, err
			}
			return matched, nil
		}
	}
	return stop("invocation_page_bound", nil)
}

// lookupKnown separates a transport failure, which is worth retrying, from a
// candidate that no longer matches the plan, which never will be.
func (s *RefundService) lookupKnown(ctx context.Context, id string, payload RefundDispatch) (Adjustment, bool, error) {
	got, err := s.client.Adjustment(ctx, billing.Reference{Scope: s.client.Scope(), ID: id})
	if err != nil {
		return Adjustment{}, false, err
	}
	if err := validateRefundAdjustment(got, payload); err != nil {
		return Adjustment{}, true, err
	}
	return got, false, nil
}

func (s *RefundService) resolveAccepted(ctx context.Context, claim integration.Claim, payload RefundDispatch, got Adjustment, expectedPrevious string) error {
	observation := checkoutObservation(claim.Message.ID, got.Reference.ID, "reconciled:"+expectedPrevious)
	return s.repo.ResolveOutbox(ctx, claim, integration.OutboxResult{ObservationID: observation, ExpectedPrevious: expectedPrevious, State: integration.OutboxCompleted, ProviderReference: got.Reference.ID, Evidence: "reconciled"}, refundNoop)
}

func (s *RefundService) saveCheckpoint(ctx context.Context, claim integration.Claim, _ RefundDispatch, cp scanCheckpoint, previous string) (string, error) {
	return saveScanCheckpoint(ctx, s.repo, claim, cp, refundScanBounds, previous, s.now(), func(string) func(integration.Session) error { return refundNoop })
}

func (s *RefundService) recoveryPayload(message integration.Message, account billing.AccountID) (RefundDispatch, error) {
	if message.Validate() != nil || message.Account != account || message.ProviderScope() != s.client.Scope() || message.Direction != integration.Outbound || message.Kind != refundDispatchKind {
		return RefundDispatch{}, ErrInvalid
	}
	payload, err := decodeRefundDispatch(message.Payload)
	if err != nil || payload.Account != account || payload.Customer.Scope != s.client.Scope() {
		return RefundDispatch{}, ErrResponse
	}
	id, err := integration.ProviderMessageID(s.client.Scope().Provider, s.client.Scope().Merchant, s.client.Scope().Environment, payload.Operation)
	if err != nil || id != message.ID {
		return RefundDispatch{}, billing.ErrConflict
	}
	return payload, nil
}
