package paddle

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

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
	var current struct {
		Data struct {
			Items []struct {
				Price struct {
					ProductID string `json:"product_id"`
				} `json:"price"`
			} `json:"items"`
		} `json:"data"`
	}
	path := "/subscriptions/" + url.PathEscape(r.Subscription)
	if err := c.call(ctx, http.MethodGet, path, nil, &current); err != nil {
		return err
	}
	var product string
	if len(current.Data.Items) > 0 {
		product = current.Data.Items[0].Price.ProductID
	}
	items := make([]customItem, len(r.Items))
	for i, it := range r.Items {
		var err error
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
