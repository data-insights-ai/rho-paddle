package paddle

import (
	"context"
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"net/http"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type ScheduledAction string

const (
	ScheduledPause  ScheduledAction = "pause"
	ScheduledCancel ScheduledAction = "cancel"
	ScheduledResume ScheduledAction = "resume"
)

type ScheduledChange struct {
	Action      ScheduledAction
	EffectiveAt time.Time
	ResumeAt    time.Time
}

type SubscriptionStatus string

const (
	SubscriptionActive   SubscriptionStatus = "active"
	SubscriptionTrialing SubscriptionStatus = "trialing"
	SubscriptionPastDue  SubscriptionStatus = "past_due"
	SubscriptionPaused   SubscriptionStatus = "paused"
	SubscriptionCanceled SubscriptionStatus = "canceled"
)

type ProrationMode string

const (
	ProrationProratedImmediately ProrationMode = "prorated_immediately"
	ProrationProratedNextPeriod  ProrationMode = "prorated_next_billing_period"
	ProrationFullImmediately     ProrationMode = "full_immediately"
	ProrationFullNextPeriod      ProrationMode = "full_next_billing_period"
	ProrationDoNotBill           ProrationMode = "do_not_bill"
)

type Subscription struct {
	Reference       billing.Reference
	Customer        billing.Reference
	Status          SubscriptionStatus
	CollectionMode  CollectionMode
	Items           []CheckoutItem
	ScheduledChange ScheduledChange
}

type SubscriptionUpdate struct {
	Subscription billing.Reference
	Items        []CheckoutItem
	Proration    ProrationMode
}

func (c *Client) Subscription(ctx context.Context, ref billing.Reference) (Subscription, error) {
	if c == nil || ref.Scope != c.scope || !paddlewire.ID(ref.ID, "sub_") {
		return Subscription{}, ErrInvalid
	}
	var wire paddlewire.Subscription
	if err := c.request(ctx, http.MethodGet, "/subscriptions/"+ref.ID, nil, &wire); err != nil {
		return Subscription{}, err
	}
	out, err := c.subscription(wire)
	if err != nil || out.Reference != ref {
		return Subscription{}, ErrResponse
	}
	return out, nil
}

func (c *Client) subscription(w paddlewire.Subscription) (Subscription, error) {
	if !paddlewire.ID(w.ID, "sub_") || !paddlewire.ID(w.CustomerID, "ctm_") {
		return Subscription{}, ErrResponse
	}
	switch SubscriptionStatus(w.Status) {
	case SubscriptionActive, SubscriptionTrialing, SubscriptionPastDue, SubscriptionPaused, SubscriptionCanceled:
	default:
		return Subscription{}, ErrResponse
	}
	switch CollectionMode(w.CollectionMode) {
	case CollectionAutomatic, CollectionManual:
	default:
		return Subscription{}, ErrResponse
	}
	items := make([]CheckoutItem, 0, len(w.Items))
	for _, item := range w.Items {
		if !paddlewire.ID(item.Price.ID, "pri_") || item.Quantity <= 0 {
			return Subscription{}, ErrResponse
		}
		items = append(items, CheckoutItem{Price: billing.Reference{Scope: c.scope, ID: item.Price.ID}, Quantity: item.Quantity})
	}
	change, err := scheduledChange(w)
	if err != nil {
		return Subscription{}, err
	}
	return Subscription{
		Reference:       billing.Reference{Scope: c.scope, ID: w.ID},
		Customer:        billing.Reference{Scope: c.scope, ID: w.CustomerID},
		Status:          SubscriptionStatus(w.Status),
		CollectionMode:  CollectionMode(w.CollectionMode),
		Items:           items,
		ScheduledChange: change,
	}, nil
}

func scheduledChange(w paddlewire.Subscription) (ScheduledChange, error) {
	if w.ScheduledChange == nil {
		return ScheduledChange{}, nil
	}
	switch w.ScheduledChange.Action {
	case string(ScheduledPause), string(ScheduledCancel), string(ScheduledResume):
	default:
		return ScheduledChange{}, ErrResponse
	}
	at, err := parseProviderTime(w.ScheduledChange.EffectiveAt)
	if err != nil {
		return ScheduledChange{}, ErrResponse
	}
	change := ScheduledChange{Action: ScheduledAction(w.ScheduledChange.Action), EffectiveAt: at}
	if w.ScheduledChange.ResumeAt != nil && *w.ScheduledChange.ResumeAt != "" {
		resume, err := parseProviderTime(*w.ScheduledChange.ResumeAt)
		if err != nil {
			return ScheduledChange{}, ErrResponse
		}
		change.ResumeAt = resume
	}
	return change, nil
}

func parseProviderTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, ErrResponse
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return billing.CanonicalTime(parsed), nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return billing.CanonicalTime(parsed), nil
}

func (c *Client) UpdateSubscription(ctx context.Context, in SubscriptionUpdate) (Subscription, error) {
	if c == nil || in.Subscription.Scope != c.scope || !paddlewire.ID(in.Subscription.ID, "sub_") || len(in.Items) == 0 || len(in.Items) > 100 {
		return Subscription{}, ErrInvalid
	}
	switch in.Proration {
	case ProrationProratedImmediately, ProrationProratedNextPeriod, ProrationFullImmediately, ProrationFullNextPeriod, ProrationDoNotBill:
	default:
		return Subscription{}, ErrInvalid
	}
	type item struct {
		PriceID  string `json:"price_id"`
		Quantity int64  `json:"quantity"`
	}
	items := make([]item, len(in.Items))
	for i, line := range in.Items {
		if line.Price.Scope != c.scope || !paddlewire.ID(line.Price.ID, "pri_") || line.Quantity <= 0 {
			return Subscription{}, ErrInvalid
		}
		items[i] = item{PriceID: line.Price.ID, Quantity: line.Quantity}
	}
	request := struct {
		Items                []item `json:"items"`
		ProrationBillingMode string `json:"proration_billing_mode"`
	}{items, string(in.Proration)}
	var wire paddlewire.Subscription
	if err := c.request(ctx, http.MethodPatch, "/subscriptions/"+in.Subscription.ID, request, &wire); err != nil {
		return Subscription{}, err
	}
	out, err := c.subscription(wire)
	if err != nil || out.Reference != in.Subscription {
		return Subscription{}, errors.Join(ErrResponse, ErrUncertain)
	}
	return out, nil
}
