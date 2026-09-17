package paddle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	corepostgres "github.com/data-insights-ai/rho-billing/postgres"
	paddle "github.com/data-insights-ai/rho-paddle"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestCheckoutRecoveryPostgresConcurrentCheckpointFence(t *testing.T) {
	f := newPaymentFixture(t, "checkout-recovery-race", false)
	quoteLineID := f.binding.Lines[0].Allocations[0].QuoteLineID
	candidateID := "txn_zyxwvutsrqponmlkjihgfedcba"
	var postCalls atomic.Int64
	var customData map[string]string
	initial, err := paddle.New(paddle.Config{
		Merchant:    "merchant-test",
		Environment: paddle.Sandbox,
		APIKey:      testAPIKey,
		HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || req.URL.Path != "/transactions" {
				return nil, fmt.Errorf("unexpected initial request %s %s", req.Method, req.URL.String())
			}
			postCalls.Add(1)
			var body struct {
				CustomerID     string            `json:"customer_id"`
				CollectionMode string            `json:"collection_mode"`
				Items          []map[string]any  `json:"items"`
				CustomData     map[string]string `json:"custom_data"`
			}
			decodeRequest(t, req, &body)
			customData = body.CustomData
			return nil, context.DeadlineExceeded
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	initialService, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: initial, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := initialService.PlanCheckout(t.Context(), f.intent.IntentInput, paddle.CustomerReference{Account: f.account, Reference: billing.Reference{Scope: initial.Scope(), ID: f.binding.CustomerID}}, []paddle.CheckoutDispatchLine{{QuoteLineID: quoteLineID, Price: billing.Reference{Scope: initial.Scope(), ID: f.binding.Lines[0].ProviderPriceID}, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "recovery-race-dispatch", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if got, err := initialService.Dispatch(t.Context(), claim); !errors.Is(err, paddle.ErrCheckoutUnresolved) || got.Reference.ID != "" {
		t.Fatalf("dispatch transaction=%+v error=%v, want zero unresolved", got, err)
	}

	second := checkoutSecondStore(t, f.db)
	arrived := make(chan struct{})
	var arrivals atomic.Int64
	var getCalls atomic.Int64
	recoveryClient, err := paddle.New(paddle.Config{
		Merchant:    "merchant-test",
		Environment: paddle.Sandbox,
		APIKey:      testAPIKey,
		HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				postCalls.Add(1)
				return nil, fmt.Errorf("unexpected recovery mutation %s %s", req.Method, req.URL.String())
			}
			getCalls.Add(1)
			switch req.URL.Path {
			case "/transactions":
				createdAt, parseErr := time.Parse(time.RFC3339Nano, req.URL.Query().Get("created_at[GT]"))
				if parseErr != nil {
					return nil, parseErr
				}
				createdAt = createdAt.Add(time.Minute)
				if arrivals.Add(1) == 2 {
					close(arrived)
				}
				select {
				case <-arrived:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				return jsonResponse(http.StatusOK, recoveryListResponse(candidateID, false, customData, createdAt, f)), nil
			case "/transactions/" + candidateID:
				return jsonResponse(http.StatusOK, recoveryTransactionResponse(candidateID, customData, f.clock.Add(time.Minute), f)), nil
			default:
				return nil, fmt.Errorf("unexpected recovery request %s %s", req.Method, req.URL.String())
			}
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstService, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: recoveryClient, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(2 * time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	secondService, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: recoveryClient, Repository: second.Queue(), Now: func() time.Time { return f.clock.Add(3 * time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	type recoveryResult struct {
		tx  paddle.Transaction
		err error
	}
	results := make(chan recoveryResult, 2)
	var wg sync.WaitGroup
	for i, service := range []*paddle.CheckoutService{firstService, secondService} {
		wg.Go(func() {
			tx, err := service.RecoverCheckout(t.Context(), f.account, plan.Message.ID, fmt.Sprintf("recovery-race-%d", i))
			results <- recoveryResult{tx: tx, err: err}
		})
	}
	wg.Wait()
	close(results)
	var completed, conflicted int
	for result := range results {
		switch {
		case result.err == nil:
			completed++
			if result.tx.Reference.ID != candidateID {
				t.Fatalf("completed transaction=%+v, want candidate", result.tx)
			}
		case errors.Is(result.err, billing.ErrConflict):
			conflicted++
			if result.tx.Reference.ID != "" || result.tx.CheckoutURL != "" {
				t.Fatalf("conflicted recovery returned transaction=%+v", result.tx)
			}
		default:
			t.Fatalf("concurrent recovery error=%v", result.err)
		}
	}
	if completed != 1 || conflicted != 1 {
		t.Fatalf("completed=%d conflicted=%d, want one each", completed, conflicted)
	}

	fresh, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: recoveryClient, Repository: second.Queue(), Now: func() time.Time { return f.clock.Add(4 * time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fresh.RecoverCheckout(t.Context(), f.account, plan.Message.ID, "recovery-race-fresh")
	if err != nil || got.Reference.ID != candidateID {
		t.Fatalf("fresh recovery transaction=%+v error=%v", got, err)
	}
	if postCalls.Load() != 1 {
		t.Fatalf("provider POST calls=%d, want original dispatch only", postCalls.Load())
	}
	if getCalls.Load() != 3 {
		t.Fatalf("provider GET calls=%d, want two simultaneous lists and one completed read", getCalls.Load())
	}
	assertCheckoutCompleted(t, f, recoveryClient, plan.Message.ID, candidateID)
	assertNoPaymentEffects(t, &f)
}

func TestCheckoutDispatchPostgresExpiredClaimSendsNothingAfterLeaseRecovery(t *testing.T) {
	f := newPaymentFixture(t, "checkout-expired-dispatch", false)
	var sends atomic.Int64
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: testAPIKey, HTTPClient: &http.Client{Transport: clientRoundTripFunc(func(*http.Request) (*http.Response, error) {
		sends.Add(1)
		return nil, errors.New("provider call must not run")
	})}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := paddle.NewCheckoutService(paddle.CheckoutServiceConfig{Client: client, Repository: f.store.Queue(), Now: func() time.Time { return f.clock.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	quoteLineID := f.binding.Lines[0].Allocations[0].QuoteLineID
	plan, err := service.PlanCheckout(t.Context(), f.intent.IntentInput, paddle.CustomerReference{Account: f.account, Reference: billing.Reference{Scope: client.Scope(), ID: f.binding.CustomerID}}, []paddle.CheckoutDispatchLine{{QuoteLineID: quoteLineID, Price: billing.Reference{Scope: client.Scope(), ID: f.binding.Lines[0].ProviderPriceID}, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := f.store.Queue().Claim(t.Context(), integration.Outbound, "expired-dispatch-worker", f.clock.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := f.db.ExecContext(t.Context(), `UPDATE billing_outbox SET lease_deadline=clock_timestamp()-interval '1 second' WHERE account_id=$1 AND message_id=$2`, f.account, plan.Message.ID); err != nil {
		t.Fatal(err)
	}
	second := checkoutSecondStore(t, f.db)
	if _, ok, err := second.Queue().Claim(t.Context(), integration.Outbound, "lease-recovery-worker", f.clock.Add(2*time.Minute), time.Minute); err != nil || ok {
		t.Fatalf("lease recovery claim ok=%v err=%v, want unknown without resend", ok, err)
	}
	if got, err := service.Dispatch(t.Context(), claim); !errors.Is(err, billing.ErrConflict) || got.Reference.ID != "" {
		t.Fatalf("stale dispatch transaction=%+v error=%v, want zero conflict", got, err)
	}
	if sends.Load() != 0 {
		t.Fatalf("provider sends=%d, want zero", sends.Load())
	}
	delivery := assertCheckoutUnknown(t, f, client, plan.Message.ID)
	if delivery.Fence != claim.Fence {
		t.Fatalf("recovered delivery fence=%d, want retained %d", delivery.Fence, claim.Fence)
	}
}

func checkoutSecondStore(t *testing.T, db *sql.DB) *corepostgres.Store {
	t.Helper()
	var schema string
	if err := db.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	secondDB, err := sql.Open("pgx", os.Getenv("BILLING_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	secondDB.SetMaxOpenConns(1)
	secondDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = secondDB.Close() })
	if _, err := secondDB.ExecContext(t.Context(), `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	return corepostgres.New(secondDB)
}
