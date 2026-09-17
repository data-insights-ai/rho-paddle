package paddle

import (
	"errors"
	"math"
	"testing"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestCheckoutDispatchCanonicalLinesAndGroupedPrices(t *testing.T) {
	scope := billing.Scope{Provider: "paddle", Merchant: "merchant", Environment: "sandbox"}
	lines, err := canonicalDispatchLines(scope, []CheckoutDispatchLine{
		{QuoteLineID: "line-b", Price: billing.Reference{Scope: scope, ID: "pri_0123456789abcdefghijklmnop"}, Quantity: 1},
		{QuoteLineID: "line-a", Price: billing.Reference{Scope: scope, ID: "pri_zyxwvutsrqponmlkjihgfedcba"}, Quantity: 2},
	})
	if err != nil || len(lines) != 2 || lines[0].QuoteLineID != "line-a" {
		t.Fatalf("lines=%#v err=%v", lines, err)
	}
	payload := CheckoutDispatch{Account: "account", Operation: "operation", IntentID: "intent", QuoteFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Customer: billing.Reference{Scope: scope, ID: "ctm_0123456789abcdefghijklmnop"}, Lines: []CheckoutDispatchLine{{QuoteLineID: "line-a", Price: billing.Reference{Scope: scope, ID: "pri_0123456789abcdefghijklmnop"}, Quantity: 1}, {QuoteLineID: "line-b", Price: billing.Reference{Scope: scope, ID: "pri_0123456789abcdefghijklmnop"}, Quantity: 1}}, Correlation: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	tx := Transaction{Reference: billing.Reference{Scope: scope, ID: "txn_0123456789abcdefghijklmnop"}, Customer: payload.Customer, Origin: "api", CollectionMode: "automatic", Currency: "USD", Correlation: CheckoutCorrelation{Account: payload.Account, Operation: payload.Operation, IntentID: payload.IntentID, QuoteFingerprint: payload.QuoteFingerprint, RequestFingerprint: payload.Correlation, Schema: "rho.checkout.v1"}, Items: []TransactionItem{{Reference: billing.Reference{Scope: scope, ID: "txnitm_0123456789abcdefghijklmnop"}, Price: lines[0].Price, Quantity: 1}, {Reference: billing.Reference{Scope: scope, ID: "txnitm_zyxwvutsrqponmlkjihgfedcba"}, Price: lines[1].Price, Quantity: 1}}}
	if !errors.Is(validateCheckoutTransaction(tx, payload, "USD"), billing.ErrConflict) {
		t.Fatal("unrelated provider price was accepted")
	}
	tx.Items = []TransactionItem{{Reference: billing.Reference{Scope: scope, ID: "txnitm_0123456789abcdefghijklmnop"}, Price: payload.Lines[0].Price, Quantity: 2}}
	if err := validateCheckoutTransaction(tx, payload, "USD"); err != nil {
		t.Fatalf("grouped price rejected: %v", err)
	}
	payload.Lines[0].Quantity = math.MaxInt64
	if _, err := canonicalDispatchLines(scope, payload.Lines); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("aggregate overflow=%v", err)
	}
}
