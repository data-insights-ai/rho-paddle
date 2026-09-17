package paddlehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDoSuccessAndHeaders(t *testing.T) {
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://sandbox-api.paddle.com/transactions?include=customer" {
			t.Fatalf("URL = %q", req.URL.String())
		}
		if req.Header.Get("Authorization") != "Bearer secret" || req.Header.Get("Paddle-Version") != "1" {
			t.Fatal("missing Paddle headers")
		}
		return response(http.StatusOK, `{"data":{}}`), nil
	})

	got, err := client.Do(t.Context(), http.MethodGet, "/transactions?include=customer", nil)
	if err != nil || string(got.Body) != `{"data":{}}` {
		t.Fatalf("Do() = %#v, %v", got, err)
	}
}

func TestMutationTransportFailureIsUncertainAndNeverRetried(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("secret network detail")
	})
	_, err := client.Do(t.Context(), http.MethodPost, "/transactions", []byte(`{"secret":"payload"}`))
	if !errors.Is(err, ErrUncertain) || calls.Load() != 1 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestCanceledContextDoesNotCallTransport(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusOK, `{}`), nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := client.Do(ctx, http.MethodPost, "/transactions", nil)
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
}

func TestTimeoutAfterMutationAttemptPreservesAmbiguityAndCancellation(t *testing.T) {
	client := newTestClient(t, func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	_, err := client.Do(ctx, http.MethodPost, "/transactions", nil)
	if !errors.Is(err, ErrUncertain) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := response(http.StatusFound, `{}`)
		resp.Header.Set("Location", "https://evil.example/collect")
		return resp, nil
	})
	_, err := client.Do(t.Context(), http.MethodPost, "/transactions", nil)
	if !errors.Is(err, ErrUncertain) || calls.Load() != 1 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
}

func TestSecretsNeverAppearInErrors(t *testing.T) {
	const key = "highly-sensitive-api-key"
	client, err := New(Config{APIKey: key, BaseURL: "https://api.paddle.com", HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network contains " + key)
	})}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(t.Context(), http.MethodPost, "/transactions", []byte(`{"private":"body"}`))
	if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "network") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestProviderErrorsAndAmbiguity(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		status    int
		body      string
		uncertain bool
	}{
		{"known 4xx", http.MethodPost, 422, `{"error":{"code":"bad_request"},"meta":{"request_id":"req_1"}}`, false},
		{"408", http.MethodPost, 408, `{"error":{"code":"timeout"}}`, true},
		{"5xx", http.MethodPost, 500, `{"error":{"code":"internal"}}`, true},
		{"redirect", http.MethodPost, 302, `{}`, true},
		{"malformed success", http.MethodPost, 200, `{"data":`, true},
		{"get 5xx", http.MethodGet, 500, `{"error":{"code":"internal"}}`, false},
		{"429", http.MethodPost, 429, `{"error":{"code":"too_many_requests"}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, func(*http.Request) (*http.Response, error) { return response(tc.status, tc.body), nil })
			_, err := client.Do(t.Context(), tc.method, "/resource", nil)
			if errors.Is(err, ErrUncertain) != tc.uncertain {
				t.Fatalf("err = %v, uncertain = %v", err, errors.Is(err, ErrUncertain))
			}
		})
	}
}

func TestTooManyRequestsExposesRetryAfterAndIsNotUncertain(t *testing.T) {
	client := newTestClient(t, func(*http.Request) (*http.Response, error) {
		resp := response(http.StatusTooManyRequests, `{"error":{"code":"too_many_requests"},"meta":{"request_id":"req_1"}}`)
		resp.Header.Set("Retry-After", "42")
		return resp, nil
	})
	_, err := client.Do(t.Context(), http.MethodPost, "/subscriptions/sub_01aaaaaaaaaaaaaaaaaaaaaaaaaa/charge", []byte(`{}`))
	if errors.Is(err, ErrUncertain) || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err=%v", err)
	}
	limited, ok := errors.AsType[*Error](err)
	if !ok || limited.RetryAfter != 42*time.Second || limited.Code != "too_many_requests" || limited.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate limit=%#v ok=%v", limited, ok)
	}
}

func TestRejectsInvalidConfigurationAndPaths(t *testing.T) {
	for _, baseURL := range []string{"http://api.paddle.com", "https://evil.example", "https://api.paddle.com/"} {
		if _, err := New(Config{APIKey: "secret", BaseURL: baseURL}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("base URL %q: %v", baseURL, err)
		}
	}
	client := newTestClient(t, func(*http.Request) (*http.Response, error) { t.Fatal("transport called"); return nil, nil })
	for _, path := range []string{"https://evil.example/x", "//evil.example/x", "/x#fragment", `/x\\y`, "relative"} {
		if _, err := client.Do(t.Context(), http.MethodGet, path, nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("path %q: %v", path, err)
		}
	}
}

func TestDoNoContentDelete(t *testing.T) {
	client := newTestClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusNoContent, ""), nil
	})
	got, err := client.Do(t.Context(), http.MethodDelete, "/notification-settings/ntfset_01gkpjp8bkm3tm53kdgkx6sms7", nil)
	if err != nil || len(got.Body) != 0 || got.StatusCode != http.StatusNoContent {
		t.Fatalf("Do() = %#v, %v", got, err)
	}
}

func TestResponseLimit(t *testing.T) {
	client, err := New(Config{APIKey: "secret", BaseURL: "https://sandbox-api.paddle.com", MaxResponseBytes: 2, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{}`+"x"), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(t.Context(), http.MethodPost, "/resource", nil)
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("err = %v", err)
	}
}

func newTestClient(t *testing.T, fn roundTripFunc) *Client {
	t.Helper()
	client, err := New(Config{APIKey: "secret", BaseURL: "https://sandbox-api.paddle.com", HTTPClient: &http.Client{Transport: fn}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
