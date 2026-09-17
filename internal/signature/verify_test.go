package signature

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"event_id":"evt_1"}`)
	secret := "pdl_ntfset_secret"
	header := signedHeader(now.Unix(), body, secret)

	tests := []struct {
		name      string
		raw       []byte
		header    string
		secrets   []string
		now       time.Time
		tolerance time.Duration
		wantErr   error
	}{
		{name: "valid", raw: body, header: header, secrets: []string{secret}, now: now, tolerance: time.Minute},
		{name: "tampered body", raw: append(bytes.Clone(body), ' '), header: header, secrets: []string{secret}, now: now, tolerance: time.Minute, wantErr: ErrSignature},
		{name: "wrong key", raw: body, header: header, secrets: []string{"wrong"}, now: now, tolerance: time.Minute, wantErr: ErrSignature},
		{name: "past boundary", raw: body, header: signedHeader(now.Add(-time.Minute).Unix(), body, secret), secrets: []string{secret}, now: now, tolerance: time.Minute},
		{name: "future boundary", raw: body, header: signedHeader(now.Add(time.Minute).Unix(), body, secret), secrets: []string{secret}, now: now, tolerance: time.Minute},
		{name: "too old", raw: body, header: signedHeader(now.Add(-time.Minute-time.Second).Unix(), body, secret), secrets: []string{secret}, now: now, tolerance: time.Minute, wantErr: ErrTimestamp},
		{name: "too far future", raw: body, header: signedHeader(now.Add(time.Minute+time.Second).Unix(), body, secret), secrets: []string{secret}, now: now, tolerance: time.Minute, wantErr: ErrTimestamp},
		{name: "empty body is signed", header: signedHeader(now.Unix(), nil, secret), secrets: []string{secret}, now: now, tolerance: time.Minute},
		{name: "zero now", raw: body, header: header, secrets: []string{secret}, tolerance: time.Minute, wantErr: ErrInvalid},
		{name: "zero tolerance", raw: body, header: header, secrets: []string{secret}, now: now, wantErr: ErrInvalid},
		{name: "negative tolerance", raw: body, header: header, secrets: []string{secret}, now: now, tolerance: -time.Second, wantErr: ErrInvalid},
		{name: "missing secrets", raw: body, header: header, now: now, tolerance: time.Minute, wantErr: ErrInvalid},
		{name: "empty secret", raw: body, header: header, secrets: []string{""}, now: now, tolerance: time.Minute, wantErr: ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.raw, tt.header, tt.secrets, tt.now, tt.tolerance)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyRotationAndMultipleSignatures(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte("raw\nbody")
	valid := signatureHex(now.Unix(), body, "new-secret")
	invalid := strings.Repeat("00", sha256.Size)
	header := "h1=" + invalid + "; ts=" + strconv.FormatInt(now.Unix(), 10) + "; h1=" + strings.ToUpper(valid)

	if err := Verify(body, header, []string{"old-secret", "new-secret"}, now, time.Second); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}

func TestVerifyRejectsMalformedHeaders(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte("body")
	validSignature := signatureHex(now.Unix(), body, "secret")

	tests := []struct {
		name    string
		header  string
		wantErr error
	}{
		{name: "empty", wantErr: ErrInvalid},
		{name: "missing timestamp", header: "h1=" + validSignature, wantErr: ErrTimestamp},
		{name: "duplicate timestamp", header: "ts=1700000000;ts=1700000000;h1=" + validSignature, wantErr: ErrTimestamp},
		{name: "zero timestamp", header: "ts=0;h1=" + validSignature, wantErr: ErrTimestamp},
		{name: "negative timestamp", header: "ts=-1;h1=" + validSignature, wantErr: ErrTimestamp},
		{name: "timestamp overflow", header: "ts=9223372036854775808;h1=" + validSignature, wantErr: ErrTimestamp},
		{name: "unknown field", header: "ts=1700000000;v1=x;h1=" + validSignature, wantErr: ErrInvalid},
		{name: "missing signature", header: "ts=1700000000", wantErr: ErrInvalid},
		{name: "short signature", header: "ts=1700000000;h1=00", wantErr: ErrInvalid},
		{name: "oversized valid hex signature", header: "ts=1700000000;h1=" + strings.Repeat("a", 66), wantErr: ErrInvalid},
		{name: "non hex signature", header: "ts=1700000000;h1=" + strings.Repeat("z", 64), wantErr: ErrInvalid},
		{name: "empty field", header: "ts=1700000000;;h1=" + validSignature, wantErr: ErrInvalid},
		{name: "missing equals", header: "ts=1700000000;h1", wantErr: ErrInvalid},
		{name: "too many signatures", header: manySignatures(now.Unix(), validSignature, 9), wantErr: ErrInvalid},
		{name: "oversized header", header: strings.Repeat("x", maxHeaderLength+1), wantErr: ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(body, tt.header, []string{"secret"}, now, time.Minute)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyRejectsInputBudgetOverflow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	header := signedHeader(now.Unix(), nil, "secret")

	if err := Verify(nil, header, []string{"a", "b", "c", "d", "e"}, now, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many secrets: error = %v, want %v", err, ErrInvalid)
	}
	if err := Verify(nil, header, []string{strings.Repeat("x", maxSecretLength+1)}, now, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized secret: error = %v, want %v", err, ErrInvalid)
	}
}

func TestVerifyExtremeTimestampsDoNotWrap(t *testing.T) {
	secret := "secret"
	body := []byte("body")
	now := time.Unix(1, 0)
	header := signedHeader(int64(^uint64(0)>>1), body, secret)

	if err := Verify(body, header, []string{secret}, now, time.Nanosecond); !errors.Is(err, ErrTimestamp) {
		t.Fatalf("Verify() error = %v, want %v", err, ErrTimestamp)
	}
}

func signedHeader(timestamp int64, body []byte, secret string) string {
	return "ts=" + strconv.FormatInt(timestamp, 10) + ";h1=" + signatureHex(timestamp, body, secret)
}

func signatureHex(timestamp int64, body []byte, secret string) string {
	message := strconv.AppendInt(nil, timestamp, 10)
	message = append(message, ':')
	message = append(message, body...)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(message)
	return hex.EncodeToString(mac.Sum(nil))
}

func manySignatures(timestamp int64, signature string, count int) string {
	var header strings.Builder
	header.WriteString("ts=")
	header.WriteString(strconv.FormatInt(timestamp, 10))
	for range count {
		header.WriteString(";h1=")
		header.WriteString(signature)
	}
	return header.String()
}

func FuzzVerifyRejectsMalformedInputWithoutPanic(f *testing.F) {
	f.Add("ts=1700000000;h1="+strings.Repeat("a", 66), []byte(`{}`))
	f.Add("ts=9223372036854775807;h1="+strings.Repeat("a", 64), []byte(`{}`))
	f.Fuzz(func(t *testing.T, header string, raw []byte) {
		if len(raw) > 1<<20 {
			return
		}
		_ = Verify(raw, header, []string{"synthetic-fuzz-key"}, time.Unix(1700000000, 0), 5*time.Second)
	})
}
