package uber

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/contracttest"
)

const (
	quoteJSON    = `{"kind":"delivery_quote","id":"dqt_1","created":"2026-11-03T10:00:00Z","expires":"2026-11-03T10:15:00Z","fee":1299,"currency":"usd","currency_type":"USD","dropoff_eta":"2026-11-03T10:45:00Z","duration":35}`
	deliveryJSON = `{"kind":"delivery","id":"del_1","status":"pending","fee":1299,"currency":"usd","currency_type":"USD","external_id":"ORD-1","created":"2026-11-03T10:01:00Z","tracking_url":"https://example.invalid/t/1"}`
)

func TestParseTimeDefaultsToAsiaDhaka(t *testing.T) {
	got := parseTime("2026-11-03 10:00:00")
	if got == nil || got.Location().String() != "Asia/Dhaka" || got.Hour() != 10 {
		t.Fatalf("got %v", got)
	}
}

func happyAPI(t *testing.T, seen *[]string) func(w http.ResponseWriter, r *http.Request, body string) {
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request, body string) {
		mu.Lock()
		*seen = append(*seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-cid-1-") {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.URL.Path == "/v1/customers/cust-1/delivery_quotes":
			_, _ = w.Write([]byte(quoteJSON))
		case r.URL.Path == "/v1/customers/cust-1/deliveries" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(deliveryJSON))
		case r.URL.Path == "/v1/customers/cust-1/deliveries/del_1" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(deliveryJSON))
		case r.URL.Path == "/v1/customers/cust-1/deliveries/del_1/cancel":
			_, _ = w.Write([]byte(`{"kind":"delivery","id":"del_1","status":"canceled"}`))
		default:
			http.NotFound(w, r)
		}
	}
}

func TestQuoteCreateGetCancelHappyPath(t *testing.T) {
	var seen []string
	var createBody string
	api := happyAPI(t, &seen)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if r.URL.Path == "/v1/customers/cust-1/deliveries" && r.Method == http.MethodPost {
			createBody = body
		}
		api(w, r, body)
	})
	c := f.global(t)
	ctx := context.Background()

	q, err := c.Quote(ctx, courier.QuoteRequest{Pickup: structuredParty("S", ""), Recipient: structuredParty("A", "")})
	if err != nil {
		t.Fatal(err)
	}
	if q.ID != "dqt_1" || q.Fee.Amount.String() != "12.99" || q.Fee.Currency != "USD" || q.ExpiresAt == nil || q.Ref.Account != "cust-1" {
		t.Fatalf("%+v", q)
	}
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "/delivery_quotes") {
		t.Fatalf("Quote must not book: %v", seen)
	}

	sh, err := c.CreateShipment(ctx, createReq())
	if err != nil {
		t.Fatal(err)
	}
	if sh.Ref.ID != "del_1" || sh.Ref.Account != "cust-1" || sh.Status != courier.StatusCreated || sh.MerchantOrderID != "ORD-1" ||
		sh.DeliveryCharge == nil || sh.DeliveryCharge.Amount.String() != "12.99" || sh.CreatedAt == nil || sh.UpdatedAt != nil {
		t.Fatalf("%+v", sh)
	}
	m := mustJSON(t, createBody)
	if _, has := m["quote_id"]; has {
		t.Fatal("empty QuoteID must not send quote_id or fetch a quote")
	}
	if m["pickup_phone_number"] != "+14155550100" || m["dropoff_phone_number"] != "+14155550123" || m["external_id"] != "ORD-1" {
		t.Fatalf("body %s", createBody)
	}
	addr := mustJSON(t, m["pickup_address"].(string))
	if addr["city"] != "New York" || addr["country"] != "US" || addr["zip_code"] != "10023" {
		t.Fatalf("addr %v", addr)
	}
	items := m["manifest_items"].([]any)[0].(map[string]any)
	if items["name"] != "Pizza" || items["quantity"].(float64) != 2 {
		t.Fatalf("items %v", items)
	}

	r := createReq()
	r.QuoteID = "dqt_1"
	if _, err := c.CreateShipment(ctx, r); err != nil || mustJSON(t, createBody)["quote_id"] != "dqt_1" {
		t.Fatalf("quote id forwarding: %v %s", err, createBody)
	}

	got, err := c.GetShipment(ctx, sh.Ref)
	if err != nil || got.Ref != sh.Ref {
		t.Fatalf("%v %+v", err, got)
	}
	if err := c.CancelShipment(ctx, sh.Ref); err != nil {
		t.Fatal(err)
	}
	if f.tokenHits.Load() != 1 {
		t.Fatalf("token fetched %d times", f.tokenHits.Load())
	}
	if form := f.lastForm; form["grant_type"] != "client_credentials" || form["scope"] != "eats.deliveries" || form["client_id"] != "cid-1" {
		t.Fatalf("form %v", form)
	}
}

