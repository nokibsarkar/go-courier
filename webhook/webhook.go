// Package webhook provides the shared plumbing for provider webhook handlers:
// bounded bodies, verification over the original bytes before dispatch,
// standard http.Handler output and constant-time HMAC helpers (HOOK-01..07).
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/httpx"
)

// DefaultMaxBody bounds webhook payloads (HOOK-03).
const DefaultMaxBody = 1 << 20

// Decoder verifies and decodes one request. body holds the ORIGINAL bytes.
// It returns an error for invalid signatures; such errors yield HTTP 401 and
// no event is dispatched.
type Decoder func(ctx context.Context, header http.Header, body []byte) ([]courier.Event, error)

// Config configures NewHandler.
type Config struct {
	Provider courier.ProviderID
	Decode   Decoder
	Handler  courier.EventHandler
	MaxBody  int64
	Logger   *slog.Logger
	// Method defaults to POST.
	Method string
	// AckBody, if set, is written (with AckContentType, default
	// application/json) on successful acknowledgement, for providers that
	// document an expected response body.
	AckBody        []byte
	AckContentType string
}

// NewHandler builds the standard handler. Behaviour:
//   - wrong method: 405; oversized body: 413; unreadable: 400
//   - verification/decoding failure: 401 (invalid signature) or 400 (malformed)
//   - every decoded event is dispatched; if any returns an error the response
//     is 500 so the provider may redeliver (acknowledgement follows
//     application acceptance, HOOK-04). No exactly-once guarantee is made.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.Decode == nil || cfg.Handler == nil {
		return nil, courier.Configuration(cfg.Provider, "webhook decoder and event handler are required")
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = DefaultMaxBody
	}
	if cfg.Method == "" {
		cfg.Method = http.MethodPost
	}
	log := courier.LoggerOrNop(cfg.Logger)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != cfg.Method {
			w.Header().Set("Allow", cfg.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cfg.MaxBody))
		if err != nil {
			code := http.StatusBadRequest
			if strings.Contains(err.Error(), "too large") {
				code = http.StatusRequestEntityTooLarge
			}
			http.Error(w, http.StatusText(code), code)
			return
		}
		if log.Enabled(r.Context(), slog.LevelDebug) {
			log.LogAttrs(r.Context(), slog.LevelDebug, "webhook received", slog.String("provider", string(cfg.Provider)),
				slog.Any("headers", httpx.RedactHeader(r.Header)), slog.String("body", httpx.RedactBody(body)))
		}
		events, err := cfg.Decode(r.Context(), r.Header, body)
		if err != nil {
			code := http.StatusBadRequest
			if courier.IsKind(err, courier.KindCredentials) || courier.IsKind(err, courier.KindAuthMode) {
				code = http.StatusUnauthorized
			}
			log.LogAttrs(r.Context(), slog.LevelWarn, "webhook rejected", slog.String("provider", string(cfg.Provider)), slog.Int("status", code))
			http.Error(w, http.StatusText(code), code)
			return
		}
		failed := false
		for _, ev := range events {
			if err := cfg.Handler.HandleEvent(r.Context(), ev); err != nil {
				failed = true
				log.LogAttrs(r.Context(), slog.LevelError, "webhook handler failed", slog.String("provider", string(cfg.Provider)), slog.String("event_type", ev.Type))
			}
		}
		if failed {
			http.Error(w, "event handling failed", http.StatusInternalServerError)
			return
		}
		if len(cfg.AckBody) > 0 {
			ct := cfg.AckContentType
			if ct == "" {
				ct = "application/json"
			}
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cfg.AckBody)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), nil
}

// VerifyHMACSHA256 reports whether sigHex (optionally "sha256="-prefixed) is
// the hex HMAC-SHA256 of body under key, in constant time.
func VerifyHMACSHA256(key, body []byte, sigHex string) bool {
	sigHex = strings.TrimPrefix(strings.TrimSpace(sigHex), "sha256=")
	got, err := hex.DecodeString(sigHex)
	if err != nil || len(key) == 0 {
		return false
	}
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// SignHMACSHA256 returns the lowercase hex HMAC-SHA256 (for tests and
// outbound request signing).
func SignHMACSHA256(key, data []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

// ErrInvalidSignature is the error decoders return for failed verification.
func ErrInvalidSignature(p courier.ProviderID) error {
	return &courier.Error{Kind: courier.KindCredentials, Provider: p, Op: "webhook.verify", Message: "invalid webhook signature"}
}
