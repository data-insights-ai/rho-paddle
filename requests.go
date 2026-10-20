package paddle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/data-insights-ai/rho-billing/provider"
)

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	resp, err := c.api.Do(ctx, method, path, body)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(resp.Body, out)
}

type item struct {
	PriceID  string `json:"price_id"`
	Quantity int    `json:"quantity"`
}

func (c *Client) Checkout(ctx context.Context, r provider.CheckoutRequest) (provider.Checkout, error) {
	items := make([]item, len(r.Items))
	for i, it := range r.Items {
		items[i] = item{PriceID: it.PriceID, Quantity: it.Quantity}
	}
	body := map[string]any{"items": items, "custom_data": map[string]string{ReferenceKey: r.Reference}}
	if r.Customer != "" {
		body["customer_id"] = r.Customer
	}
	if r.SuccessURL != "" {
		body["checkout"] = map[string]string{"url": r.SuccessURL}
	}
	var out struct {
		Data struct {
			ID       string `json:"id"`
			Checkout struct {
				URL string `json:"url"`
			} `json:"checkout"`
		} `json:"data"`
	}
	if err := c.call(ctx, http.MethodPost, "/transactions", body, &out); err != nil {
		return provider.Checkout{}, err
	}
	return provider.Checkout{ID: out.Data.ID, URL: out.Data.Checkout.URL}, nil
}

func (c *Client) CustomerPortal(ctx context.Context, customer string) (string, error) {
	var out struct {
		Data struct {
			URLs struct {
				General struct {
					Overview string `json:"overview"`
				} `json:"general"`
			} `json:"urls"`
		} `json:"data"`
	}
	err := c.call(ctx, http.MethodPost, "/customers/"+url.PathEscape(customer)+"/portal-sessions", map[string]any{}, &out)
	return out.Data.URLs.General.Overview, err
}

// Charge bills a one-time price with the subscription's next renewal.
// Paddle has no idempotency keys, so the host records a charge before it
// asks for it and treats an uncertain outcome as possibly done.
func (c *Client) Charge(ctx context.Context, r provider.ChargeRequest) error {
	body := map[string]any{
		"effective_from": "next_billing_period",
		"items":          []item{{PriceID: r.PriceID, Quantity: r.Quantity}},
	}
	return c.call(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(r.Subscription)+"/charge", body, nil)
}