func TestFeeConversionExactDecimals(t *testing.T) {
	cases := []struct{ fee, cur, want string }{
		{"1299", "USD", "12.99"}, {"1500", "usd", "15"}, {"500", "JPY", "5"}, {"1234", "KWD", "12.34"},
		{"5", "EUR", "0.05"}, {"99999999999999999", "USD", "999999999999999.99"}, {`"250"`, "VND", "2.5"},
	}
	for _, tc := range cases {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
			_, _ = fmt.Fprintf(w, `{"id":"dqt_1","fee":%s,"currency_type":%q}`, tc.fee, tc.cur)
		})
		q, err := f.global(t).Quote(context.Background(), courier.QuoteRequest{Pickup: structuredParty("S", ""), Recipient: structuredParty("A", "")})
		if err != nil {
			t.Fatalf("%v: %v", tc, err)
		}
		if q.Fee.Amount.String() != tc.want || q.Fee.Currency != courier.Currency(strings.ToUpper(tc.cur)) {
			t.Errorf("%v: got %s", tc, q.Fee)
		}
	}
}

func TestMissingCurrencyOrBadFeeIsProtocolError(t *testing.T) {
	for name, body := range map[string]string{
		"no currency":   `{"id":"dqt_1","fee":1299}`,
		"no fee":        `{"id":"dqt_1","currency_type":"USD"}`,
		"fractional":    `{"id":"dqt_1","fee":12.5,"currency_type":"USD"}`,
		"negative":      `{"id":"dqt_1","fee":-1,"currency_type":"USD"}`,
		"conflict":      `{"id":"dqt_1","fee":1,"currency":"EUR","currency_type":"USD"}`,
		"no id":         `{"fee":1,"currency_type":"USD"}`,
		"malformed":     `{"id":`,
		"trailing data": `{"id":"dqt_1","fee":1,"currency_type":"USD"} {}`,
	} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(body)) })
		_, err := f.global(t).Quote(context.Background(), courier.QuoteRequest{Pickup: structuredParty("S", ""), Recipient: structuredParty("A", "")})
		if !errors.Is(err, courier.ErrProtocol) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestMalformedCreateAndCancelAreIndeterminate(t *testing.T) {
	for name, body := range map[string]string{"json": `{"id":`, "no id": `{"status":"pending"}`, "bad fee": `{"id":"del_1","fee":1}`} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(body)) })
		_, err := f.global(t).CreateShipment(context.Background(), createReq())
		if !courier.IsIndeterminate(err) {
			t.Errorf("create %s: got %v", name, err)
		}
	}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"id":"del_1","status":"pending"}`))
	})
	if err := f.global(t).CancelShipment(context.Background(), courier.ShipmentRef{ID: "del_1"}); !courier.IsIndeterminate(err) {
		t.Fatalf("cancel not confirmed: %v", err)
	}
}

func TestEmbeddedFailureInHTTP200(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"kind":"error","code":"invalid_params","message":"pickup 100 Maiden Ln is bad"}`))
	})
	_, err := f.global(t).CreateShipment(context.Background(), createReq())
	if !errors.Is(err, courier.ErrValidation) || courier.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "Maiden") {
		t.Fatal("provider message leaked into error")
	}
	f2 := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(`{"code":"not_found"}`)) })
	if _, err := f2.global(t).GetShipment(context.Background(), courier.ShipmentRef{ID: "del_9"}); !errors.Is(err, courier.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIndeterminateOnServerErrorAndTimeoutNoAutoRetry(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(status) })
		c := f.global(t, fastRetry())
		if _, err := c.CreateShipment(context.Background(), createReq()); !courier.IsIndeterminate(err) {
			t.Fatalf("create %d: %v", status, err)
		}
		if err := c.CancelShipment(context.Background(), courier.ShipmentRef{ID: "del_1"}); !courier.IsIndeterminate(err) {
			t.Fatalf("cancel %d: %v", status, err)
		}
		if got := f.apiHits.Load(); got != 2 {
			t.Fatalf("mutations were retried: %d api hits", got)
		}
	}
	// Timeout.
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { time.Sleep(300 * time.Millisecond) })
	c := f.global(t, WithHTTPClient(&http.Client{Timeout: 100 * time.Millisecond}))
	// Prime the token so only the API call times out.
	if _, _, err := c.token(context.Background(), creds); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateShipment(context.Background(), createReq()); !courier.IsIndeterminate(err) {
		t.Fatalf("create timeout: %v", err)
	}
	if err := c.CancelShipment(context.Background(), courier.ShipmentRef{ID: "del_1"}); !courier.IsIndeterminate(err) {
		t.Fatalf("cancel timeout: %v", err)
	}
}

