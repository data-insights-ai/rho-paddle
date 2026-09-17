package paddlehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	defaultMaxResponseBytes = int64(1 << 20)
	maxMaxResponseBytes     = int64(16 << 20)
	defaultTimeout          = 30 * time.Second
	maxRetryAfter           = 24 * time.Hour
)

var (
	ErrInvalid     = errors.New("paddlehttp: invalid input")
	ErrUncertain   = errors.New("paddlehttp: uncertain outcome")
	ErrRateLimited = errors.New("paddlehttp: rate limited")
)

type Config struct {
	APIKey           string
	BaseURL          string
	HTTPClient       *http.Client
	MaxResponseBytes int64
}

type Client struct {
	apiKey           string
	baseURL          string
	httpClient       *http.Client
	maxResponseBytes int64
}

type Response struct {
	Body       []byte
	StatusCode int
	RequestID  string
}

type Error struct {
	StatusCode int
	Code       string
	RequestID  string
	Uncertain  bool
	RetryAfter time.Duration

	cause error
}

func (e *Error) Error() string {
	if e == nil {
		return "paddlehttp: error"
	}
	return fmt.Sprintf("paddlehttp: status=%d code=%s request_id=%s", e.StatusCode, safeToken(e.Code), safeToken(e.RequestID))
}

func (e *Error) Is(target error) bool {
	if e == nil {
		return false
	}
	if target == ErrUncertain && e.Uncertain {
		return true
	}
	return target == ErrRateLimited && e.StatusCode == http.StatusTooManyRequests
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func New(config Config) (*Client, error) {
	if config.APIKey == "" || len(config.APIKey) > 4096 || strings.TrimSpace(config.APIKey) != config.APIKey || hasControl(config.APIKey) {
		return nil, ErrInvalid
	}
	if config.BaseURL != "https://api.paddle.com" && config.BaseURL != "https://sandbox-api.paddle.com" {
		return nil, ErrInvalid
	}
	if config.MaxResponseBytes < 0 || config.MaxResponseBytes > maxMaxResponseBytes {
		return nil, ErrInvalid
	}

	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clone := *httpClient
	if clone.Timeout <= 0 {
		clone.Timeout = defaultTimeout
	}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &Client{
		apiKey:           config.APIKey,
		baseURL:          config.BaseURL,
		httpClient:       &clone,
		maxResponseBytes: maxResponseBytes,
	}, nil
}

func (c *Client) Do(ctx context.Context, method, path string, body []byte) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if !validPath(path) || method == "" || hasControl(method) {
		return Response{}, ErrInvalid
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return Response{}, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Paddle-Version", "1")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Response{}, c.transportError(ctx, method)
	}
	defer resp.Body.Close()

	requestID := c.diagnostic(resp.Header.Get("Paddle-Request-Id"))
	responseBody, oversized, readErr := readBounded(resp.Body, c.maxResponseBytes)
	result := Response{StatusCode: resp.StatusCode, RequestID: requestID}
	uncertain := isMutation(method) && (resp.StatusCode == http.StatusRequestTimeout ||
		(resp.StatusCode >= 300 && resp.StatusCode <= 399) || resp.StatusCode >= 500)
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	if resp.StatusCode == http.StatusTooManyRequests {
		uncertain = false
	}
	if readErr != nil || oversized {
		return result, newError(resp.StatusCode, "invalid_response", requestID, isMutation(method) && resp.StatusCode != http.StatusTooManyRequests, retryAfter, contextCause(ctx))
	}

	if resp.StatusCode == http.StatusNoContent {
		return result, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if !validJSONObject(responseBody) {
			return result, newError(resp.StatusCode, "invalid_response", requestID, isMutation(method), retryAfter, contextCause(ctx))
		}
		result.Body = responseBody
		return result, nil
	}

	code, bodyRequestID, structured := providerError(responseBody)
	code = c.diagnostic(code)
	bodyRequestID = c.diagnostic(bodyRequestID)
	if requestID == "" {
		requestID = bodyRequestID
		result.RequestID = requestID
	}
	if !structured {
		code = "invalid_response"
		if isMutation(method) && resp.StatusCode != http.StatusTooManyRequests {
			uncertain = true
		}
	}
	return result, newError(resp.StatusCode, code, requestID, uncertain, retryAfter, contextCause(ctx))
}

func (c *Client) diagnostic(value string) string {
	if strings.Contains(value, c.apiKey) {
		return ""
	}
	return safeToken(value)
}

func (c *Client) transportError(ctx context.Context, method string) error {
	cause := contextCause(ctx)
	return newError(0, "transport", "", isMutation(method), 0, cause)
}

func newError(status int, code, requestID string, uncertain bool, retryAfter time.Duration, cause error) *Error {
	return &Error{StatusCode: status, Code: safeToken(code), RequestID: safeToken(requestID), Uncertain: uncertain, RetryAfter: retryAfter, cause: cause}
}

// parseRetryAfter accepts both RFC 7231 forms: delta-seconds and an HTTP-date.
// A date in the past yields zero, which callers read as "no delay supplied".
func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds < 0 || seconds > int64(maxRetryAfter/time.Second) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	delay := time.Until(when)
	if delay <= 0 {
		return 0
	}
	return min(delay, maxRetryAfter)
}

func contextCause(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func validPath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\\#") {
		return false
	}
	u, err := url.ParseRequestURI(path)
	return err == nil && !u.IsAbs() && u.Host == "" && u.Fragment == "" && !strings.ContainsRune(u.Path, '\\')
}

func isMutation(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func readBounded(r io.Reader, limit int64) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limit {
		return nil, true, nil
	}
	return body, false, nil
}

func validJSONObject(body []byte) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(body, &value) == nil && value != nil
}

func providerError(body []byte) (code, requestID string, ok bool) {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Meta struct {
			RequestID string `json:"request_id"`
		} `json:"meta"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return "", "", false
	}
	code = safeToken(envelope.Error.Code)
	requestID = safeToken(envelope.Meta.RequestID)
	return code, requestID, code != ""
}

func safeToken(value string) string {
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:-", r)) {
			return ""
		}
	}
	return value
}

func hasControl(value string) bool {
	return strings.ContainsFunc(value, unicode.IsControl)
}
