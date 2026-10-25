package paddle

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/data-insights-ai/rho-billing/provider"
)

// taxCategory is Paddle's category for software sold as a service, which
// custom items are billed under when they bring no product of their own.
const taxCategory = "saas"

type customItem struct {
	Price    customPrice `json:"price"`
	Quantity int         `json:"quantity"`
}

type customPrice struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	UnitPrice    money             `json:"unit_price"`
	BillingCycle *cycle            `json:"billing_cycle,omitempty"`
	TrialPeriod  *cycle            `json:"trial_period,omitempty"`
	ProductID    string            `json:"product_id,omitempty"`
	Product      *customProduct    `json:"product,omitempty"`
	CustomData   map[string]string `json:"custom_data,omitempty"`
}

type money struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
}

type cycle struct {
	Interval  string `json:"interval"`
	Frequency int    `json:"frequency"`
}

type customProduct struct {
	Name        string `json:"name"`
	TaxCategory string `json:"tax_category"`
}

// toCustom is a host's price as Paddle's non-catalog item: under the given
// product, else a non-catalog product of the item's name.
func toCustom(it provider.CustomItem, product string) (customItem, error) {
	if err := it.Validate(); err != nil {
		return customItem{}, err
	}
	description := it.Description
	if description == "" {
		description = it.Name
	}
	p := customPrice{
		Name: it.Name, Description: description,
		UnitPrice: money{Amount: strconv.FormatInt(it.Amount, 10), CurrencyCode: it.Currency},
	}
	if it.Interval != "" {
		p.BillingCycle = &cycle{Interval: it.Interval, Frequency: it.Frequency}
	}
	if it.TrialDays > 0 {
		p.TrialPeriod = &cycle{Interval: "day", Frequency: it.TrialDays}
	}
	if product != "" {
		p.ProductID = product
	} else {
		p.Product = &customProduct{Name: it.Name, TaxCategory: taxCategory}
	}
	if it.Reference != "" {
		p.CustomData = map[string]string{ReferenceKey: it.Reference}
	}
	return customItem{Price: p, Quantity: it.Quantity}, nil
}

// ReplaceItems puts the custom items on the subscription without billing
// the change, so the next renewal bills them. They are priced under the
// product of the subscription's first item, as Paddle requires an existing
// product for a subscription's non-catalog prices.
func (c *Client) ReplaceItems(ctx context.Context, r provider.ReplaceRequest) error {
	if len(r.Items) == 0 {
		return provider.ErrInvalidItem
	}
	path := "/subscriptions/" + url.PathEscape(r.Subscription)
	product, err := c.subscriptionProduct(ctx, r.Subscription)
	if err != nil {
		return err
	}
	items := make([]customItem, len(r.Items))
	for i, it := range r.Items {
		if items[i], err = toCustom(it, product); err != nil {
			return err
		}
	}
	return c.call(ctx, http.MethodPatch, path, map[string]any{"items": items, "proration_billing_mode": "do_not_bill"}, nil)
}

// CancelAtPeriodEnd schedules the subscription's cancellation for the end
// of its current billing period.
func (c *Client) CancelAtPeriodEnd(ctx context.Context, subscription string) error {
	body := map[string]string{"effective_from": "next_billing_period"}
	return c.call(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(subscription)+"/cancel", body, nil)
}

// SetNextBilling moves the subscription's next renewal; Paddle ends the
// current period then and bills nothing for the change. Paddle refuses it in
// the same request as an item change, so the two are separate calls.
func (c *Client) SetNextBilling(ctx context.Context, subscription string, at time.Time) error {
	body := map[string]string{"next_billed_at": at.UTC().Format(time.RFC3339), "proration_billing_mode": "do_not_bill"}
	return c.call(ctx, http.MethodPatch, "/subscriptions/"+url.PathEscape(subscription), body, nil)
}

// subscriptionProduct is the product of the subscription's first item:
// Paddle prices a subscription's non-catalog items under an existing one.
func (c *Client) subscriptionProduct(ctx context.Context, subscription string) (string, error) {
	var current struct {
		Data struct {
			Items []struct {
				Price struct {
					ProductID string `json:"product_id"`
				} `json:"price"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := c.call(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(subscription), nil, &current); err != nil {
		return "", err
	}
	if len(current.Data.Items) == 0 {
		return "", nil
	}
	return current.Data.Items[0].Price.ProductID, nil
}