func TestReadsRetryOn5xx(t *testing.T) {
	var n atomic.Int32
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if n.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(deliveryJSON))
	})
	if _, err := f.global(t, fastRetry()).GetShipment(context.Background(), courier.ShipmentRef{ID: "del_1"}); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalTenantExclusivityZeroOutboundCalls(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(deliveryJSON)) })
	ref := courier.ShipmentRef{ID: "del_1"}

	g := f.global(t)
	_, err := g.GetShipment(ContextWithCredentials(context.Background(), creds), ref)
	if !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("global with ctx creds: %v", err)
	}
	tn := f.tenant(t)
	if _, err := tn.GetShipment(context.Background(), ref); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("tenant without ctx creds: %v", err)
	}
	if _, err := tn.Quote(context.Background(), courier.QuoteRequest{}); err == nil {
		t.Fatal("want error")
	}
	if f.tokenHits.Load() != 0 || f.apiHits.Load() != 0 {
		t.Fatalf("outbound calls on violation: token=%d api=%d", f.tokenHits.Load(), f.apiHits.Load())
	}
	if _, err := NewTenant(f.opts(WithCredentials(creds))...); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("tenant must reject WithCredentials: %v", err)
	}
	// Tenant works with ctx creds; global env must never leak into tenant.
	t.Setenv("UBER_DIRECT_CLIENT_ID", "cid-env")
	t.Setenv("UBER_DIRECT_CLIENT_SECRET", "sec-env")
	t.Setenv("UBER_DIRECT_CUSTOMER_ID", "cust-env")
	if _, err := tn.GetShipment(context.Background(), ref); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("tenant must not use env: %v", err)
	}
	if _, err := tn.GetShipment(ContextWithCredentials(context.Background(), creds), ref); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalConstructionRequiresCredentialsAndReadsEnv(t *testing.T) {
	for _, k := range []string{"UBER_DIRECT_CLIENT_ID", "UBER_DIRECT_CLIENT_SECRET", "UBER_DIRECT_CUSTOMER_ID"} {
		t.Setenv(k, "")
	}
	if _, err := New(); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("got %v", err)
	}
	if _, err := New(WithCredentials(Credentials{ClientID: "a", ClientSecret: "b"})); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("customer id is required: %v", err)
	}
	if _, err := New(WithCredentials(Credentials{ClientID: "a", ClientSecret: "b", CustomerID: "x y"})); err == nil {
		t.Fatal("whitespace in customer id")
	}
	t.Setenv("UBER_DIRECT_CLIENT_ID", "cid-1")
	t.Setenv("UBER_DIRECT_CLIENT_SECRET", "sec-1")
	t.Setenv("UBER_DIRECT_CUSTOMER_ID", "cust-1")
	c, err := New()
	if err != nil || c.Mode() != auth.ModeGlobal {
		t.Fatalf("%v", err)
	}
}

