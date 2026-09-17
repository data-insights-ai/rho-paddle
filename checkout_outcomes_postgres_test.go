package paddle_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestCheckoutDispatchPostgresDefiniteProviderRejectionIsDurable(t *testing.T) {
	f := newPaymentFixture(t, "checkout-rejected", false)
	var calls int
	client, err := paddle.New(paddle.Config{
		Merchant:    "merchant-test",
		Environment: paddle.Sandbox,
		APIKey:      testAPIKey,
		HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return jsonResponse(http.StatusUnprocessableEntity, `{"error":{"code":"transaction_not_allowed"},"meta":{"request_id":"req_rejected"}}`), nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, plan, claim := makeRecoveryClaim(t, f, client, "checkout-rejected-worker")
	_, err = service.Dispatch(t.Context(), claim)
	if err == nil || errors.Is(err, paddle.ErrCheckoutUnresolved) || errors.Is(err, paddle.ErrUncertain) {
		t.Fatalf("dispatch error=%v, want definite rejection", err)
	}
	if calls != 1 {
		t.Fatalf("provider calls=%d, want one", calls)
	}
	delivery, err := f.store.Queue().Outbox(t.Context(), f.account, client.Scope(), plan.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(delivery.State) != string(integration.OutboxRejected) || delivery.ProviderReference != "" {
		t.Fatalf("delivery=%+v, want rejected without provider reference", delivery)
	}
	intent, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).Intent(t.Context(), f.account, f.intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Command != purchase.CommandRejected {
		t.Fatalf("intent=%+v, want rejected command", intent)
	}
	assertNoPaymentEffects(t, &f)
	if _, err := service.Dispatch(t.Context(), claim); err == nil {
		t.Fatal("replayed rejected dispatch succeeded")
	}
	if calls != 1 {
		t.Fatalf("replayed provider calls=%d, want one", calls)
	}
}

func TestCheckoutDispatchPostgresMalformedSuccessIsUnknownWithoutEffects(t *testing.T) {
	f := newPaymentFixture(t, "checkout-malformed", false)
	var calls int
	client, err := paddle.New(paddle.Config{
		Merchant:    "merchant-test",
		Environment: paddle.Sandbox,
		APIKey:      testAPIKey,
		HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return jsonResponse(http.StatusCreated, `{"data":{"id":"txn_abcdefghijklmnopqrstuvwxyz"}}`), nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, plan, claim := makeRecoveryClaim(t, f, client, "checkout-malformed-worker")
	got, err := service.Dispatch(t.Context(), claim)
	if !errors.Is(err, paddle.ErrCheckoutUnresolved) {
		t.Fatalf("dispatch error=%v, want unresolved malformed response", err)
	}
	if got.Reference.ID != "" || got.CheckoutURL != "" || len(got.Items) != 0 {
		t.Fatalf("dispatch result=%+v, want zero result", got)
	}
	delivery := assertCheckoutUnknown(t, f, client, plan.Message.ID)
	if delivery.ProviderReference != "" || delivery.LastResult == nil {
		t.Fatalf("delivery=%+v, want unknown without provider reference", delivery)
	}
	intent, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).Intent(t.Context(), f.account, f.intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Command != purchase.CommandUnknown {
		t.Fatalf("intent=%+v, want unknown command", intent)
	}
	assertNoPaymentEffects(t, &f)
	if _, err := service.Dispatch(t.Context(), claim); err == nil {
		t.Fatal("replayed malformed dispatch succeeded")
	}
	if calls != 1 {
		t.Fatalf("replayed provider calls=%d, want one", calls)
	}
}

func TestCheckoutRecoveryPostgresScansAtLeast31CandidatesAndRetainsMatch(t *testing.T) {
	f := newPaymentFixture(t, "checkout-many-candidates", false)
	exactID := "txn_00000000000000000000000000"
	firstPage := make([]string, 30)
	for i := range firstPage {
		firstPage[i] = "txn_" + fmt.Sprintf("%026d", i)
	}
	secondID := "txn_" + fmt.Sprintf("%026d", 30)
	var createCalls, listCalls int
	var customData map[string]string
	var candidateCreatedAt time.Time
	client, err := paddle.New(paddle.Config{
		Merchant:    "merchant-test",
		Environment: paddle.Sandbox,
		APIKey:      testAPIKey,
		HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodPost && req.URL.Path == "/transactions":
				createCalls++
				var body struct {
					CustomerID     string            `json:"customer_id"`
					CollectionMode string            `json:"collection_mode"`
					Items          []map[string]any  `json:"items"`
					CustomData     map[string]string `json:"custom_data"`
				}
				decodeRequest(t, req, &body)
				customData = body.CustomData
				return nil, context.DeadlineExceeded
			case req.Method == http.MethodGet && req.URL.Path == "/transactions":
				listCalls++
				candidateCreatedAt, _ = time.Parse(time.RFC3339Nano, req.URL.Query().Get("created_at[GT]"))
				candidateCreatedAt = candidateCreatedAt.Add(time.Minute)
				if listCalls == 1 {
					return jsonResponse(http.StatusOK, manyRecoveryListResponse(firstPage, exactID, true, customData, candidateCreatedAt, f)), nil
				}
				if req.URL.Query().Get("after") != firstPage[len(firstPage)-1] {
					t.Fatalf("second-page cursor=%q, want %q", req.URL.Query().Get("after"), firstPage[len(firstPage)-1])
				}
				return jsonResponse(http.StatusOK, manyRecoveryListResponse([]string{secondID}, exactID, false, customData, candidateCreatedAt, f)), nil
			default:
				return nil, fmt.Errorf("unexpected many-candidate request %s %s", req.Method, req.URL.String())
			}
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, plan, claim := makeRecoveryClaim(t, f, client, "checkout-many-candidates-worker")
	if _, err := service.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrCheckoutUnresolved) {
		t.Fatalf("dispatch error=%v, want unresolved timeout", err)
	}
	got, err := service.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "checkout-many-candidates-resumer")
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != exactID || got.CheckoutURL != "https://checkout.example.test/recovered" {
		t.Fatalf("recovered transaction=%+v, want retained exact match", got)
	}
	if createCalls != 1 || listCalls != 2 {
		t.Fatalf("provider calls create=%d list=%d, want one create and two pages", createCalls, listCalls)
	}
	assertCheckoutCompleted(t, f, client, plan.Message.ID, exactID)
	if _, err := purchase.New(f.store.Purchases(), func() time.Time { return f.clock.Add(time.Minute) }).CollectionBinding(t.Context(), f.account, client.Scope(), exactID); err != nil {
		t.Fatalf("binding after many-candidate recovery err=%v", err)
	}
	assertNoPaymentEffects(t, &f)
}

func manyRecoveryListResponse(ids []string, exactID string, hasMore bool, customData map[string]string, createdAt time.Time, f paymentFixture) string {
	objects := make([]string, 0, len(ids))
	for _, id := range ids {
		data := customData
		if id != exactID {
			data = nil
		}
		objects = append(objects, recoveryTransactionObject(id, data, createdAt, f))
	}
	next := ""
	if hasMore && len(ids) > 0 {
		next = "https://sandbox-api.paddle.com/transactions?after=" + ids[len(ids)-1]
	}
	return fmt.Sprintf(`{"data":[%s],"meta":{"pagination":{"has_more":%t,"next":%q}}}`, strings.Join(objects, ","), hasMore, next)
}
