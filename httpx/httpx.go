// Package httpx is the shared, publicly reusable HTTP transport for adapters.
//
// It implements the cross-cutting rules of the SRS: net/http with pooled
// connections, caller-context cancellation, bounded response size and
// concurrency, no redirect following (so credentials can never be forwarded to
// another origin), protected auth/transport headers, retries only for
// operations the adapter marks replay-safe, and indeterminate-outcome
// classification for mutations whose result cannot be known.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// Defaults.
const (
	DefaultMaxResponseBytes int64 = 4 << 20
	DefaultMaxConcurrent          = 16
	DefaultTimeout                = 30 * time.Second
)

// RetryPolicy bounds retries of replay-safe requests only (ERR-03, HTTP-03).
type RetryPolicy struct {
	MaxAttempts int           // total attempts including the first; <=1 disables
	BaseDelay   time.Duration // default 200ms
	MaxDelay    time.Duration // default 5s
}

// Config configures a Client. The zero value is usable.
type Config struct {
	Provider         courier.ProviderID
	HTTPClient       *http.Client // explicit trusted extension point (HTTP-01)
	UserAgent        string
	MaxResponseBytes int64
	MaxConcurrent    int
	Retry            RetryPolicy
	Logger           *slog.Logger
	// AllowInsecure permits plain http to non-loopback hosts (tests/proxies).
	AllowInsecure bool
}

// Client is safe for concurrent use and holds no per-request state.
type Client struct {
	provider courier.ProviderID
	hc       *http.Client
	ua       string
	maxBody  int64
	sem      chan struct{}
	retry    RetryPolicy
	log      *slog.Logger
	insecure bool
}

// New builds a Client. It performs no network calls (SCP-05).
func New(cfg Config) (*Client, error) {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	// Copy so we never mutate the caller's client; never follow redirects.
	cp := *hc
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if cfg.MaxResponseBytes < 0 || cfg.MaxConcurrent < 0 {
		return nil, courier.Configuration(cfg.Provider, "limits must not be negative")
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = DefaultMaxConcurrent
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "courier-go/" + courier.Version
	}
	if strings.ContainsAny(cfg.UserAgent, "\r\n") {
		return nil, courier.Configuration(cfg.Provider, "invalid user agent")
	}
	r := cfg.Retry
	if r.BaseDelay <= 0 {
		r.BaseDelay = 200 * time.Millisecond
	}
	if r.MaxDelay <= 0 {
		r.MaxDelay = 5 * time.Second
	}
	return &Client{
		provider: cfg.Provider, hc: &cp, ua: cfg.UserAgent, maxBody: cfg.MaxResponseBytes,
		sem: make(chan struct{}, cfg.MaxConcurrent), retry: r,
		log: courier.LoggerOrNop(cfg.Logger), insecure: cfg.AllowInsecure,
	}, nil
}

// Request describes one call. Header carries only non-protected extras; auth
// material goes in Auth and is applied last.
type Request struct {
	Op     string // operation name for errors/logs, e.g. "tracking.get"
	Method string
	URL    string // absolute
	Query  url.Values
	Header http.Header
	// Auth headers set by the adapter; may include protected names.
	Auth http.Header
	Body []byte
	// ContentType defaults to application/json when Body is non-empty.
	ContentType string
	// ReplaySafe marks the request as safe to retry automatically: reads, or
	// mutations with provider-documented idempotency. Defaults to false for
	// everything except GET/HEAD when Mutation is false.
	ReplaySafe bool
	// Mutation marks a state-changing request. A POST used as a read lookup
	// must set Mutation=false AND, to retry, ReplaySafe=true explicitly
	// (TRK-13).
	Mutation bool
	// ErrorMessage extracts a provider message/code from a non-2xx body.
	ErrorMessage func(status int, body []byte) (code, message string)
}

// Response is a fully read, bounded response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// protected headers cannot be set through Request.Header (AUTH-06).
var protected = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true, "host": true,
	"content-length": true, "content-type": true, "accept": true, "user-agent": true,
	"x-api-key": true, "api-key": true, "secret-key": true, "transfer-encoding": true,
	"connection": true, "origin": true, "referer": true,
}

// IsProtectedHeader reports whether name is auth- or transport-controlled.
func IsProtectedHeader(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return protected[n] || strings.HasPrefix(n, "x-auth-") || strings.HasPrefix(n, "proxy-")
}