func TestCredentialsAreSnapshotAndRedacted(t *testing.T) {
	if s := fmt.Sprintf("%v %+v %#v", creds, creds, creds); strings.Contains(s, "sec-1") || strings.Contains(s, "cid-1") {
		t.Fatalf("credentials print secrets: %s", s)
	}
}

func TestTokenCacheReuseAndSingleReauthOn401(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(deliveryJSON)) })
	c := f.global(t)
	for i := 0; i < 5; i++ {
		if _, err := c.GetShipment(context.Background(), courier.ShipmentRef{ID: "del_1"}); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokenHits.Load() != 1 {
		t.Fatalf("token reuse: %d fetches", f.tokenHits.Load())
	}
	f.reject401.Store(1) // next API call 401 -> re-issue once
	if _, err := c.GetShipment(context.Background(), courier.ShipmentRef{ID: "del_1"}); err != nil {
		t.Fatal(err)
	}
	if f.tokenHits.Load() != 2 {
		t.Fatalf("re-auth: %d fetches", f.tokenHits.Load())
	}
	// Mutation is retried exactly once after 401 (request was not processed).
	f.reject401.Store(1)
	before := f.apiHits.Load()
	if _, err := c.CreateShipment(context.Background(), createReq()); err != nil {
		t.Fatal(err)
	}
	if f.apiHits.Load()-before != 2 || f.tokenHits.Load() != 3 {
		t.Fatalf("api=%d token=%d", f.apiHits.Load()-before, f.tokenHits.Load())
	}
	// Persistent 401: one re-issue, then a credentials error, no loop.
	f.reject401.Store(100)
	tokBefore, apiBefore := f.tokenHits.Load(), f.apiHits.Load()
	_, err := c.GetShipment(context.Background(), courier.ShipmentRef{ID: "del_1"})
	if !errors.Is(err, courier.ErrCredentials) {
		t.Fatalf("got %v", err)
	}
	if f.tokenHits.Load()-tokBefore != 1 || f.apiHits.Load()-apiBefore != 2 {
		t.Fatalf("token=%d api=%d", f.tokenHits.Load()-tokBefore, f.apiHits.Load()-apiBefore)
	}
}

func TestTokenRejectionIsCredentialsErrorAndNotRetried(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {})
	bad := Credentials{ClientID: "cid-1", ClientSecret: "WRONG", CustomerID: "cust-1"}
	c, _ := New(f.opts(WithCredentials(bad), fastRetry())...)
	_, err := c.Quote(context.Background(), courier.QuoteRequest{Pickup: structuredParty("S", ""), Recipient: structuredParty("A", "")})
	if !errors.Is(err, courier.ErrCredentials) {
		t.Fatalf("got %v", err)
	}
	if f.tokenHits.Load() != 1 || f.apiHits.Load() != 0 {
		t.Fatalf("token=%d api=%d", f.tokenHits.Load(), f.apiHits.Load())
	}
}

