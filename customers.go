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

// CreateCustomer creates a Paddle customer for the account. Paddle keeps
// one customer per email address; when the address has one already, that
// customer is answered, so a person who buys for several accounts pays as
// one customer.
func (c *Client) CreateCustomer(ctx context.Context, r provider.CustomerRequest) (string, error) {
	body := map[string]any{"email": r.Email}
	if r.Name != "" {
		body["name"] = r.Name
	}
	if r.Reference != "" {
		body["custom_data"] = map[string]string{ReferenceKey: r.Reference}
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	err := c.call(ctx, http.MethodPost, "/customers", body, &out)
	var refused *APIError
	if errors.As(err, &refused) && refused.Code == "customer_already_exists" {
		if id := existingCustomer.FindString(refused.Detail); id != "" {
			return id, nil
		}
	}
	return out.Data.ID, err
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
		} `json:"data"`
	}
	if err := c.call(ctx, http.MethodGet, "/transactions/"+url.PathEscape(id), nil, &out); err != nil {
		return provider.Transaction{}, err
	}
	d := out.Data
	return provider.Transaction{ID: d.ID, Status: d.Status, Customer: d.CustomerID, Subscription: d.SubscriptionID, Reference: d.CustomData[ReferenceKey]}, nil
}
