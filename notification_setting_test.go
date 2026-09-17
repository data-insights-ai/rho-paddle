package paddle_test

import (
	"encoding/json"
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"net/http"
	"strings"
	"testing"

	paddle "github.com/data-insights-ai/rho-paddle"
)

const (
	testNtfsetID  = "ntfset_0123456789abcdefghijklmnop"
	testNtfSecret = "pdl_ntfset_0123456789abcdefghijklmnop_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123"
)

func TestCreateNotificationSettingWireMapping(t *testing.T) {
	const dest = "https://hooks.example.test/paddle"
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodPost, "https://sandbox-api.paddle.com/notification-settings")
		var body struct {
			Description      string   `json:"description"`
			Type             string   `json:"type"`
			Destination      string   `json:"destination"`
			APIVersion       int      `json:"api_version"`
			TrafficSource    string   `json:"traffic_source"`
			SubscribedEvents []string `json:"subscribed_events"`
		}
		decodeRequest(t, req, &body)
		if body.Description != "rho-billing-live" || body.Type != "url" || body.Destination != dest || body.APIVersion != 1 || body.TrafficSource != "platform" || strings.Join(body.SubscribedEvents, ",") != "customer.created" {
			t.Fatalf("request body = %#v", body)
		}
		return jsonResponse(http.StatusCreated, notificationSettingJSON(dest)), nil
	})
	got, err := client.CreateNotificationSetting(t.Context(), paddle.NotificationSettingInput{
		Description: "rho-billing-live",
		Destination: dest,
		Events:      []string{"customer.created"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Reference.ID != testNtfsetID || got.Destination != dest || !got.Active || got.Secret != testNtfSecret || strings.Join(got.Events, ",") != "customer.created" {
		t.Fatalf("setting = %#v", got)
	}
	if strings.Contains(got.Reference.ID, testNtfSecret) {
		t.Fatal("reference contains secret")
	}
}

func TestCreateNotificationSettingRejectsHTTPAndEmptyEvents(t *testing.T) {
	client := newClient(t, paddle.Sandbox, func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected transport")
		return nil, errors.New("unused")
	})
	if _, err := client.CreateNotificationSetting(t.Context(), paddle.NotificationSettingInput{Description: "x", Destination: "http://hooks.example.test/paddle", Events: []string{"customer.created"}}); !errors.Is(err, paddle.ErrInvalid) {
		t.Fatalf("http destination err = %v", err)
	}
	if _, err := client.CreateNotificationSetting(t.Context(), paddle.NotificationSettingInput{Description: "x", Destination: "https://hooks.example.test/paddle"}); !errors.Is(err, paddle.ErrInvalid) {
		t.Fatalf("empty events err = %v", err)
	}
}

func TestDeleteNotificationSetting(t *testing.T) {
	client := newClient(t, paddle.Sandbox, func(req *http.Request) (*http.Response, error) {
		assertRequest(t, req, http.MethodDelete, "https://sandbox-api.paddle.com/notification-settings/"+testNtfsetID)
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
	})
	ref := billing.Reference{Scope: client.Scope(), ID: testNtfsetID}
	if err := client.DeleteNotificationSetting(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedNotificationSecretIsNotInError(t *testing.T) {
	const dest = "https://hooks.example.test/paddle"
	const bad = "plaintext-destination-secret"
	client := newClient(t, paddle.Sandbox, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusCreated, `{"data":{"id":"`+testNtfsetID+`","description":"rho-billing-live","type":"url","destination":"`+dest+`","active":true,"endpoint_secret_key":"`+bad+`","subscribed_events":[{"name":"customer.created"}]}}`), nil
	})
	_, err := client.CreateNotificationSetting(t.Context(), paddle.NotificationSettingInput{
		Description: "rho-billing-live",
		Destination: dest,
		Events:      []string{"customer.created"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), bad) {
		t.Fatalf("unsafe error = %v", err)
	}
}

func notificationSettingJSON(dest string) string {
	events, _ := json.Marshal([]map[string]string{{"name": "customer.created"}})
	return `{"data":{"id":"` + testNtfsetID + `","description":"rho-billing-live","type":"url","destination":"` + dest + `","active":true,"endpoint_secret_key":"` + testNtfSecret + `","subscribed_events":` + string(events) + `}}`
}
