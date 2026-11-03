package httpx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

func newClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStatusClassification(t *testing.T) {
	cases := []struct {
		status   int
		mutation bool
		want     *courier.Error
		outcome  courier.Outcome
	}{
		{401, true, courier.ErrCredentials, courier.OutcomeRejected},
		{403, false, courier.ErrPermission, courier.OutcomeRejected},
		{404, false, courier.ErrNotFound, courier.OutcomeRejected},
		{409, true, courier.ErrConflict, courier.OutcomeRejected},
		{422, true, courier.ErrValidation, courier.OutcomeRejected},
		{429, false, courier.ErrRateLimit, courier.OutcomeRejected},
		{500, true, courier.ErrIndeterminate, courier.OutcomeIndeterminate},
		{503, false, courier.ErrUnavailable, ""},
		{302, false, courier.ErrProtocol, ""},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://evil.example/")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"message":"nope","code":"E1"}`))
		}))
		c := newClient(t, Config{Provider: "p"})
		method := http.MethodGet
		if tc.mutation {
			method = http.MethodPost
		}
		resp, err := c.Do(context.Background(), Request{Op: "t", Method: method, URL: srv.URL, Mutation: tc.mutation, ErrorMessage: JSONMessage})
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: got %v", tc.status, err)
			continue
		}
		var ce *courier.Error
		errors.As(err, &ce)
		if ce.Outcome != tc.outcome {
			t.Errorf("status %d outcome %q", tc.status, ce.Outcome)
		}
		if tc.status != 302 && (!strings.HasPrefix(ce.Message, "nope") || ce.ProviderCode != "E1") {
			t.Errorf("provider message lost: %+v", ce)
		}
		if resp == nil {
			t.Errorf("status %d: response should accompany error", tc.status)
		}
	}
}

func TestRedirectsAreNeverFollowedAndAuthNeverForwarded(t *testing.T) {
	var hits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusFound)
	}))
	defer srv.Close()
	c := newClient(t, Config{})
	_, err := c.Do(context.Background(), Request{URL: srv.URL, Auth: http.Header{"Authorization": {"Bearer secret"}}})
	if !errors.Is(err, courier.ErrProtocol) || hits.Load() != 0 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestProtectedHeadersAndAuthWins(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer srv.Close()
	c := newClient(t, Config{UserAgent: "ua/1"})
	if _, err := c.Do(context.Background(), Request{URL: srv.URL, Header: http.Header{"Authorization": {"x"}}}); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("want validation, got %v", err)
	}
	if _, err := c.Do(context.Background(), Request{URL: srv.URL, Header: http.Header{"X-Extra": {"1"}}, Auth: http.Header{"Authorization": {"Bearer t"}}}); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer t" || got.Get("User-Agent") != "ua/1" || got.Get("X-Extra") != "1" || got.Get("Accept") != "application/json" {
		t.Fatalf("headers: %v", got)
	}
}

func TestRetryOnlyWhenReplaySafe(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	c := newClient(t, Config{Retry: RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}})

	_, _ = c.Do(context.Background(), Request{Method: "GET", URL: srv.URL})
	if n.Load() != 3 {
		t.Fatalf("GET attempts = %d", n.Load())
	}
	n.Store(0)
	_, err := c.Do(context.Background(), Request{Method: "POST", URL: srv.URL, Mutation: true, Body: []byte(`{}`)})
	if n.Load() != 1 || !courier.IsIndeterminate(err) {
		t.Fatalf("mutation attempts=%d err=%v", n.Load(), err)
	}
	n.Store(0)
	// POST lookup is NOT automatically replay-safe (TRK-13)
	_, _ = c.Do(context.Background(), Request{Method: "POST", URL: srv.URL, Body: []byte(`{}`)})
	if n.Load() != 1 {
		t.Fatalf("POST lookup retried %d times without ReplaySafe", n.Load())
	}
	n.Store(0)
	_, _ = c.Do(context.Background(), Request{Method: "POST", URL: srv.URL, ReplaySafe: true, Body: []byte(`{}`)})
	if n.Load() != 3 {
		t.Fatalf("explicit replay-safe POST attempts = %d", n.Load())
	}
}

func TestCancellationOverridesRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	c := newClient(t, Config{Retry: RetryPolicy{MaxAttempts: 5, BaseDelay: time.Hour, MaxDelay: time.Hour}})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Do(ctx, Request{URL: srv.URL})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("did not stop promptly: %v", err)
	}
}

func TestMutationTimeoutIsIndeterminate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := newClient(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Do(ctx, Request{Method: "POST", URL: srv.URL, Mutation: true, Body: []byte(`{}`)})
	if !courier.IsIndeterminate(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestResponseSizeBound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 100)))
	}))
	defer srv.Close()
	c := newClient(t, Config{MaxResponseBytes: 50})
	if _, err := c.Do(context.Background(), Request{URL: srv.URL}); !errors.Is(err, courier.ErrResourceLimit) {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrencyBound(t *testing.T) {
	var cur, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := cur.Add(1)
		for {
			p := peak.Load()
			if v <= p || peak.CompareAndSwap(p, v) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
	}))
	defer srv.Close()
	c := newClient(t, Config{MaxConcurrent: 2})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() { _, _ = c.Do(context.Background(), Request{URL: srv.URL}); done <- struct{}{} }()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if peak.Load() > 2 {
		t.Fatalf("peak concurrency %d", peak.Load())
	}
}

func TestURLPolicy(t *testing.T) {
	c := newClient(t, Config{})
	for _, u := range []string{"http://example.com/x", "ftp://example.com", "https://user:pw@example.com/", "/relative"} {
		if _, err := c.Do(context.Background(), Request{URL: u}); !errors.Is(err, courier.ErrConfiguration) {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestDecodeJSONStrict(t *testing.T) {
	var v struct{ A any }
	if err := DecodeJSON("p", "op", []byte(`{"A":1.10}`), &v); err != nil || v.A.(interface{ String() string }).String() != "1.10" {
		t.Fatalf("%v %v", v, err)
	}
	for _, bad := range []string{`{`, `{} {}`, ``} {
		if err := DecodeJSON("p", "op", []byte(bad), &v); !errors.Is(err, courier.ErrProtocol) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

type captureHandler struct{ recs *[]slog.Record }

func (captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	*h.recs = append(*h.recs, r)
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

func TestDebugLogShowsBodiesButNeverCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"consignment":{"id":7},"access_token":"resp-secret"}`))
	}))
	defer srv.Close()
	var recs []slog.Record
	c, err := New(Config{Provider: "x", Logger: slog.New(captureHandler{&recs}), AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(context.Background(), Request{Op: "t", Method: http.MethodPost, URL: srv.URL + "/a?phone=01711", Mutation: true,
		Auth: http.Header{"Api-Key": {"key-secret"}}, Body: []byte(`{"name":"Rahim","client_secret":"body-secret"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, r := range recs {
		r.Attrs(func(a slog.Attr) bool { all.WriteString(a.Key + "=" + a.Value.String() + "\n"); return true })
	}
	out := all.String()
	for _, want := range []string{"Rahim", `"id":7`, "request_body", "response_body"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug record lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"key-secret", "body-secret", "resp-secret", "01711"} {
		if strings.Contains(out, secret) {
			t.Errorf("debug record leaks %q:\n%s", secret, out)
		}
	}
}
