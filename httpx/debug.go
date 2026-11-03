package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxLoggedBody bounds how much of a request or response body a debug record
// carries.
const MaxLoggedBody = 8 << 10

const redacted = "[REDACTED]"

// sensitiveKey reports whether a header or JSON/form field name is likely to
// hold a credential, and so must never be logged.
func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"authorization", "secret", "token", "password", "passwd", "api-key", "api_key", "apikey",
		"cookie", "signature", "credential", "client-id", "client_id", "client-context", "assertion", "otp"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// RedactHeader returns a copy of h with credential-bearing values replaced.
func RedactHeader(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if sensitiveKey(k) {
			out[k] = redacted
		} else {
			out[k] = strings.Join(vs, ", ")
		}
	}
	return out
}

func redactJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if sensitiveKey(k) {
				t[k] = redacted
			} else {
				t[k] = redactJSON(e)
			}
		}
	case []any:
		for i, e := range t {
			t[i] = redactJSON(e)
		}
	}
	return v
}

// RedactBody renders a body for a debug record: credential fields in JSON and
// form bodies are masked and the text is truncated. Business data (names,
// phones, addresses, amounts) is left intact, which is the point of debug
// logging; run production at info or above if that is unacceptable. A body
// that is neither JSON nor a form is logged only as a length.
func RedactBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(body, &v); err == nil {
		if b, err := json.Marshal(redactJSON(v)); err == nil {
			return clip(string(b))
		}
	}
	if q, err := url.ParseQuery(string(body)); err == nil && len(q) > 0 && !strings.ContainsAny(string(body), "<>") {
		for k := range q {
			if sensitiveKey(k) {
				q.Set(k, redacted)
			}
		}
		return clip(q.Encode())
	}
	return "[" + itoa(len(body)) + " bytes, not JSON]"
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func clip(s string) string {
	if len(s) <= MaxLoggedBody {
		return s
	}
	s = s[:MaxLoggedBody]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s + "…[truncated]"
}

// redactURL drops the query string values (they can carry credentials or
// personal data) but keeps the names.
func redactURL(u *url.URL) string {
	c := *u
	c.User = nil
	if c.RawQuery != "" {
		q := c.Query()
		for k := range q {
			q.Set(k, "…")
		}
		c.RawQuery = q.Encode()
	}
	return c.String()
}

func (c *Client) debug(ctx context.Context, method string, u *url.URL, r Request, attempt int, dur time.Duration, resp *Response, err error) {
	if !c.log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	reqH := http.Header{}
	for k, vs := range r.Header {
		reqH[k] = vs
	}
	for k := range r.Auth {
		reqH[k] = []string{redacted}
	}
	attrs := []slog.Attr{
		slog.String("provider", string(c.provider)), slog.String("op", r.Op), slog.Int("attempt", attempt),
		slog.String("method", method), slog.String("url", redactURL(u)), slog.Duration("duration", dur),
		slog.Any("request_headers", RedactHeader(reqH)), slog.String("request_body", RedactBody(r.Body)),
	}
	if resp != nil {
		attrs = append(attrs, slog.Int("status", resp.Status), slog.Any("response_headers", RedactHeader(resp.Header)),
			slog.String("response_body", RedactBody(resp.Body)))
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	c.log.LogAttrs(ctx, slog.LevelDebug, "courier http exchange", attrs...)
}
