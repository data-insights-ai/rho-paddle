package paddle_test

import (
	"context"
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"sync/atomic"
	"testing"

	paddle "github.com/data-insights-ai/rho-paddle"
)

func TestSubscriptionLifecycleCapabilitiesFailClosedBeforeMutation(t *testing.T) {
	var calls atomic.Int32
	client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("provider must not be contacted")
	})
	if err := billing.RequireSupported(paddle.SubscriptionLifecycleCapability("pause")); err != nil {
		t.Fatal(err)
	}
	if err := billing.RequireSupported(paddle.EffectiveFromCapability("pause", paddle.EffectiveImmediately)); err != nil {
		t.Fatal(err)
	}
	unknown, ok := errors.AsType[*billing.CapabilityError](billing.RequireSupported(paddle.SubscriptionLifecycleCapability("archive")))
	if !ok || unknown.Reason != "lifecycle_operation_not_supported" {
		t.Fatalf("archive capability=%v", unknown)
	}
	when, ok := errors.AsType[*billing.CapabilityError](billing.RequireSupported(paddle.EffectiveFromCapability("pause", "whenever")))
	if !ok || when.Reason != "effective_from_not_supported" {
		t.Fatalf("effective_from capability=%v", when)
	}
	topup, ok := errors.AsType[*billing.CapabilityError](billing.RequireSupported(paddle.AutomaticTopUpCapability()))
	if !ok || topup.Support != billing.SupportUnsupported || topup.Reason != "automatic_top_up" {
		t.Fatalf("top-up capability=%v", topup)
	}
	_, err := client.PauseSubscription(t.Context(), paddle.PauseInput{
		Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		When:         "whenever",
	})
	if capabilityReason(err) != "effective_from_not_supported" {
		t.Fatalf("pause error=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("unsupported lifecycle issued %d provider calls", calls.Load())
	}
}

func TestResumeSubscriptionNextPeriodFailsClosedBeforeMutation(t *testing.T) {
	var calls atomic.Int32
	client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("provider must not be contacted")
	})
	capErr := billing.RequireSupported(paddle.EffectiveFromCapability("resume", paddle.EffectiveNextPeriod))
	reason, ok := errors.AsType[*billing.CapabilityError](capErr)
	if !ok || reason.Reason != "resume_next_period_not_supported" {
		t.Fatalf("capability=%v", capErr)
	}
	_, err := client.ResumeSubscription(t.Context(), paddle.ResumeInput{
		Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		When:         paddle.EffectiveNextPeriod,
	})
	if capabilityReason(err) != "resume_next_period_not_supported" {
		t.Fatalf("resume error=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("resume next_billing_period issued %d provider calls", calls.Load())
	}
}

func TestPauseResumeCancelWireMapping(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		when      paddle.EffectiveFrom
		status    paddle.SubscriptionStatus
		call      func(*paddle.Client, paddle.EffectiveFrom) (paddle.Subscription, error)
	}{
		{"pause", "pause", paddle.EffectiveImmediately, paddle.SubscriptionPaused, func(c *paddle.Client, when paddle.EffectiveFrom) (paddle.Subscription, error) {
			return c.PauseSubscription(t.Context(), paddle.PauseInput{Subscription: billing.Reference{Scope: c.Scope(), ID: testSubID}, When: when})
		}},
		{"resume", "resume", paddle.EffectiveImmediately, paddle.SubscriptionActive, func(c *paddle.Client, when paddle.EffectiveFrom) (paddle.Subscription, error) {
			return c.ResumeSubscription(t.Context(), paddle.ResumeInput{Subscription: billing.Reference{Scope: c.Scope(), ID: testSubID}, When: when})
		}},
		{"cancel", "cancel", paddle.EffectiveNextPeriod, paddle.SubscriptionActive, func(c *paddle.Client, when paddle.EffectiveFrom) (paddle.Subscription, error) {
			return c.CancelSubscription(t.Context(), paddle.CancelInput{Subscription: billing.Reference{Scope: c.Scope(), ID: testSubID}, When: when})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
				assertRequest(t, req, http.MethodPost, "https://sandbox-api.paddle.com/subscriptions/"+testSubID+"/"+tt.operation)
				var body struct {
					EffectiveFrom string `json:"effective_from"`
				}
				decodeRequest(t, req, &body)
				if body.EffectiveFrom != string(tt.when) {
					t.Fatalf("body=%#v", body)
				}
				payload := `{"data":{"id":"` + testSubID + `","status":"` + string(tt.status) + `","customer_id":"` + testCustomerID + `","collection_mode":"automatic"`
				if tt.when == paddle.EffectiveNextPeriod {
					payload += `,"scheduled_change":{"action":"` + tt.operation + `","effective_at":"2026-10-01T00:00:00Z"}`
				}
				payload += `}}`
				return jsonResponse(http.StatusOK, payload), nil
			})
			got, err := tt.call(client, tt.when)
			if err != nil {
				t.Fatal(err)
			}
			if got.Reference.ID != testSubID || got.Status != tt.status {
				t.Fatalf("subscription=%#v", got)
			}
			if tt.when == paddle.EffectiveNextPeriod && (got.ScheduledChange.Action != paddle.ScheduledAction(tt.operation) || got.ScheduledChange.EffectiveAt.IsZero()) {
				t.Fatalf("scheduled_change=%#v", got.ScheduledChange)
			}
		})
	}
}

func TestPauseSubscriptionTimeoutAfterSubmitIsUncertain(t *testing.T) {
	var calls atomic.Int32
	client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, context.DeadlineExceeded
	})
	_, err := client.PauseSubscription(t.Context(), paddle.PauseInput{
		Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		When:         paddle.EffectiveImmediately,
	})
	if !errors.Is(err, paddle.ErrUncertain) {
		t.Fatalf("timeout error=%v, want uncertain", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("timeout retried provider calls=%d", calls.Load())
	}
}

func TestResumeSubscriptionTimeoutAfterSubmitIsUncertain(t *testing.T) {
	client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	_, err := client.ResumeSubscription(t.Context(), paddle.ResumeInput{
		Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		When:         paddle.EffectiveImmediately,
	})
	if !errors.Is(err, paddle.ErrUncertain) {
		t.Fatalf("timeout error=%v, want uncertain", err)
	}
}

func TestCancelSubscriptionTimeoutAfterSubmitIsUncertain(t *testing.T) {
	client := newClient(t, paddle.Live, func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	_, err := client.CancelSubscription(t.Context(), paddle.CancelInput{
		Subscription: billing.Reference{Scope: client.Scope(), ID: testSubID},
		When:         paddle.EffectiveNextPeriod,
	})
	if !errors.Is(err, paddle.ErrUncertain) {
		t.Fatalf("timeout error=%v, want uncertain", err)
	}
}