func TestConcurrentMultiTenantIsolation(t *testing.T) {
	var f *fake
	f = newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		// path customer must match the client that owns the bearer token
		parts := strings.Split(r.URL.Path, "/")
		cust := parts[3] // /v1/customers/{cust}/...
		want := "Bearer tok-cid-" + strings.TrimPrefix(cust, "cust-") + "-"
		if !strings.HasPrefix(r.Header.Get("Authorization"), want) {
			f.violate("cust %s got auth for another tenant", cust)
		}
		fmt.Fprintf(w, `{"id":"del_1","status":"pending","external_id":%q}`, cust)
	})
	c := f.tenant(t)
	const tenants, per = 8, 12
	var wg sync.WaitGroup
	for i := 0; i < tenants; i++ {
		cr := Credentials{ClientID: auth.Secret(fmt.Sprintf("cid-%d", i)), ClientSecret: auth.Secret(fmt.Sprintf("sec-%d", i)), CustomerID: fmt.Sprintf("cust-%d", i)}
		for j := 0; j < per; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sh, err := c.GetShipment(ContextWithCredentials(context.Background(), cr), courier.ShipmentRef{ID: "del_1"})
				if err != nil {
					t.Error(err)
					return
				}
				if sh.Ref.Account != courier.AccountID(cr.CustomerID) || string(sh.MerchantOrderID) != cr.CustomerID {
					t.Errorf("cross-tenant result: %+v", sh.Ref)
				}
			}()
		}
	}
	wg.Wait()
	if len(f.violations) > 0 {
		t.Fatal(f.violations)
	}
	if got := f.tokenHits.Load(); got != tenants {
		t.Fatalf("want one token per tenant (%d), got %d", tenants, got)
	}
}

func TestMutatedCredentialsAfterWithCredentialsDoNotAffectClient(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(deliveryJSON)) })
	cr := creds
	c := f.global(t, WithCredentials(cr))
	cr.CustomerID = "other"
	if _, err := c.GetShipment(context.Background(), courier.ShipmentRef{ID: "del_1"}); err != nil {
		t.Fatal(err)
	}
}

func TestRefAccountMismatchRejectedBeforeHTTP(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(deliveryJSON)) })
	c := f.global(t)
	for _, ref := range []courier.ShipmentRef{
		{Account: "someone-else", ID: "del_1"}, {Provider: "steadfast", ID: "del_1"}, {ID: ""}, {ID: "../x"}, {ID: "a/b"},
	} {
		if _, err := c.GetShipment(context.Background(), ref); !errors.Is(err, courier.ErrValidation) {
			t.Errorf("%+v: %v", ref, err)
		}
		if err := c.CancelShipment(context.Background(), ref); !errors.Is(err, courier.ErrValidation) {
			t.Errorf("cancel %+v: %v", ref, err)
		}
	}
	if f.apiHits.Load() != 0 || f.tokenHits.Load() != 0 {
		t.Fatal("outbound call for invalid ref")
	}
}

func TestValidationBeforeAnyHTTP(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {})
	c := f.global(t)
	mod := func(fn func(*courier.CreateShipmentRequest)) courier.CreateShipmentRequest {
		r := createReq()
		fn(&r)
		return r
	}
	cod, _ := courier.ParseMoney("10", "USD")
	bad := map[string]courier.CreateShipmentRequest{
		"cod":          mod(func(r *courier.CreateShipmentRequest) { r.COD = &cod }),
		"charge":       mod(func(r *courier.CreateShipmentRequest) { r.DeliveryCharge = &cod }),
		"phone":        mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Contact.Phone = "4155550123" }),
		"no name":      mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Contact.Name = "" }),
		"no desc":      mod(func(r *courier.CreateShipmentRequest) { r.Package.Description = "" }),
		"point":        mod(func(r *courier.CreateShipmentRequest) { r.Delivery = courier.DeliveryPoint }),
		"no address":   mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Address = courier.Address{} }),
		"text only":    mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Address = courier.Address{Text: "somewhere"} }),
		"partial addr": mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Address = courier.Address{Line1: "x"} }),
		"neg qty":      mod(func(r *courier.CreateShipmentRequest) { r.Package.Quantity = -1 }),
	}
	for name, r := range bad {
		if _, err := c.CreateShipment(context.Background(), r); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := c.Quote(context.Background(), courier.QuoteRequest{COD: &cod, Pickup: structuredParty("S", ""), Recipient: structuredParty("A", "")}); err == nil {
		t.Error("quote cod")
	}
	if f.apiHits.Load() != 0 || f.tokenHits.Load() != 0 {
		t.Fatalf("HTTP for invalid input: api=%d token=%d", f.apiHits.Load(), f.tokenHits.Load())
	}
}