// CheckExtraHeaders validates caller-supplied extra headers.
func CheckExtraHeaders(h http.Header) error {
	for k, vs := range h {
		if IsProtectedHeader(k) {
			return courier.Validation("header", fmt.Sprintf("header %q is protected", k))
		}
		if !validToken(k) {
			return courier.Validation("header", "invalid header name")
		}
		for _, v := range vs {
			if strings.ContainsAny(v, "\r\n") {
				return courier.Validation("header", "invalid header value")
			}
		}
	}
	return nil
}

func validToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

func (c *Client) checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, courier.Configuration(c.provider, "invalid request URL")
	}
	if u.User != nil {
		return nil, courier.Configuration(c.provider, "credentials in URL are not allowed")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !c.insecure && !isLoopback(u.Hostname()) {
			return nil, courier.Configuration(c.provider, "plain http is only allowed for loopback hosts")
		}
	default:
		return nil, courier.Configuration(c.provider, "unsupported URL scheme")
	}
	return u, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Do performs the request and returns the bounded response. For non-2xx
// statuses it returns BOTH the response and a classified *courier.Error so
// adapters can inspect the body.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	if err := CheckExtraHeaders(r.Header); err != nil {
		return nil, err
	}
	u, err := c.checkURL(r.URL)
	if err != nil {
		return nil, err
	}
	if len(r.Query) > 0 {
		q := u.Query()
		for k, vs := range r.Query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		u.RawQuery = q.Encode()
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	mutation := r.Mutation
	replaySafe := r.ReplaySafe || (!mutation && (method == http.MethodGet || method == http.MethodHead))
	attempts := 1
	if replaySafe && c.retry.MaxAttempts > 1 {
		attempts = c.retry.MaxAttempts
	}

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, c.ctxErr(r, ctx.Err(), false)
	}
	if err := ctx.Err(); err != nil {
		return nil, c.ctxErr(r, err, false)
	}

	var lastErr error
	var lastResp *Response
	for i := 0; i < attempts; i++ {
		if i > 0 {
			wait := c.backoff(i, lastErr)
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return lastResp, c.ctxErr(r, ctx.Err(), r.Mutation)
			case <-t.C:
			}
		}
		start := time.Now()
		resp, err := c.once(ctx, method, u.String(), r, mutation)
		c.log.LogAttrs(ctx, slog.LevelDebug, "courier http", slog.String("provider", string(c.provider)),
			slog.String("op", r.Op), slog.Int("attempt", i+1), slog.Duration("duration", time.Since(start)),
			slog.Int("status", statusOf(resp)), slog.Bool("ok", err == nil))
		c.debug(ctx, method, u, r, i+1, time.Since(start), resp, err)
		if err == nil {
			return resp, nil
		}
		lastErr, lastResp = err, resp
		var ce *courier.Error
		if !errors.As(err, &ce) || !ce.Retryable || ctx.Err() != nil {
			break
		}
	}
	return lastResp, lastErr
}

func statusOf(r *Response) int {
	if r == nil {
		return 0
	}
	return r.Status
}

func (c *Client) backoff(attempt int, last error) time.Duration {
	var ce *courier.Error
	if errors.As(last, &ce) && ce.RetryAfter > 0 {
		return min(ce.RetryAfter, c.retry.MaxDelay)
	}
	d := c.retry.BaseDelay << (attempt - 1)
	if d <= 0 || d > c.retry.MaxDelay {
		d = c.retry.MaxDelay
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1)) // equal jitter
}

func (c *Client) ctxErr(r Request, err error, sent bool) error {
	e := &courier.Error{Kind: courier.KindUnavailable, Provider: c.provider, Op: r.Op, Message: "request canceled or timed out", Err: err}
	if r.Mutation && sent {
		e.Kind, e.Outcome = courier.KindIndeterminate, courier.OutcomeIndeterminate
		e.Message = "request canceled or timed out; the operation may have been applied"
	}
	return e
}

