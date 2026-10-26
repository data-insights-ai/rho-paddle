package paddle

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/data-insights-ai/rho-billing/provider"
)

// ErrBusinessNeedsCustomer: Paddle keeps businesses under a customer, so a
// checkout for a business names its customer.
var ErrBusinessNeedsCustomer = errors.New("paddle: a checkout for a business needs the customer")

type business struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	TaxIdentifier string `json:"tax_identifier"`
}

// businessFor is the customer's active business of that name, created when
// it has none and given the tax number when it gains or changes one. A
// business without a tax number keeps the one Paddle has: removing it is
// not a checkout's decision. The tax number travels only in request
// bodies, never in a URL.
func (c *Client) businessFor(ctx context.Context, customer string, b provider.Business) (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}
	if customer == "" {
		return "", ErrBusinessNeedsCustomer
	}
	name, taxID := strings.TrimSpace(b.Name), strings.TrimSpace(b.TaxID)
	base := "/customers/" + url.PathEscape(customer) + "/businesses"
	var found struct {
		Data []business `json:"data"`
	}
	query := url.Values{"status": {"active"}, "search": {name}}
	if err := c.call(ctx, http.MethodGet, base+"?"+query.Encode(), nil, &found); err != nil {
		return "", err
	}
	for _, existing := range found.Data {
		if !strings.EqualFold(strings.TrimSpace(existing.Name), name) {
			continue
		}
		if taxID != "" && existing.TaxIdentifier != taxID {
			if err := c.call(ctx, http.MethodPatch, base+"/"+url.PathEscape(existing.ID), map[string]any{"tax_identifier": taxID}, nil); err != nil {
				return "", err
			}
		}
		return existing.ID, nil
	}
	body := map[string]any{"name": name}
	if taxID != "" {
		body["tax_identifier"] = taxID
	}
	var created struct {
		Data business `json:"data"`
	}
	if err := c.call(ctx, http.MethodPost, base, body, &created); err != nil {
		return "", err
	}
	return created.Data.ID, nil
}
