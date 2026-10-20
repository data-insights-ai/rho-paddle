package paddle

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/data-insights-ai/rho-billing/provider"
	"github.com/data-insights-ai/rho-billing/subscription"
)

var ErrMalformed = errors.New("paddle: malformed event")

type envelope struct {
	EventID    string          `json:"event_id"`
	EventType  string          `json:"event_type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

type wireItem struct {
	Quantity int `json:"quantity"`
	Price    struct {
		ID string `json:"id"`
	} `json:"price"`
}

type wirePeriod struct {
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

type wireSubscription struct {
	ID                   string            `json:"id"`
	Status               string            `json:"status"`
	CustomerID           string            `json:"customer_id"`
	Items                []wireItem        `json:"items"`
	CustomData           map[string]string `json:"custom_data"`
	CurrentBillingPeriod *wirePeriod       `json:"current_billing_period"`
	ScheduledChange      *struct {
		Action      string    `json:"action"`
		EffectiveAt time.Time `json:"effective_at"`
	} `json:"scheduled_change"`
}

type wireTransaction struct {
	ID             string            `json:"id"`
	CustomerID     string            `json:"customer_id"`
	SubscriptionID string            `json:"subscription_id"`
	CurrencyCode   string            `json:"currency_code"`
	Items          []wireItem        `json:"items"`
	CustomData     map[string]string `json:"custom_data"`
	BilledAt       time.Time         `json:"billed_at"`
	Details        struct {
		Totals struct {
			GrandTotal string `json:"grand_total"`
		} `json:"totals"`
	} `json:"details"`
}

// Decode turns a verified webhook body into an event; kinds a host does
// not need answer provider.ErrIgnored.
func Decode(body []byte) (provider.Event, error) {
	var e envelope
	if err := json.Unmarshal(body, &e); err != nil || e.EventID == "" || e.OccurredAt.IsZero() {
		return provider.Event{}, ErrMalformed
	}
	out := provider.Event{ID: e.EventID, OccurredAt: e.OccurredAt}
	switch {
	case strings.HasPrefix(e.EventType, "subscription."):
		var s wireSubscription
		if err := json.Unmarshal(e.Data, &s); err != nil || s.ID == "" {
			return provider.Event{}, ErrMalformed
		}
		sub := toSubscription(s, subscription.Revision{At: e.OccurredAt, EventID: e.EventID})
		out.Kind, out.Subscription = provider.SubscriptionChanged, &sub
	case e.EventType == "transaction.completed":
		var t wireTransaction
		if err := json.Unmarshal(e.Data, &t); err != nil || t.ID == "" {
			return provider.Event{}, ErrMalformed
		}
		total, err := strconv.ParseInt(t.Details.Totals.GrandTotal, 10, 64)
		if err != nil {
			return provider.Event{}, ErrMalformed
		}
		out.Kind, out.Payment = provider.PaymentCompleted, &provider.Payment{
			ID: t.ID, Customer: t.CustomerID, Subscription: t.SubscriptionID, Items: items(t.Items),
			Reference: t.CustomData[ReferenceKey], Currency: t.CurrencyCode, Total: total, BilledAt: t.BilledAt,
		}
	default:
		return out, provider.ErrIgnored
	}
	return out, nil
}

func toSubscription(s wireSubscription, rev subscription.Revision) subscription.Subscription {
	out := subscription.Subscription{
		ID: s.ID, Customer: s.CustomerID, Status: subscription.Status(s.Status), Items: items(s.Items),
		Reference: s.CustomData[ReferenceKey], Revision: rev,
	}
	if p := s.CurrentBillingPeriod; p != nil {
		out.PeriodStart, out.PeriodEnd = p.StartsAt, p.EndsAt
	}
	if c := s.ScheduledChange; c != nil && c.Action == "cancel" {
		out.CancelAt = c.EffectiveAt
	}
	return out
}

func items(in []wireItem) []subscription.Item {
	out := make([]subscription.Item, len(in))
	for i, it := range in {
		out[i] = subscription.Item{PriceID: it.Price.ID, Quantity: it.Quantity}
	}
	return out
}