func (c *Client) once(ctx context.Context, method, target string, r Request, mutation bool) (*Response, error) {
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, courier.Configuration(c.provider, "cannot build request")
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	if len(r.Body) > 0 {
		ct := r.ContentType
		if ct == "" {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}
	for k, vs := range r.Auth {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, c.ctxErr(r, ctx.Err(), true)
		}
		e := &courier.Error{Kind: courier.KindUnavailable, Provider: c.provider, Op: r.Op, Retryable: true, Message: "transport failure", Err: scrub(err)}
		if mutation {
			e.Kind, e.Outcome, e.Retryable = courier.KindIndeterminate, courier.OutcomeIndeterminate, false
			e.Message = "transport failure; the operation may have been applied"
		}
		return nil, e
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, c.ctxErr(r, ctx.Err(), true)
		}
		e := &courier.Error{Kind: courier.KindUnavailable, Provider: c.provider, Op: r.Op, Retryable: true, StatusCode: resp.StatusCode, Message: "failed reading response", Err: scrub(err)}
		if mutation {
			e.Kind, e.Outcome, e.Retryable = courier.KindIndeterminate, courier.OutcomeIndeterminate, false
		}
		return nil, e
	}
	if int64(len(data)) > c.maxBody {
		return nil, &courier.Error{Kind: courier.KindResourceLimit, Provider: c.provider, Op: r.Op, StatusCode: resp.StatusCode,
			Message: fmt.Sprintf("response exceeds %d bytes", c.maxBody)}
	}
	out := &Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: data}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return out, nil
	}
	return out, c.classify(r, out, mutation)
}

// scrub removes the URL (which may carry query data) from net/url errors.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

func (c *Client) classify(r Request, resp *Response, mutation bool) error {
	e := &courier.Error{Provider: c.provider, Op: r.Op, StatusCode: resp.Status}
	if r.ErrorMessage != nil {
		code, msg := r.ErrorMessage(resp.Status, resp.Body)
		e.ProviderCode, e.Message = truncate(code, 64), truncate(msg, 300)
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.Status)
	}
	switch s := resp.Status; {
	case s >= 300 && s < 400:
		e.Kind, e.Message = courier.KindProtocol, "unexpected redirect (not followed)"
	case s == 401:
		e.Kind, e.Outcome = courier.KindCredentials, courier.OutcomeRejected
	case s == 403:
		e.Kind, e.Outcome = courier.KindPermission, courier.OutcomeRejected
	case s == 404:
		e.Kind, e.Outcome = courier.KindNotFound, courier.OutcomeRejected
	case s == 409:
		e.Kind, e.Outcome = courier.KindConflict, courier.OutcomeRejected
	case s == 429:
		e.Kind, e.Retryable, e.Outcome = courier.KindRateLimit, true, courier.OutcomeRejected
		e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	case s >= 500:
		if mutation {
			e.Kind, e.Outcome = courier.KindIndeterminate, courier.OutcomeIndeterminate
			e.Message = strings.TrimSpace(e.Message + "; the operation may have been applied")
		} else {
			e.Kind, e.Retryable = courier.KindUnavailable, true
		}
	default: // other 4xx
		e.Kind, e.Outcome = courier.KindRejected, courier.OutcomeRejected
		if s == 400 || s == 422 {
			e.Kind = courier.KindValidation
		}
	}
	return e
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// DecodeJSON strictly decodes a response body into out using json.Number for
// numerics (no float coercion) and rejects trailing data. Failures are
// ErrProtocol (ERR-05).
func DecodeJSON(provider courier.ProviderID, op string, body []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return &courier.Error{Kind: courier.KindProtocol, Provider: provider, Op: op, Message: "malformed JSON response", Err: err}
	}
	if dec.More() {
		return &courier.Error{Kind: courier.KindProtocol, Provider: provider, Op: op, Message: "unexpected trailing data in response"}
	}
	return nil
}

// JSONMessage extracts common {"message"|"error"|"error_description"|"code"}
// fields from an error body; suitable as Request.ErrorMessage.
func JSONMessage(_ int, body []byte) (code, message string) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return "", ""
	}
	str := func(k string) string {
		var s string
		if json.Unmarshal(m[k], &s) == nil {
			return s
		}
		return ""
	}
	for _, k := range []string{"message", "error_description", "error", "detail"} {
		if s := str(k); s != "" {
			message = s
			break
		}
	}
	for _, k := range []string{"code", "error_code", "error"} {
		if s := str(k); s != "" && s != message {
			code = s
			break
		}
	}
	return code, message
}
