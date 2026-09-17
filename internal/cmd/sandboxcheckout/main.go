// Command sandboxcheckout serves a local Paddle.js overlay so an operator can
// capture a sandbox payment with the 4242 test card. It is not a product UI.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

const paddleAPI = "https://sandbox-api.paddle.com"

type session struct {
	Token         string `json:"token"`
	TransactionID string `json:"transaction_id"`
	CustomerID    string `json:"customer_id"`
	Email         string `json:"email"`
}

type statusPayload struct {
	TransactionID   string `json:"transaction_id"`
	Status          string `json:"status"`
	PaymentMethodID string `json:"payment_method_id,omitempty"`
}

func main() {
	log.SetFlags(0)
	keyPath := os.Getenv("PADDLE_SANDBOX_KEY_FILE")
	if keyPath == "" {
		keyPath = filepath.Join(os.Getenv("HOME"), ".config/data-insights/rho-paddle/sandbox-api-key")
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatal("sandbox API key file is unavailable")
	}
	key := strings.TrimSpace(string(keyBytes))
	if !strings.HasPrefix(key, "pdl_sdbx_apikey_") {
		log.Fatal("expected a sandbox API key")
	}
	addr := os.Getenv("SANDBOX_CHECKOUT_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	origin := "http://" + addr
	if host, port, err := net.SplitHostPort(addr); err == nil && (host == "127.0.0.1" || host == "localhost" || host == "") {
		origin = "http://localhost:" + port
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}
	token, err := clientSideToken(ctx, client, key, filepath.Join(filepath.Dir(keyPath), "sandbox-client-token"))
	if err != nil {
		log.Fatal(err)
	}
	txn := strings.TrimSpace(os.Getenv("PADDLE_SANDBOX_TRANSACTION_ID"))
	customer := ""
	email := ""
	if strings.HasPrefix(txn, "txn_") {
		raw, err := paddleDo(ctx, client, key, http.MethodGet, "/transactions/"+txn, nil)
		if err != nil {
			log.Fatal(err)
		}
		var envelope struct {
			Data struct {
				ID         string `json:"id"`
				Status     string `json:"status"`
				CustomerID string `json:"customer_id"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &envelope) != nil || envelope.Data.ID != txn {
			log.Fatal("existing sandbox transaction is unavailable")
		}
		customer = envelope.Data.CustomerID
		log.Printf("reusing transaction=%s status=%s customer=%s", txn, envelope.Data.Status, customer)
	} else {
		run := "rho-capture-" + time.Now().UTC().Format("20060102T150405")
		email = run + "@example.com"
		product := mustEntity(ctx, client, key, http.MethodPost, "/products", map[string]any{
			"name": run, "tax_category": "saas", "custom_data": map[string]string{"purpose": "lib28-capture"},
		})
		priceBody := map[string]any{
			"product_id": product, "description": run, "tax_mode": "internal",
			"unit_price": map[string]string{"amount": "100", "currency_code": "USD"},
		}
		if os.Getenv("PADDLE_SANDBOX_RECURRING") == "1" {
			priceBody["billing_cycle"] = map[string]any{"interval": "month", "frequency": 1}
		}
		price := mustEntity(ctx, client, key, http.MethodPost, "/prices", priceBody)
		customer = mustEntity(ctx, client, key, http.MethodPost, "/customers", map[string]any{
			"email": email, "name": "Rho Billing Capture",
		})
		address := mustEntity(ctx, client, key, http.MethodPost, "/customers/"+customer+"/addresses", map[string]any{
			"country_code": "US", "postal_code": "10021", "city": "New York", "region": "NY", "first_line": "1 Capture Street",
		})
		txn = mustEntity(ctx, client, key, http.MethodPost, "/transactions", map[string]any{
			"items":           []map[string]any{{"price_id": price, "quantity": 1}},
			"customer_id":     customer,
			"address_id":      address,
			"collection_mode": "automatic",
			"currency_code":   "USD",
		})
		log.Printf("transaction=%s customer=%s product=%s price=%s", txn, customer, product, price)
	}

	state := &captureState{session: session{Token: token, TransactionID: txn, CustomerID: customer, Email: email}, status: "draft"}
	go pollStatus(client, key, state)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, state.snapshot())
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, state.statusPayload())
	})
	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var ev struct {
			EventID   string `json:"event_id"`
			EventType string `json:"event_type"`
		}
		_ = json.Unmarshal(body, &ev)
		if ev.EventType != "" {
			log.Printf("webhook event_type=%s event_id=%s", ev.EventType, ev.EventID)
		}
		w.WriteHeader(http.StatusOK)
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("open %s", origin+"/")
	log.Printf("pay with 4242 4242 4242 4242, any future expiry, CVC 100")
	log.Printf("transaction=%s customer=%s", txn, customer)
	log.Printf("webhooks: Paddle cannot reach this process until you add a public HTTPS destination; capture is polled from GET /transactions")
	if err := http.Serve(ln, mux); err != nil {
		log.Fatal(err)
	}
}

type captureState struct {
	mu      sync.Mutex
	session session
	status  string
	method  string
}

func (s *captureState) snapshot() session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

func (s *captureState) statusPayload() statusPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return statusPayload{TransactionID: s.session.TransactionID, Status: s.status, PaymentMethodID: s.method}
}

func (s *captureState) set(status, method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if status != "" {
		s.status = status
	}
	if method != "" {
		s.method = method
	}
}

func pollStatus(client *http.Client, key string, state *captureState) {
	for {
		time.Sleep(2 * time.Second)
		id := state.snapshot().TransactionID
		raw, err := paddleDo(context.Background(), client, key, http.MethodGet, "/transactions/"+id+"?include=address", nil)
		if err != nil {
			continue
		}
		var envelope struct {
			Data struct {
				ID       string `json:"id"`
				Status   string `json:"status"`
				Payments []struct {
					Status          string `json:"status"`
					PaymentMethodID string `json:"payment_method_id"`
				} `json:"payments"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &envelope) != nil {
			continue
		}
		method := ""
		for _, payment := range envelope.Data.Payments {
			if payment.PaymentMethodID != "" {
				method = payment.PaymentMethodID
				break
			}
		}
		prev := state.statusPayload()
		state.set(envelope.Data.Status, method)
		if envelope.Data.Status != prev.Status {
			log.Printf("transaction=%s status=%s payment_method=%s", envelope.Data.ID, envelope.Data.Status, method)
		}
	}
}

func clientSideToken(ctx context.Context, client *http.Client, key, path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(raw))
		if strings.HasPrefix(token, "test_") {
			return token, nil
		}
	}
	body, err := paddleDo(ctx, client, key, http.MethodPost, "/client-tokens", map[string]string{
		"name":        "rho-billing-sandbox-capture",
		"description": "Local operator overlay for sandbox captured-payment evidence",
	})
	if err != nil {
		return "", err
	}
	var envelope struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || !strings.HasPrefix(envelope.Data.Token, "test_") {
		if envelope.Error.Code != "" {
			return "", fmt.Errorf("create client-side token: %s", envelope.Error.Code)
		}
		return "", fmt.Errorf("create client-side token: unexpected response")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(envelope.Data.Token+"\n"), 0o600); err != nil {
		return "", err
	}
	return envelope.Data.Token, nil
}

func mustEntity(ctx context.Context, client *http.Client, key, method, path string, payload any) string {
	body, err := paddleDo(ctx, client, key, method, path, payload)
	if err != nil {
		log.Fatal(err)
	}
	var envelope struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Data.ID == "" {
		if envelope.Error.Code != "" {
			log.Fatalf("%s %s: %s", method, path, envelope.Error.Code)
		}
		log.Fatalf("%s %s: unexpected response", method, path)
	}
	return envelope.Data.ID
}

func paddleDo(ctx context.Context, client *http.Client, key, method, path string, payload any) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, paddleAPI+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Paddle-Version", "1")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}