func TestAddressIsJSONStringPathEscapingAndIdempotencyKey(t *testing.T) {
	var path, rawBody string
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body string) {
		path, rawBody = r.URL.EscapedPath(), body
		_, _ = w.Write([]byte(deliveryJSON))
	})
	cr := Credentials{ClientID: "cid-1", ClientSecret: "sec-1", CustomerID: "a/b?c"}
	c, _ := New(f.opts(WithCredentials(cr))...)
	r := createReq()
	r.IdempotencyKey = "key-123"
	if _, err := c.CreateShipment(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/customers/a%2Fb%3Fc/deliveries" {
		t.Fatalf("path %s", path)
	}
	body := mustJSON(t, rawBody)
	addr, _ := body["dropoff_address"].(string)
	var parsed map[string]any // the spec defines this field as a JSON string
	if err := json.Unmarshal([]byte(addr), &parsed); err != nil || parsed["city"] == nil || parsed["country"] == nil {
		t.Fatalf("dropoff_address must be a JSON string: %q", addr)
	}
	if body["idempotency_key"] != "key-123" {
		t.Fatalf("idempotency key not forwarded: %s", rawBody)
	}
}

func TestTrackingEventsOnlyFromRealWaypointOutcomes(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"kind":"delivery","id":"del_abc","status":"delivered","fee":500,"currency_type":"USD",
			"pickup":{"status":"completed","status_timestamp":"2026-03-01T10:00:00Z"},
			"dropoff":{"status":"completed","status_timestamp":"2026-03-01T10:30:00Z"}}`))
	})
	res, err := f.global(t).TrackShipment(context.Background(), courier.TrackingRequest{Code: "del_abc"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Coverage != courier.CoveragePartial || len(res.Events) != 2 || res.Events[0].Status != courier.StatusPickedUp ||
		res.Events[1].Status != courier.StatusDelivered || res.Delivery == nil || res.Delivery.DeliveredAt == nil ||
		res.Delivery.DeliveredAt.Format(time.RFC3339) != "2026-03-01T10:30:00Z" {
		t.Fatalf("%+v", res)
	}
	// no waypoint data -> current-only, no events, no DeliveredAt
	f2 := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"id":"del_abc","status":"pickup"}`))
	})
	res, err = f2.global(t).TrackShipment(context.Background(), courier.TrackingRequest{Code: "del_abc"})
	if err != nil || res.Coverage != courier.CoverageCurrentOnly || len(res.Events) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// failed dropoff without a timestamp must not invent an event
	f3 := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"id":"del_abc","status":"returned","dropoff":{"status":"failed"}}`))
	})
	res, _ = f3.global(t).TrackShipment(context.Background(), courier.TrackingRequest{Code: "del_abc"})
	if res == nil || len(res.Events) != 0 || res.Status != courier.StatusReturned {
		t.Fatalf("%+v", res)
	}
}

func TestListDeliveriesQueryAndStrictDecoding(t *testing.T) {
	var q url.Values
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		q = r.URL.Query()
		_, _ = w.Write([]byte(`{"data":[{"id":"del_1","status":"pending","fee":100,"currency_type":"USD"},{"id":"del_2","status":"delivered"}],"next_href":"api.uber.com/v1/x?offset=30","total_count":-1}`))
	})
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	page, err := f.global(t).ListDeliveries(context.Background(), DeliveryFilter{State: "pending", ExternalStoreID: "s1", Start: &start, Limit: 30, Offset: 30})
	if err != nil || len(page.Shipments) != 2 || page.NextHref == "" || page.Shipments[1].DeliveryCharge != nil {
		t.Fatalf("%+v %v", page, err)
	}
	if q.Get("filter") != "pending" || q.Get("Offset") != "30" || q.Get("limit") != "30" || q.Get("start_dt") != "2026-01-02T03:04:05Z" || q.Get("external_store_id") != "s1" {
		t.Fatalf("query %v", q)
	}
	if _, err := f.global(t).ListDeliveries(context.Background(), DeliveryFilter{State: "bogus"}); err == nil {
		t.Fatal("unknown state")
	}
	f2 := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"data":[{"status":"pending"}]}`))
	})
	if _, err := f2.global(t).ListDeliveries(context.Background(), DeliveryFilter{}); !errors.Is(err, courier.ErrProtocol) {
		t.Fatalf("malformed record must not be skipped: %v", err)
	}
}

