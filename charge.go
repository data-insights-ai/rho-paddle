package paddle

import (
	"context"
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"net/http"

	billing "github.com/data-insights-ai/rho-billing"
)

type ChargeWhen string

const (
	ChargeImmediately ChargeWhen = "immediately"
	ChargeNextPeriod  ChargeWhen = "next_billing_period"
)

type CollectionMode string

const (
	CollectionAutomatic CollectionMode = "automatic"
	CollectionManual    CollectionMode = "manual"
)

type ChargeInput struct {
	Account        billing.AccountID
	Operation      string
	Subscription   billing.Reference
	When           ChargeWhen
	CollectionMode CollectionMode
	Items          []CheckoutItem
}

type ChargeResult struct {
	Subscription   billing.Reference
	Customer       billing.Reference
	Status         SubscriptionStatus
	CollectionMode CollectionMode
}

// CollectionModeCapability reports whether this adapter will dispatch a given
// Paddle collection mode. Manual invoicing stays unsupported.
func CollectionModeCapability(mode CollectionMode) billing.Capability {
	switch mode {
	case CollectionAutomatic:
		return billing.Capability{Operation: "collection_mode", Support: billing.SupportSupported}
	case CollectionManual:
		return billing.Capability{Operation: "collection_mode", Support: billing.SupportUnsupported, Reason: "manual_invoice_collection"}
	default:
		return billing.Capability{Operation: "collection_mode", Support: billing.SupportUnsupported, Reason: "collection_mode_not_supported"}
	}
}

func ChargeTimingCapability(when ChargeWhen) billing.Capability {
	switch when {
	case ChargeImmediately:
		return billing.Capability{Operation: "immediate_usage_charge", Support: billing.SupportSupported}
	case ChargeNextPeriod:
		return billing.Capability{Operation: "next_period_usage_charge", Support: billing.SupportSupported}
	default:
		return billing.Capability{Operation: "subscription_charge", Support: billing.SupportUnsupported, Reason: "charge_timing_not_supported"}
	}
}

// ChargeSubscription bills non-recurring catalog prices onto one subscription.
// Unsupported collection modes and timings return before any provider request.
// Immediate charges may attempt payment synchronously; a timeout stays unknown.
func (c *Client) ChargeSubscription(ctx context.Context, in ChargeInput) (ChargeResult, error) {
	if c == nil || !billing.ValidID(string(in.Account)) || !billing.ValidID(in.Operation) || in.Subscription.Scope != c.scope || !paddlewire.ID(in.Subscription.ID, "sub_") || len(in.Items) == 0 || len(in.Items) > 100 {
		return ChargeResult{}, ErrInvalid
	}
	if err := billing.RequireSupported(CollectionModeCapability(in.CollectionMode)); err != nil {
		return ChargeResult{}, err
	}
	if err := billing.RequireSupported(ChargeTimingCapability(in.When)); err != nil {
		return ChargeResult{}, err
	}
	current, err := c.Subscription(ctx, in.Subscription)
	if err != nil {
		return ChargeResult{}, err
	}
	if err := billing.RequireSupported(CollectionModeCapability(current.CollectionMode)); err != nil {
		return ChargeResult{}, err
	}
	type item struct {
		PriceID  string `json:"price_id"`
		Quantity int64  `json:"quantity"`
	}
	items := make([]item, len(in.Items))
	for i, line := range in.Items {
		if line.Price.Scope != c.scope || !paddlewire.ID(line.Price.ID, "pri_") || line.Quantity <= 0 {
			return ChargeResult{}, ErrInvalid
		}
		items[i] = item{PriceID: line.Price.ID, Quantity: line.Quantity}
	}
	request := struct {
		EffectiveFrom string `json:"effective_from"`
		Items         []item `json:"items"`
	}{EffectiveFrom: string(in.When), Items: items}
	var wire struct {
		ID             string `json:"id"`
		CustomerID     string `json:"customer_id"`
		Status         string `json:"status"`
		CollectionMode string `json:"collection_mode"`
	}
	if err := c.request(ctx, http.MethodPost, "/subscriptions/"+in.Subscription.ID+"/charge", request, &wire); err != nil {
		return ChargeResult{}, err
	}
	if wire.ID != in.Subscription.ID || !paddlewire.ID(wire.CustomerID, "ctm_") {
		return ChargeResult{}, errors.Join(ErrResponse, ErrUncertain)
	}
	switch SubscriptionStatus(wire.Status) {
	case SubscriptionActive, SubscriptionTrialing, SubscriptionPastDue, SubscriptionPaused, SubscriptionCanceled:
	default:
		return ChargeResult{}, errors.Join(ErrResponse, ErrUncertain)
	}
	if CollectionMode(wire.CollectionMode) != CollectionAutomatic {
		return ChargeResult{}, errors.Join(ErrUncertain, billing.RequireSupported(CollectionModeCapability(CollectionMode(wire.CollectionMode))))
	}
	return ChargeResult{
		Subscription:   in.Subscription,
		Customer:       billing.Reference{Scope: c.scope, ID: wire.CustomerID},
		Status:         SubscriptionStatus(wire.Status),
		CollectionMode: CollectionMode(wire.CollectionMode),
	}, nil
}
