package paddle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	paddle "github.com/data-insights-ai/rho-paddle"
)

type customDataAccount struct{}

func (customDataAccount) ResolveAccount(_ context.Context, _ billing.Scope, ev paddle.Event) (billing.AccountID, error) {
	// A host reads the correlation through the adapter, never by parsing
	// Paddle JSON itself.
	account, _, _ := ev.Correlation()
	if !billing.ValidID(string(account)) {
		return "", paddle.ErrInvalid
	}
	return account, nil
}

type inboundWebhook struct {
	body    []byte
	headers integration.WebhookHeaders
}

func TestSandboxLiveWebhookReceivesPlatformPOST(t *testing.T) {
	if os.Getenv("PADDLE_SANDBOX_TEST") != "1" {
		t.Skip("set PADDLE_SANDBOX_TEST=1 and PADDLE_SANDBOX_KEY_FILE for live webhook evidence")
	}
	keyBytes, err := os.ReadFile(os.Getenv("PADDLE_SANDBOX_KEY_FILE"))
	if err != nil {
		t.Fatal("sandbox key file is unavailable")
	}
	key := strings.TrimSpace(string(keyBytes))
	if !strings.HasPrefix(key, "pdl_sdbx_apikey_") {
		t.Fatal("expected a sandbox credential")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	received := make(chan inboundWebhook, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /paddle", func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if readErr != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		select {
		case received <- inboundWebhook{body: body, headers: integration.WebhookHeaders(r.Header.Clone())}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	ngrok := exec.Command("ngrok", "http", fmt.Sprintf("%d", port), "--log=stdout", "--log-format=json")
	stdout, err := ngrok.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	ngrok.Stderr = io.Discard
	if err := ngrok.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ngrok.Process.Kill() })
	publicURL := waitNgrokHTTPS(t, stdout)
	client, err := paddle.New(paddle.Config{Merchant: "merchant-test", Environment: paddle.Sandbox, APIKey: key, HTTPClient: &http.Client{Timeout: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	setting, err := client.CreateNotificationSetting(t.Context(), paddle.NotificationSettingInput{
		Description: fmt.Sprintf("rho-billing-live-%d", time.Now().UnixNano()),
		Destination: publicURL + "/paddle",
		Events:      []string{"customer.created"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.DeleteNotificationSetting(context.Background(), setting.Reference)
	})
	if setting.Secret == "" || strings.Contains(setting.Destination, setting.Secret) {
		t.Fatal("missing destination secret")
	}

	run := fmt.Sprintf("rho-wh-%d", time.Now().UnixNano())
	account := billing.AccountID("live-webhook")
	customer, err := client.CreateCustomer(t.Context(), paddle.CustomerInput{Account: account, Operation: run, Email: run + "@example.com", Name: "Rho Billing Webhook"})
	if err != nil {
		t.Fatal(err)
	}
	setup := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() {
		sandboxEntity(t, setup, key, http.MethodPatch, "/customers/"+customer.Reference.ID, map[string]string{"status": "archived"})
	})

	var in inboundWebhook
	select {
	case in = <-received:
	case <-time.After(45 * time.Second):
		t.Fatal("no webhook POST received")
	}
	verifier, err := paddle.NewWebhookVerifier(paddle.WebhookConfig{
		Merchant:    "merchant-test",
		Environment: paddle.Sandbox,
		Secrets:     []string{setting.Secret},
		Resolver:    customDataAccount{},
		Now:         time.Now,
		Tolerance:   time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := verifier.Verify(t.Context(), in.body, in.headers)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Kind != "customer.created" || msg.Account != account || msg.ID == "" {
		t.Fatalf("message=%+v", msg)
	}
	t.Logf("live webhook POST verified kind=%s event=%s customer=%s destination=%s", msg.Kind, msg.ID, customer.Reference.ID, setting.Reference.ID)
}

func waitNgrokHTTPS(t *testing.T, logs io.Reader) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		dec := json.NewDecoder(logs)
		for {
			var rec map[string]any
			if err := dec.Decode(&rec); err != nil {
				return
			}
			u, _ := rec["url"].(string)
			if strings.HasPrefix(u, "https://") {
				select {
				case done <- strings.TrimRight(u, "/"):
				default:
				}
				return
			}
		}
	}()
	select {
	case url := <-done:
		return url
	case <-time.After(20 * time.Second):
		t.Fatal("ngrok did not publish an https URL")
		return ""
	}
}
