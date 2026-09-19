package paddle_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	paddle "github.com/data-insights-ai/rho-paddle"
)

// A declarative catalog is only worth anything if it survives a second run
// against the real provider: the first run creates, the second must find what
// it created and change nothing. Reconciliation that is not idempotent quietly
// mints a duplicate price on every deploy, and the duplicates are live money
// objects that cannot be deleted.
func TestSandboxReconcileCatalogIsIdempotent(t *testing.T) {
	client := sandboxClient(t)
	ctx := t.Context()

	// A fresh key per run: this creates real provider objects, and reusing a
	// key across runs would test the provider's leftovers rather than this code.
	suffix := time.Now().UTC().Format("20060102T150405.000000000")
	suffix = strings.ReplaceAll(suffix, ".", "")
	productKey := "rho-catalog-product-" + suffix
	monthly := "rho-catalog-monthly-" + suffix
	oneOff := "rho-catalog-oneoff-" + suffix

	declared := paddle.ProviderCatalog{
		Products: []paddle.ProductSpec{{
			Key: productKey, Name: "rho catalog test " + suffix, TaxCategory: "saas",
		}},
		Prices: []paddle.PriceSpec{
			{
				Key: monthly, ProductKey: productKey, Description: "monthly " + suffix,
				Currency: "EUR", Amount: 4900, Tax: paddle.TaxInclusive,
				Interval: paddle.BillingInterval{Unit: "month", Frequency: 1},
				Quantity: paddle.QuantityBounds{Min: 1, Max: 1},
			},
			{
				Key: oneOff, ProductKey: productKey, Description: "one-off " + suffix,
				Currency: "EUR", Amount: 1500, Tax: paddle.TaxInclusive,
			},
		},
	}

	first, err := client.ReconcileCatalog(ctx, declared)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	monthlyRef, ok := first.Price(monthly)
	if !ok || !strings.HasPrefix(monthlyRef.ID, "pri_") {
		t.Fatalf("monthly price was not mapped: %+v", first.Prices)
	}
	if _, ok := first.Price(oneOff); !ok {
		t.Fatal("one-off price was not mapped")
	}
	if ref, ok := first.Products[productKey]; !ok || !strings.HasPrefix(ref.ID, "pro_") {
		t.Fatalf("product was not mapped: %+v", first.Products)
	}

	second, err := client.ReconcileCatalog(ctx, declared)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Products[productKey] != first.Products[productKey] {
		t.Fatalf("reconcile created a second product: %v then %v",
			first.Products[productKey], second.Products[productKey])
	}
	for _, key := range []string{monthly, oneOff} {
		if second.Prices[key] != first.Prices[key] {
			t.Fatalf("reconcile created a second price for %q: %v then %v",
				key, first.Prices[key], second.Prices[key])
		}
	}

	// Quantity bounds are not commercial terms: a changed declaration is
	// brought in line on the existing price, not reported as drift.
	rebounded := declared
	rebounded.Prices = append([]paddle.PriceSpec(nil), declared.Prices...)
	rebounded.Prices[0].Quantity = paddle.QuantityBounds{Min: 1, Max: 3}
	third, err := client.ReconcileCatalog(ctx, rebounded)
	if err != nil {
		t.Fatalf("reconcile with new quantity bounds: %v", err)
	}
	if third.Prices[monthly] != first.Prices[monthly] {
		t.Fatalf("quantity change replaced the price: %v then %v", first.Prices[monthly], third.Prices[monthly])
	}
	if _, err := client.ReconcileCatalog(ctx, rebounded); err != nil {
		t.Fatalf("reconcile after the quantity update must be a no-op: %v", err)
	}

	// Changing an amount under a key that already exists must not edit a live
	// price. Existing subscriptions renew against it; silently repricing them
	// is the worst outcome available here.
	drifted := declared
	drifted.Prices = append([]paddle.PriceSpec(nil), declared.Prices...)
	drifted.Prices[0].Amount = 5900
	if _, err := client.ReconcileCatalog(ctx, drifted); !errors.Is(err, paddle.ErrCatalogDrift) {
		t.Fatalf("repricing an existing key returned %v; want ErrCatalogDrift", err)
	}
}
