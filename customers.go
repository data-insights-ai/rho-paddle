package paddle

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"

	"github.com/data-insights-ai/rho-billing/provider"
)

// existingCustomer finds the customer Paddle names when an address is taken.
var existingCustomer = regexp.MustCompile(`\bctm_[0-9a-z]{26}\b`)

// ErrCustomerExists: Paddle has a customer with that email address that is
// not this account's (its reference differs or is missing). It is never
// answered as the account's customer, so nobody gets another person's
// invoices or payment methods by naming their address.
var ErrCustomerExists = errors.New("paddle: a customer with this email address belongs to someone else")

// CreateCustomer creates a Paddle customer with the account's reference.
// Paddle keeps one customer per email address: when the address has one,
// it is answered only if its reference is exactly this account's (an
// earlier attempt that created it), else ErrCustomerExists.
func (c *Client) CreateCustomer(ctx context.Context, r provider.CustomerRequest) (string, error) {
	return c.customerFor(ctx, r, false)
}

// ClaimCustomer is CreateCustomer for an address the host has proven to be
// the buyer's own (verified at sign-in, or confirmed by a mailed code): an
// existing customer without a reference becomes this account's customer,
// its other custom data kept. A customer whose reference is another
// account's is never taken over (ErrCustomerExists).
func (c *Client) ClaimCustomer(ctx context.Context, r provider.CustomerRequest) (string, error) {
	return c.customerFor(ctx, r, true)
}

func (c *Client) customerFor(ctx context.Context, r provider.CustomerRequest, claim bool) (string, error) {
	body := map[string]any{"email": r.Email}
	if r.Name != "" {
		body["name"] = r.Name
	}
	if r.Locale != "" {
		body["locale"] = r.Locale
	}
	if r.Reference != "" {
		body["custom_data"] = map[string]string{ReferenceKey: r.Reference}
	}
	var out customer
	err := c.call(ctx, http.MethodPost, "/customers", body, &out)
	var refused *APIError
	if !errors.As(err, &refused) || refused.Code != "customer_already_exists" {
		return out.Data.ID, err
	}
	id := existingCustomer.FindString(refused.Detail)
	if id == "" || r.Reference == "" {
		return "", ErrCustomerExists
	}
	var existing customer
	if err := c.call(ctx, http.MethodGet, "/customers/"+url.PathEscape(id), nil, &existing); err != nil {
		return "", err
	}
	reference := existing.Data.CustomData[ReferenceKey]
	switch {
	case existing.Data.ID != id:
		return "", ErrCustomerExists
	case reference == r.Reference:
		return id, c.keepLocale(ctx, id, existing.Data.Locale, r.Locale)
	case reference != "" || !claim:
		return "", ErrCustomerExists
	}
	data := map[string]string{}
	for k, v := range existing.Data.CustomData {
		data[k] = v
	}
	data[ReferenceKey] = r.Reference
	patch := map[string]any{"custom_data": data}
	if r.Locale != "" {
		patch["locale"] = r.Locale
	}
	if err := c.call(ctx, http.MethodPatch, "/customers/"+url.PathEscape(id), patch, nil); err != nil {
		return "", err
	}
	return id, nil
}

type customer struct {
	Data struct {
		ID         string            `json:"id"`
		Locale     string            `json:"locale"`
		CustomData map[string]string `json:"custom_data"`
	} `json:"data"`
}

// keepLocale moves the account's existing customer to the account's
// language, so Paddle's mail and checkout follow it.
func (c *Client) keepLocale(ctx context.Context, id, current, locale string) error {
	if locale == "" || locale == current {
		return nil
	}
	return c.call(ctx, http.MethodPatch, "/customers/"+url.PathEscape(id), map[string]any{"locale": locale}, nil)
}

// GetTransaction reads a transaction: its customer, subscription and the
// host's reference.
func (c *Client) GetTransaction(ctx context.Context, id string) (provider.Transaction, error) {
	var out struct {
		Data struct {
			ID             string            `json:"id"`
			Status         string            `json:"status"`
			CustomerID     string            `json:"customer_id"`
			SubscriptionID string            `json:"subscription_id"`
			CustomData     map[string]string `json:"custom_data"`
			Details        struct {
				LineItems []wireLine `json:"line_items"`
			} `json:"details"`
		} `json:"data"`
	}
	if err := c.call(ctx, http.MethodGet, "/transactions/"+url.PathEscape(id), nil, &out); err != nil {
		return provider.Transaction{}, err
	}
	d := out.Data
	tx := provider.Transaction{ID: d.ID, Status: d.Status, Customer: d.CustomerID, Subscription: d.SubscriptionID, Reference: d.CustomData[ReferenceKey]}
	for _, l := range d.Details.LineItems {
		total, err := amount(l.Totals.Total)
		if err != nil {
			return provider.Transaction{}, err
		}
		tx.Lines = append(tx.Lines, provider.Line{ID: l.ID, PriceID: l.PriceID, Quantity: l.Quantity, Total: total})
	}
	return tx, nil
}