func TestProofOfDeliveryDocument(t *testing.T) {
	var body string
	var method string
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b string) {
		body, method = b, r.Method
		_, _ = w.Write([]byte(`{"document":"` + base64.StdEncoding.EncodeToString([]byte("\x89PNG-bytes")) + `"}`))
	})
	c := f.global(t)
	doc, err := c.GetProofOfDeliveryDocument(context.Background(), courier.ShipmentRef{ID: "del_abc"}, ProofDropoff, ProofSignature)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Body.Close()
	got, _ := io.ReadAll(doc.Body)
	if method != "POST" || string(got) != "\x89PNG-bytes" || doc.ContentType != "image/png" || mustJSON(t, body)["waypoint"] != "dropoff" || mustJSON(t, body)["type"] != "signature" {
		t.Fatalf("%s %q %s", method, got, body)
	}
	hits := f.apiHits.Load()
	for _, bad := range []func() error{
		func() error {
			_, e := c.GetProofOfDeliveryDocument(context.Background(), courier.ShipmentRef{ID: "del_abc"}, "x", ProofPicture)
			return e
		},
		func() error {
			_, e := c.GetProofOfDeliveryDocument(context.Background(), courier.ShipmentRef{ID: "del_abc"}, ProofPickup, "y")
			return e
		},
		func() error {
			_, e := c.GetProofOfDeliveryDocument(context.Background(), courier.ShipmentRef{ID: "../x"}, ProofPickup, ProofPicture)
			return e
		},
	} {
		if err := bad(); !errors.Is(err, courier.ErrValidation) {
			t.Errorf("want validation: %v", err)
		}
	}
	if f.apiHits.Load() != hits {
		t.Fatal("invalid POD input reached the network")
	}
	f2 := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"document":"!!notbase64"}`))
	})
	if _, err := f2.global(t).GetProofOfDeliveryDocument(context.Background(), courier.ShipmentRef{ID: "del_abc"}, ProofPickup, ProofPicture); !errors.Is(err, courier.ErrProtocol) {
		t.Fatalf("%v", err)
	}
}

func TestRedirectDoesNotForwardAuthorization(t *testing.T) {
	var evilHits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { evilHits.Add(1) }))
	defer evil.Close()
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	})
	_, err := f.global(t).GetShipment(context.Background(), courier.ShipmentRef{ID: "del_1"})
	if !errors.Is(err, courier.ErrProtocol) {
		t.Fatalf("got %v", err)
	}
	if evilHits.Load() != 0 {
		t.Fatal("redirect was followed")
	}
	// Token endpoint redirect: no follow either.
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusTemporaryRedirect)
	})
	s := httptest.NewServer(mux)
	defer s.Close()
	c, _ := New(WithCredentials(creds), WithBaseURL(s.URL+"/v1"), WithAuthURL(s.URL+"/oauth/v2/token"))
	if _, err := c.GetShipment(context.Background(), courier.ShipmentRef{ID: "del_1"}); err == nil || evilHits.Load() != 0 {
		t.Fatalf("token redirect: %v hits=%d", err, evilHits.Load())
	}
}

func TestTrackShipmentMappingAndLookup(t *testing.T) {
	status := "delivered"
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		fmt.Fprintf(w, `{"id":"del_1","status":%q}`, status)
	})
	c := f.global(t)
	res, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Lookups: []courier.Lookup{{Type: courier.LookupProviderShipmentID, Value: "del_1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != courier.StatusDelivered || res.Delivery == nil || res.Delivery.DeliveredAt != nil || res.Coverage != courier.CoverageCurrentOnly || len(res.Events) != 0 {
		t.Fatalf("%+v", res)
	}
	for in, want := range map[string]courier.Status{"pending": courier.StatusCreated, "pickup": courier.StatusPickupPending, "pickup_complete": courier.StatusPickedUp,
		"dropoff": courier.StatusOutForDelivery, "canceled": courier.StatusCancelled, "returned": courier.StatusReturned, "weird": courier.StatusUnknown} {
		status = in
		r, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "del_1"})
		if err != nil || r.Status != want || r.ProviderStatus.Code != in || r.Delivery != nil {
			t.Errorf("%s: %v %+v", in, err, r)
		}
	}
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Lookups: []courier.Lookup{{Type: courier.LookupPhone, Value: "+1"}}}); !errors.Is(err, courier.ErrUnsupported) {
		t.Fatalf("phone lookup: %v", err)
	}
	status = ""
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "del_1"}); !errors.Is(err, courier.ErrProtocol) {
		t.Fatalf("missing status: %v", err)
	}
}

func TestTrackerConformance(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/del_ok"):
			_, _ = w.Write([]byte(`{"id":"del_ok","status":"delivered","tracking_url":"https://example.invalid"}`))
		case strings.HasSuffix(r.URL.Path, "/del_bad"):
			_, _ = w.Write([]byte(`{"id":`))
		default:
			http.NotFound(w, r)
		}
	})
	contracttest.RunShipmentTracker(t, contracttest.TrackerSuite{
		Tracker:          f.global(t),
		ForbidCreation:   false,
		MalformedRequest: &courier.TrackingRequest{Code: "del_bad"},
		Cases: []contracttest.TrackerCase{{
			Name:    "delivered",
			Request: courier.TrackingRequest{Code: "del_ok"},
			Check: func(t *testing.T, r *courier.TrackingResult) {
				if r.Status != courier.StatusDelivered || r.Delivery == nil || r.Ref.Account != "cust-1" {
					t.Fatalf("%+v", r)
				}
			},
		}},
	})
}

func TestCapabilitiesExactlyAsDeclared(t *testing.T) {
	var c any = &Client{}
	if _, ok := c.(courier.BatchShipmentCreator); ok {
		t.Error("must not implement BatchShipmentCreator")
	}
	if _, ok := c.(courier.CustomerIncidentReporter); ok {
		t.Error("must not implement CustomerIncidentReporter")
	}
	if _, ok := c.(courier.ProofOfDeliveryReader); ok {
		t.Error("must not implement ProofOfDeliveryReader")
	}
	if _, ok := c.(courier.BalanceReader); ok {
		t.Error("must not implement BalanceReader")
	}
	if _, ok := c.(courier.ShipmentLabelReader); ok {
		t.Error("must not implement ShipmentLabelReader")
	}
	if _, ok := c.(courier.ReturnRequester); ok {
		t.Error("must not implement ReturnRequester")
	}
	if _, ok := c.(courier.PickupRequester); ok {
		t.Error("must not implement PickupRequester")
	}
}

func TestManifest(t *testing.T) {
	m := Manifest()
	if m.Provider != ProviderID || m.VerifiedOn.Format("2006-01-02") != "2026-11-03" {
		t.Fatalf("%+v", m)
	}
	if m.Capabilities["proof_of_delivery"] != courier.VerifiedImplemented || m.Capabilities["webhook_delivery_status"] != courier.VerifiedImplemented {
		t.Fatal("manifest states")
	}
}

type recHandler struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (h *recHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool { h.buf.WriteString(" " + a.Key + "=" + a.Value.String()); return true })
	h.buf.WriteString("\n")
	return nil
}
func (h *recHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recHandler) WithGroup(string) slog.Handler      { return h }

func TestLogsContainNoCredentials(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(deliveryJSON)) })
	h := &recHandler{}
	c := f.global(t, WithLogger(slog.New(h)))
	_, _ = c.CreateShipment(context.Background(), createReq())
	_, _ = c.GetShipment(context.Background(), courier.ShipmentRef{ID: "del_1"})
	out := h.buf.String()
	for _, bad := range []string{"sec-1", "cid-1", "tok-"} {
		if strings.Contains(out, bad) {
			t.Fatalf("log leaks %q:\n%s", bad, out)
		}
	}
}
