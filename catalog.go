package paddle

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/data-insights-ai/rho-billing/provider"
)

type wireProduct struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	CustomData map[string]string `json:"custom_data"`
	Prices     []wirePrice       `json:"prices"`
}

type wirePrice struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	Status      string            `json:"status"`
	CustomData  map[string]string `json:"custom_data"`
	UnitPrice   struct {
		Amount       string `json:"amount"`
		CurrencyCode string `json:"currency_code"`
	} `json:"unit_price"`
	BillingCycle *struct {
		Interval  string `json:"interval"`
		Frequency int    `json:"frequency"`
	} `json:"billing_cycle"`
}

// maxPages bounds the catalog walk; a catalog this large is a mistake.
const maxPages = 50

// Catalog reads every product with its prices, archived ones included, so
// the host can tell what stopped being sold.
func (c *Client) Catalog(ctx context.Context) ([]provider.Product, error) {
	var out []provider.Product
	path := "/products?include=prices&status=active,archived&per_page=200"
	for range maxPages {
		var page struct {
			Data []wireProduct `json:"data"`
			Meta struct {
				Pagination struct {
					Next    string `json:"next"`
					HasMore bool   `json:"has_more"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := c.call(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		for _, p := range page.Data {
			out = append(out, toProduct(p))
		}
		if !page.Meta.Pagination.HasMore {
			return out, nil
		}
		next, err := url.Parse(page.Meta.Pagination.Next)
		if err != nil {
			return nil, err
		}
		path = next.Path + "?" + next.RawQuery
	}
	return nil, ErrInvalid
}

func toProduct(p wireProduct) provider.Product {
	out := provider.Product{ID: p.ID, Name: p.Name, Reference: p.CustomData[ReferenceKey], Archived: p.Status == "archived"}
	for _, pr := range p.Prices {
		amount, _ := strconv.ParseInt(pr.UnitPrice.Amount, 10, 64)
		price := provider.Price{
			ID: pr.ID, Description: pr.Description, Amount: amount, Currency: pr.UnitPrice.CurrencyCode,
			Reference: pr.CustomData[ReferenceKey], Archived: pr.Status == "archived",
		}
		if pr.BillingCycle != nil {
			price.Interval, price.Frequency = pr.BillingCycle.Interval, pr.BillingCycle.Frequency
		}
		out.Prices = append(out.Prices, price)
	}
	return out
}
