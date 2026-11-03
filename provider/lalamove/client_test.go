package lalamove

// All fixtures below are derived from public docs (https://developers.lalamove.com/),
// not captured from production. No test contacts a real Lalamove host.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/contracttest"
	"github.com/nokibsarkar/go-courier/httpx"
)

var (
	testNow = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	creds   = Credentials{APIKey: "pk_test_abc", APISecret: "sk_test_secret", Market: "SG", AccountID: "acct-1"}
)

func fixedClock() time.Time { return testNow }

func fastRetry(n int) httpx.RetryPolicy {
	return httpx.RetryPolicy{MaxAttempts: n, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
}

// refSign independently recomputes the documented signature.
func refSign(secret, ts, method, path string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "\r\n" + method + "\r\n" + path + "\r\n\r\n"))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// checkAuth verifies the Authorization header against the exact received bytes
// and returns (apiKey, requestID).
func checkAuth(t *testing.T, r *http.Request, body []byte, secrets map[string]string) (string, string) {
	t.Helper()
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "hmac ") {
		t.Errorf("bad Authorization scheme")
		return "", ""
	}
	parts := strings.Split(strings.TrimPrefix(h, "hmac "), ":")
	if len(parts) != 3 {
		t.Errorf("bad Authorization format")
		return "", ""
	}
	secret, ok := secrets[parts[0]]
	if !ok {
		t.Errorf("unknown api key %q", parts[0])
		return "", ""
	}
	if want := refSign(secret, parts[1], r.Method, r.URL.Path, body); parts[2] != want {
		t.Errorf("signature mismatch for %s %s", r.Method, r.URL.Path)
	}
	if r.Header.Get("Request-ID") == "" {
		t.Errorf("missing Request-ID")
	}
	return parts[0], r.Header.Get("Request-ID")
}

const quoteJSON = `{"data":{"quotationId":"Q123","scheduleAt":"2026-11-03T10:00:00.000Z","expiresAt":"2026-11-03T10:05:00.000Z","serviceType":"MOTORCYCLE","language":"en_SG",` +
	`"stops":[{"stopId":"S1","coordinates":{"lat":"1.3521","lng":"103.8198"},"address":"Secret Street 9"},{"stopId":"S2","coordinates":{"lat":"1.29","lng":"103.85"},"address":"Hidden Road 4"}],` +
	`"priceBreakdown":{"base":"9.00","total":"1050.35","currency":"SGD"}}}`

func orderJSON(status string, extra string) string {
	return `{"data":{"orderId":"O1","quotationId":"Q123","driverId":"","shareLink":"https://share.example/o1","status":"` + status + `",` +
		`"priceBreakdown":{"total":"1050.35","currency":"SGD"},"stops":[{"stopId":"S1"},{"stopId":"S2"` + extra + `}]}}`
}

type fake struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	hits  map[string]int // "METHOD path"
	ids   []string
	bodys map[string][]byte
	h     func(w http.ResponseWriter, r *http.Request, body []byte) bool // return true if handled
	quote string
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte) bool) *fake {
	f := &fake{t: t, hits: map[string]int{}, bodys: map[string][]byte{}, h: h, quote: quoteJSON}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.hits[key]++
		f.ids = append(f.ids, r.Header.Get("Request-ID"))
		f.bodys[key] = body
		f.mu.Unlock()
		checkAuth(t, r, body, map[string]string{"pk_test_abc": "sk_test_secret"})
		if f.h != nil && f.h(w, r, body) {
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v3/quotations/Q123":
			_, _ = w.Write([]byte(f.quote))
		case r.Method == "POST" && r.URL.Path == "/v3/quotations":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(f.quote))
		case r.Method == "POST" && r.URL.Path == "/v3/orders":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(orderJSON("ASSIGNING_DRIVER", "")))
		case r.Method == "GET" && r.URL.Path == "/v3/orders/O1":
			_, _ = w.Write([]byte(orderJSON("PICKED_UP", "")))
		case r.Method == "DELETE" && r.URL.Path == "/v3/orders/O1":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) count(k string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.hits[k] }
func (f *fake) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.hits {
		n += v
	}
	return n
}

func (f *fake) global(opts ...Option) *Client {
	f.t.Helper()
	c, err := New(append([]Option{WithCredentials(creds), WithBaseURL(f.srv.URL + "/v3"), WithClock(fixedClock)}, opts...)...)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func createReq() courier.CreateShipmentRequest {
	return courier.CreateShipmentRequest{
		QuoteID:         "Q123",
		MerchantOrderID: "M-1",
		Pickup: courier.Party{
			Contact: courier.Contact{Name: "Sender", Phone: "+65 8123 4567"},
			Address: courier.Address{Text: "Secret Street 9", Coordinates: &courier.Coordinates{Lat: 1.3521, Lng: 103.8198}},
		},
		Recipient: courier.Party{
			Contact:      courier.Contact{Name: "Recipient", Phone: "0060123456789"},
			Address:      courier.Address{Text: "Hidden Road 4", Coordinates: &courier.Coordinates{Lat: 1.29, Lng: 103.85}},
			Instructions: "leave at door",
		},
	}
}

func makeQuoteReq() courier.QuoteRequest {
	r := createReq()
	return courier.QuoteRequest{Pickup: r.Pickup, Recipient: r.Recipient}
}

var qopts = QuoteOptions{ServiceType: "MOTORCYCLE", Language: "en_SG"}

// ---- signing ----

func TestSignatureHandComputedVector(t *testing.T) {
	// Vectors computed independently with `openssl dgst -sha256 -hmac` over the documented
	// raw string TIMESTAMP\r\nMETHOD\r\nPATH\r\n\r\nBODY.
	body := []byte(`{"data":{"serviceType":"MOTORCYCLE"}}`)
	if got := rawSignature("1700000000000", "POST", "/v3/quotations", body); got != "1700000000000\r\nPOST\r\n/v3/quotations\r\n\r\n"+string(body) {
		t.Fatalf("raw signature %q", got)
	}
	if got := sign("sk_test_example_secret", "1700000000000", "POST", "/v3/quotations", body); got != "6760e2d6d707523e42dab74de27774d02a5e4bffea67c794588dcc124b401507" {
		t.Fatalf("POST vector %s", got)
	}
	if got := sign("sk_test_example_secret", "1700000000000", "GET", "/v3/orders/123", nil); got != "4fd668dfe8061e111da9211fb092eea6658177bf4cf300ba7b5e66be94e18490" {
		t.Fatalf("GET vector %s", got)
	}
}

func TestSignatureCoversExactBodyBytesAndHeaders(t *testing.T) {
	f := newFake(t, nil)
	c := f.global()
	if _, err := c.QuoteWithOptions(context.Background(), makeQuoteReq(), qopts); err != nil {
		t.Fatal(err)
	}
	// Server-side checkAuth already verified the signature over the received bytes.
	body := f.bodys["POST /v3/quotations"]
	if !bytes.Contains(body, []byte(`"lat":"1.3521"`)) || !bytes.Contains(body, []byte(`"serviceType":"MOTORCYCLE"`)) {
		t.Fatalf("body %s", body)
	}
	// Timestamps come from the injected clock in Unix milliseconds.
	f2 := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if !strings.Contains(r.Header.Get("Authorization"), ":1793700000000:") {
			t.Errorf("timestamp not unix ms from clock: %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Market") != "SG" {
			t.Errorf("market header %q", r.Header.Get("Market"))
		}
		return false
	})
	if _, err := f2.global().GetQuote(context.Background(), "Q123"); err != nil {
		t.Fatal(err)
	}
	if testNow.UnixMilli() != 1793700000000 {
		t.Fatalf("test constant stale: %d", testNow.UnixMilli())
	}
}

func TestFreshRequestIDAndSignaturePerAttemptIncludingRetries(t *testing.T) {
	var n atomic.Int32
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.URL.Path == "/v3/orders/O1" && n.Add(1) < 3 {
			w.WriteHeader(503)
			return true
		}
		return false
	})
	c := f.global(WithRetry(fastRetry(4)))
	if _, err := c.GetShipment(context.Background(), courier.ShipmentRef{ID: "O1"}); err != nil {
		t.Fatal(err)
	}
	if f.count("GET /v3/orders/O1") != 3 {
		t.Fatalf("hits %d", f.count("GET /v3/orders/O1"))
	}
	seen := map[string]bool{}
	for _, id := range f.ids {
		if len(id) != 36 || id[14] != '4' || seen[id] {
			t.Fatalf("request id %q not a fresh uuid4 (ids=%v)", id, f.ids)
		}
		seen[id] = true
	}
}

// ---- modes ----

func TestGlobalTenantExclusivityZeroOutboundCalls(t *testing.T) {
	f := newFake(t, nil)
	g := f.global()
	ctx := ContextWithCredentials(context.Background(), creds)
	if _, err := g.GetShipment(ctx, courier.ShipmentRef{ID: "O1"}); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("global+ctx: %v", err)
	}
	if err := g.CancelShipment(ctx, courier.ShipmentRef{ID: "O1"}); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("global cancel: %v", err)
	}
	if _, err := g.CreateShipment(ctx, createReq()); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("global create: %v", err)
	}
	tn, err := NewTenant(WithBaseURL(f.srv.URL+"/v3"), WithClock(fixedClock))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tn.GetShipment(context.Background(), courier.ShipmentRef{ID: "O1"}); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("tenant w/o ctx: %v", err)
	}
	if _, err := NewTenant(WithBaseURL(f.srv.URL), WithCredentials(creds)); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("tenant WithCredentials: %v", err)
	}
	if f.total() != 0 {
		t.Fatalf("%d outbound calls on mode violations", f.total())
	}
	if g.Mode() != auth.ModeGlobal || tn.Mode() != auth.ModeTenant {
		t.Fatal("modes")
	}
}

func TestGlobalEnvAndMissingCredentials(t *testing.T) {
	t.Setenv("LALAMOVE_API_KEY", "")
	t.Setenv("LALAMOVE_API_SECRET", "")
	t.Setenv("LALAMOVE_MARKET", "")
	if _, err := New(WithSandbox()); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("missing creds: %v", err)
	}
	t.Setenv("LALAMOVE_API_KEY", "pk_test_env")
	t.Setenv("LALAMOVE_API_SECRET", "sk_test_env")
	if _, err := New(WithSandbox()); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("missing market must fail (no default): %v", err)
	}
	t.Setenv("LALAMOVE_MARKET", "MY")
	if _, err := New(WithSandbox()); err != nil {
		t.Fatal(err)
	}
	// tenant never reads the environment
	tn, err := NewTenant(WithSandbox())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tn.GetShipment(context.Background(), courier.ShipmentRef{ID: "O1"}); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("tenant must not use env creds: %v", err)
	}
}

func TestMarketRequiredAndValidated(t *testing.T) {
	for _, m := range []string{"", "sg", "SGP", "S", "S1", " SG", "BDT"} {
		c := creds
		c.Market = m
		if _, err := New(WithCredentials(c), WithBaseURL("http://127.0.0.1:1/v3")); !errors.Is(err, courier.ErrConfiguration) {
			t.Errorf("market %q accepted: %v", m, err)
		}
	}
	// no default: tenant call with empty market fails locally, zero HTTP
	f := newFake(t, nil)
	tn, _ := NewTenant(WithBaseURL(f.srv.URL + "/v3"))
	c := creds
	c.Market = ""
	if _, err := tn.GetShipment(ContextWithCredentials(context.Background(), c), courier.ShipmentRef{ID: "O1"}); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("tenant empty market: %v", err)
	}
	if f.total() != 0 {
		t.Fatal("HTTP sent with invalid market")
	}
	// Bangladesh is not special-cased: BD is only valid as an explicit value, never a default.
	c.Market = "BD"
	if _, err := tn.GetShipment(ContextWithCredentials(context.Background(), c), courier.ShipmentRef{ID: "O1"}); err != nil {
		t.Fatalf("explicit market passes through untouched: %v", err)
	}
	if f.global().env != envCustom {
		t.Fatal("env")
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnvironmentExplicitAndKeyPrefixMismatchFailsLocally(t *testing.T) {
	prod := Credentials{APIKey: "pk_prod_x", APISecret: "sk_prod_x", Market: "SG"}
	if _, err := New(WithCredentials(creds)); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("no environment: %v", err)
	}
	if _, err := NewTenant(); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("tenant no environment: %v", err)
	}
	if _, err := New(WithCredentials(creds), WithSandbox(), WithProduction()); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("both: %v", err)
	}
	if _, err := New(WithCredentials(prod), WithSandbox()); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("prod keys on sandbox: %v", err)
	}
	if _, err := New(WithCredentials(creds), WithProduction()); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("test keys on production: %v", err)
	}
	mixed := creds
	mixed.APISecret = "sk_prod_x"
	if _, err := New(WithCredentials(mixed), WithSandbox()); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("mixed secret: %v", err)
	}
	_, err := New(WithCredentials(prod), WithSandbox())
	if strings.Contains(err.Error(), "pk_prod_x") || strings.Contains(err.Error(), "sk_prod_x") {
		t.Fatal("error leaks credentials")
	}
	// custom host skips the prefix check
	if _, err := New(WithCredentials(prod), WithBaseURL("https://proxy.example/v3")); err != nil {
		t.Fatal(err)
	}
	// tenant: per-call mismatch fails before any HTTP
	var calls atomic.Int32
	hc := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not be called")
	})}
	tn, err := NewTenant(WithSandbox(), WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tn.GetShipment(ContextWithCredentials(context.Background(), prod), courier.ShipmentRef{ID: "O1"}); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("tenant mismatch: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("HTTP attempted on environment mismatch")
	}
}

func TestDocumentedHostsAndPathsViaFakeTransport(t *testing.T) {
	prod := Credentials{APIKey: "pk_prod_x", APISecret: "sk_prod_x", Market: "SG"}
	for _, tc := range []struct {
		opt  Option
		cr   Credentials
		host string
	}{{WithSandbox(), creds, "rest.sandbox.lalamove.com"}, {WithProduction(), prod, "rest.lalamove.com"}} {
		var host, path, authz string
		hc := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			host, path, authz = r.URL.Host, r.URL.Path, r.Header.Get("Authorization")
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(orderJSON("COMPLETED", ""))), Request: r}, nil
		})}
		c, err := New(WithCredentials(tc.cr), tc.opt, WithHTTPClient(hc))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetShipment(context.Background(), courier.ShipmentRef{ID: "O1"}); err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(strings.TrimPrefix(authz, "hmac "), ":")
		if host != tc.host || path != "/v3/orders/O1" || parts[2] != refSign(tc.cr.APISecret.Reveal(), parts[1], "GET", "/v3/orders/O1", nil) {
			t.Fatalf("host=%s path=%s authz ok=%v", host, path, parts[2] == refSign(tc.cr.APISecret.Reveal(), parts[1], "GET", "/v3/orders/O1", nil))
		}
	}
}

func TestConcurrentMultiTenantIsolation(t *testing.T) {
	type tenant struct{ key, secret, market string }
	tenants := []tenant{}
	secrets := map[string]string{}
	markets := map[string]string{}
	for i, m := range []string{"SG", "MY", "TH", "VN", "HK", "ID", "PH", "TW"} {
		k := fmt.Sprintf("pk_test_t%d", i)
		tenants = append(tenants, tenant{k, fmt.Sprintf("sk_test_t%d", i), m})
		secrets[k], markets[k] = fmt.Sprintf("sk_test_t%d", i), m
	}
	var bad atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, _ := checkAuth(t, r, nil, secrets)
		if r.Header.Get("Market") != markets[key] {
			bad.Add(1)
		}
		_, _ = w.Write([]byte(strings.Replace(orderJSON("COMPLETED", ""), `"O1"`, `"`+strings.TrimPrefix(r.URL.Path, "/v3/orders/")+`"`, 1)))
	}))
	defer srv.Close()
	c, err := NewTenant(WithBaseURL(srv.URL + "/v3"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			tn := tenants[g%len(tenants)]
			ctx := ContextWithCredentials(context.Background(), Credentials{APIKey: tn.key, APISecret: auth.Secret(tn.secret), Market: tn.market, AccountID: courier.AccountID(tn.key)})
			for i := 0; i < 10; i++ {
				id := fmt.Sprintf("ORD-%d-%d", g, i)
				sh, err := c.GetShipment(ctx, courier.ShipmentRef{ID: courier.ShipmentID(id)})
				if err != nil || string(sh.Ref.ID) != id || sh.Ref.Account != courier.AccountID(tn.key) {
					t.Errorf("tenant %s: %v %+v", tn.key, err, sh)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d requests carried another tenant's market", bad.Load())
	}
}

// ---- quotation ----

func TestQuoteMapsExactDecimalCurrencyExpiryAndStops(t *testing.T) {
	f := newFake(t, nil)
	q, err := f.global().QuoteWithOptions(context.Background(), makeQuoteReq(), qopts)
	if err != nil {
		t.Fatal(err)
	}
	if q.ID != "Q123" || q.Fee.Amount.String() != "1050.35" || q.Fee.Currency != "SGD" || q.Ref.Account != "acct-1" {
		t.Fatalf("%+v", q)
	}
	if q.ExpiresAt == nil || !q.ExpiresAt.Equal(testNow.Add(5*time.Minute)) {
		t.Fatalf("expiry %v", q.ExpiresAt)
	}
	stops, err := QuoteStops(q)
	if err != nil || len(stops) != 2 || stops[0].ID != "S1" || stops[1].ID != "S2" || stops[0].Lat != "1.3521" {
		t.Fatalf("stops %+v %v", stops, err)
	}
	if !bytes.Contains(q.Raw, []byte(`"stopId":"S1"`)) {
		t.Fatal("stop ids not preserved in Raw")
	}
	// via the canonical interface with options in context
	var iq courier.Quoter = f.global()
	if _, err := iq.Quote(ContextWithQuoteOptions(context.Background(), qopts), makeQuoteReq()); err != nil {
		t.Fatal(err)
	}
	gq, err := f.global().GetQuote(context.Background(), "Q123")
	if err != nil || gq.Fee.String() != "1050.35 SGD" {
		t.Fatalf("%v %v", gq, err)
	}
	var env struct {
		Data struct {
			Stops []struct {
				Coordinates map[string]any `json:"coordinates"`
			} `json:"stops"`
			ScheduleAt string `json:"scheduleAt"`
		} `json:"data"`
	}
	_ = json.Unmarshal(f.bodys["POST /v3/quotations"], &env)
	if _, ok := env.Data.Stops[0].Coordinates["lat"].(string); !ok || env.Data.ScheduleAt != "" {
		t.Fatalf("coordinates must be strings and schedule omitted: %s", f.bodys["POST /v3/quotations"])
	}
}

func TestQuoteInputValidationNoHTTP(t *testing.T) {
	f := newFake(t, nil)
	c := f.global()
	ctx := context.Background()
	if _, err := c.Quote(ctx, makeQuoteReq()); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("missing options: %v", err)
	}
	noCoord := makeQuoteReq()
	noCoord.Pickup.Address.Coordinates = nil
	if _, err := c.QuoteWithOptions(ctx, noCoord, qopts); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("no coordinates: %v", err)
	}
	if _, err := c.QuoteWithOptions(ctx, makeQuoteReq(), QuoteOptions{Language: "en_SG"}); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("no service type: %v", err)
	}
	cod := makeQuoteReq()
	m, _ := courier.ParseMoney("5", "SGD")
	cod.COD = &m
	if _, err := c.QuoteWithOptions(ctx, cod, qopts); !errors.Is(err, courier.ErrUnsupported) {
		t.Fatalf("cod: %v", err)
	}
	if f.total() != 0 {
		t.Fatalf("%d HTTP calls", f.total())
	}
}

func TestQuoteResponseWithoutPriceOrCurrencyIsProtocolError(t *testing.T) {
	for _, pb := range []string{`"priceBreakdown":{"currency":"SGD"}`, `"priceBreakdown":{"total":"5"}`, `"x":1`} {
		f := newFake(t, nil)
		f.quote = `{"data":{"quotationId":"Q123","expiresAt":"2026-11-03T10:05:00Z","stops":[],` + pb + `}}`
		if _, err := f.global().GetQuote(context.Background(), "Q123"); !errors.Is(err, courier.ErrProtocol) {
			t.Errorf("%s: %v", pb, err)
		}
	}
}

// ---- creation ----

func TestCreateRequiresQuoteIDNoHTTP(t *testing.T) {
	f := newFake(t, nil)
	r := createReq()
	r.QuoteID = ""
	if _, err := f.global().CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("%v", err)
	}
	r = createReq()
	r.IdempotencyKey = "k"
	if _, err := f.global().CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrUnsupported) {
		t.Fatalf("idempotency: %v", err)
	}
	r = createReq()
	r.Recipient.Contact.Phone = "0123456789" // national form: never guess a country
	if _, err := f.global().CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("phone: %v", err)
	}
	r = createReq()
	r.QuoteID = "../orders"
	if _, err := f.global().CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("path injection: %v", err)
	}
	if f.total() != 0 {
		t.Fatalf("%d HTTP calls", f.total())
	}
}

func TestCreateExpiredQuoteRejectedLocally(t *testing.T) {
	f := newFake(t, nil)
	late := func() time.Time { return testNow.Add(5 * time.Minute) } // == expiresAt
	c := f.global(WithClock(late))
	_, err := c.CreateShipment(context.Background(), createReq())
	if !errors.Is(err, courier.ErrValidation) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("%v", err)
	}
	if f.count("POST /v3/orders") != 0 {
		t.Fatal("order was sent with an expired quote")
	}
}

func TestCreateMultiStopQuoteUnsupportedAndMismatchRejected(t *testing.T) {
	f := newFake(t, nil)
	f.quote = strings.Replace(quoteJSON, `{"stopId":"S2"`, `{"stopId":"S3","coordinates":{"lat":"1","lng":"1"},"address":"x"},{"stopId":"S2"`, 1)
	if _, err := f.global().CreateShipment(context.Background(), createReq()); !errors.Is(err, courier.ErrUnsupported) {
		t.Fatalf("multi-stop: %v", err)
	}
	f.quote = quoteJSON
	r := createReq()
	r.Recipient.Address.Coordinates = &courier.Coordinates{Lat: 9, Lng: 9}
	if _, err := f.global().CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("coordinate mismatch: %v", err)
	}
	// swapped parties (pickup/recipient coordinates reversed) must not be silently reordered
	r = createReq()
	r.Pickup, r.Recipient = r.Recipient, r.Pickup
	if _, err := f.global().CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("reordered: %v", err)
	}
	f.quote = strings.Replace(quoteJSON, `"stopId":"S2"`, `"stopId":"S1"`, 1)
	if _, err := f.global().CreateShipment(context.Background(), createReq()); !errors.Is(err, courier.ErrProtocol) {
		t.Fatalf("duplicate stop ids: %v", err)
	}
	if f.count("POST /v3/orders") != 0 {
		t.Fatal("order sent")
	}
}

func TestHappyPathQuoteCreateGetTrackCancel(t *testing.T) {
	f := newFake(t, nil)
	c := f.global()
	ctx := context.Background()
	q, err := c.QuoteWithOptions(ctx, makeQuoteReq(), qopts)
	if err != nil {
		t.Fatal(err)
	}
	r := createReq()
	r.QuoteID = q.ID
	sh, err := c.CreateShipmentWithOptions(ctx, r, CreateOptions{EnablePOD: true})
	if err != nil {
		t.Fatal(err)
	}
	var ob struct {
		Data struct {
			QuotationID  string            `json:"quotationId"`
			Sender       map[string]string `json:"sender"`
			Recipients   []map[string]string
			IsPODEnabled bool              `json:"isPODEnabled"`
			Metadata     map[string]string `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(f.bodys["POST /v3/orders"], &ob); err != nil {
		t.Fatal(err)
	}
	d := ob.Data
	if d.QuotationID != "Q123" || d.Sender["stopId"] != "S1" || len(d.Recipients) != 1 || d.Recipients[0]["stopId"] != "S2" ||
		d.Sender["phone"] != "+6581234567" || d.Recipients[0]["phone"] != "+60123456789" || d.Recipients[0]["remarks"] != "leave at door" ||
		!d.IsPODEnabled || d.Metadata["merchantOrderId"] != "M-1" {
		t.Fatalf("order body %s", f.bodys["POST /v3/orders"])
	}
	if sh.Ref.ID != "O1" || sh.Ref.Provider != ProviderID || sh.Ref.Account != "acct-1" || sh.Status != courier.StatusCreated ||
		sh.MerchantOrderID != "M-1" || sh.DeliveryCharge == nil || sh.DeliveryCharge.String() != "1050.35 SGD" || sh.CreatedAt != nil {
		t.Fatalf("%+v", sh)
	}
	got, err := c.GetShipment(ctx, sh.Ref)
	if err != nil || got.Status != courier.StatusPickedUp || got.ProviderStatus.Code != "PICKED_UP" {
		t.Fatalf("%+v %v", got, err)
	}
	res, err := c.TrackShipment(ctx, courier.TrackingRequest{Code: "O1"})
	if err != nil || res.Coverage != courier.CoverageCurrentOnly || len(res.Events) != 0 || res.ObservedAt != testNow {
		t.Fatalf("%+v %v", res, err)
	}
	if err := c.CancelShipment(ctx, sh.Ref); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE /v3/orders/O1") != 1 {
		t.Fatal("cancel not sent")
	}
	// ref scoped to a different account is refused locally
	other := sh.Ref
	other.Account = "acct-2"
	n := f.total()
	if err := c.CancelShipment(ctx, other); !errors.Is(err, courier.ErrValidation) || f.total() != n {
		t.Fatalf("cross-account cancel: %v", err)
	}
}

func TestCancelConflictPreservesProviderCode(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.Method == "DELETE" {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"errors":[{"id":"ERR_CANCELLATION_FORBIDDEN","message":"cannot cancel for +60123456789","detail":"x"}]}`))
			return true
		}
		return false
	})
	err := f.global(WithRetry(fastRetry(3))).CancelShipment(context.Background(), courier.ShipmentRef{ID: "O1"})
	var ce *courier.Error
	if !errors.Is(err, courier.ErrConflict) || !errors.As(err, &ce) || ce.ProviderCode != "ERR_CANCELLATION_FORBIDDEN" || ce.Outcome != courier.OutcomeRejected {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "60123456789") {
		t.Fatal("provider message text leaked into error")
	}
	if f.count("DELETE /v3/orders/O1") != 1 {
		t.Fatal("cancel replayed")
	}
	// cancel 5xx is indeterminate and not replayed
	f2 := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.Method == "DELETE" {
			w.WriteHeader(500)
			return true
		}
		return false
	})
	err = f2.global(WithRetry(fastRetry(3))).CancelShipment(context.Background(), courier.ShipmentRef{ID: "O1"})
	if !courier.IsIndeterminate(err) || f2.count("DELETE /v3/orders/O1") != 1 {
		t.Fatalf("%v", err)
	}
}

func TestCreateIndeterminateAndNeverReplayed(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"502":       func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) },
		"malformed": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); _, _ = w.Write([]byte(`{oops`)) },
		"no-id": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"data":{}}`))
		},
		"timeout": func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
				if r.Method == "POST" && r.URL.Path == "/v3/orders" {
					h(w, r)
					return true
				}
				return false
			})
			ctx := context.Background()
			if name == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			_, err := f.global(WithRetry(fastRetry(5))).CreateShipment(ctx, createReq())
			if !courier.IsIndeterminate(err) {
				t.Fatalf("want indeterminate, got %v", err)
			}
			if n := f.count("POST /v3/orders"); n != 1 {
				t.Fatalf("order create hit %d times", n)
			}
		})
	}
}

func TestRedirectDoesNotForwardAuthorization(t *testing.T) {
	var other atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other.Add(1) }))
	defer b.Close()
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		http.Redirect(w, r, b.URL+"/steal", http.StatusFound)
		return true
	})
	_, err := f.global(WithRetry(fastRetry(1))).GetShipment(context.Background(), courier.ShipmentRef{ID: "O1"})
	if err == nil || other.Load() != 0 {
		t.Fatalf("err=%v other=%d", err, other.Load())
	}
}

// ---- tracking, status, POD, driver ----

func TestStatusMapping(t *testing.T) {
	for code, want := range map[string]courier.Status{
		"ASSIGNING_DRIVER": courier.StatusCreated, "PICKED_UP": courier.StatusPickedUp, "COMPLETED": courier.StatusDelivered,
		"CANCELED": courier.StatusCancelled, "ON_GOING": courier.StatusUnknown, "REJECTED": courier.StatusUnknown,
		"EXPIRED": courier.StatusUnknown, "NEW_THING": courier.StatusUnknown,
	} {
		if got := mapOrderStatus(code, nil); got != want {
			t.Errorf("%s: %s want %s", code, got, want)
		}
	}
	failed := []orderStopData{{POD: &podData{Status: "FAILED"}}}
	if mapOrderStatus("COMPLETED", failed) != courier.StatusUnknown {
		t.Error("completed with failed POD must be unknown")
	}
}

func TestPODAndDeliveryConfirmationAndAbsentPrice(t *testing.T) {
	pod := `,"POD":{"status":"DELIVERED","image":"https://img.example/p.jpg","deliveredAt":"2026-11-03T09:30:00.000Z"}`
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.URL.Path == "/v3/orders/O1" {
			_, _ = w.Write([]byte(orderJSON("COMPLETED", pod)))
			return true
		}
		if r.URL.Path == "/v3/orders/O2" {
			_, _ = w.Write([]byte(`{"data":{"orderId":"O2","status":"ASSIGNING_DRIVER","stops":[]}}`))
			return true
		}
		return false
	})
	c := f.global()
	o, err := c.GetOrder(context.Background(), courier.ShipmentRef{ID: "O1"})
	if err != nil || len(o.Stops) != 2 || o.Stops[0].POD != nil || o.Stops[1].POD == nil || o.Stops[1].POD.Status != "DELIVERED" ||
		o.Stops[1].POD.DeliveredAt == nil || o.Stops[1].POD.Image == "" || o.Shipment.Status != courier.StatusDelivered {
		t.Fatalf("%+v %v", o, err)
	}
	res, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Lookups: []courier.Lookup{{Type: courier.LookupProviderShipmentID, Value: "O1"}}})
	if err != nil || res.Delivery == nil || res.Delivery.DeliveredAt == nil || !res.Delivery.DeliveredAt.Equal(time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", res, err)
	}
	sh, err := c.GetShipment(context.Background(), courier.ShipmentRef{ID: "O2"})
	if err != nil || sh.DeliveryCharge != nil || sh.CreatedAt != nil {
		t.Fatalf("absent price must be nil: %+v %v", sh, err)
	}
	// id echoed by the provider must match the request
	f.h = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		_, _ = w.Write([]byte(orderJSON("COMPLETED", "")))
		return true
	}
	if _, err := c.GetShipment(context.Background(), courier.ShipmentRef{ID: "O9"}); !errors.Is(err, courier.ErrProtocol) {
		t.Fatalf("mismatched order id: %v", err)
	}
}

func TestGetDriver(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		if r.URL.Path == "/v3/orders/O1/drivers/D1" {
			_, _ = w.Write([]byte(`{"data":{"driverId":"D1","name":"Dee","phone":"+6599999999","plateNumber":"SG****A","photo":"https://p","coordinates":{"lat":"1.3","lng":"103.8","updatedAt":"2026-11-03T09:59:50Z"}}}`))
			return true
		}
		return false
	})
	d, err := f.global().GetDriver(context.Background(), courier.ShipmentRef{ID: "O1"}, "D1")
	if err != nil || d.ID != "D1" || d.Lat != "1.3" || d.LocationAt == nil {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestTrackerConformance(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		switch r.URL.Path {
		case "/v3/orders/BAD":
			_, _ = w.Write([]byte(`{oops`))
			return true
		case "/v3/orders/ONGOING":
			_, _ = w.Write([]byte(strings.Replace(orderJSON("ON_GOING", ""), `"O1"`, `"ONGOING"`, 1)))
			return true
		}
		return false
	})
	bad := courier.TrackingRequest{Code: "BAD"}
	contracttest.RunShipmentTracker(t, contracttest.TrackerSuite{
		Tracker: f.global(),
		Cases: []contracttest.TrackerCase{
			{Name: "on going stays unknown", Request: courier.TrackingRequest{Code: "ONGOING"}, Check: func(t *testing.T, r *courier.TrackingResult) {
				if r.Status != courier.StatusUnknown || r.ProviderStatus.Code != "ON_GOING" {
					t.Fatalf("%+v", r)
				}
			}},
			{Name: "picked up", Request: courier.TrackingRequest{Code: "O1"}},
		},
		MalformedRequest: &bad,
	})
}

// ---- negative assertions, privacy ----

func TestCapabilityAssertions(t *testing.T) {
	var c any = &Client{}
	if _, ok := c.(courier.WebhookHandlerProvider); ok {
		t.Error("must not implement WebhookHandlerProvider (LALA-05)")
	}
	if _, ok := c.(courier.WebhookDecoder); ok {
		t.Error("must not implement WebhookDecoder (LALA-05)")
	}
	if _, ok := c.(courier.BatchShipmentCreator); ok {
		t.Error("must not implement BatchShipmentCreator")
	}
	if _, ok := c.(courier.CustomerIncidentReporter); ok {
		t.Error("must not implement CustomerIncidentReporter")
	}
	if _, ok := c.(courier.ProofOfDeliveryReader); ok {
		t.Error("ProofOfDeliveryReader is documented-not-implemented")
	}
	if _, ok := c.(courier.BalanceReader); ok {
		t.Error("unverified balance capability exposed")
	}
}

func TestManifest(t *testing.T) {
	m := Manifest()
	if m.Provider != ProviderID || !m.VerifiedOn.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", m)
	}
	if m.Capabilities["availability_BD"] != courier.UnknownUnverified || m.Capabilities["market_BD"] != courier.UnknownUnverified ||
		m.Capabilities["webhooks"] != courier.DocumentedNotImplemented || m.Capabilities["create_shipment"] != courier.VerifiedImplemented {
		t.Fatalf("%+v", m.Capabilities)
	}
	if !strings.Contains(m.Notes, "not BD") {
		t.Fatalf("notes %q", m.Notes)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestNoPersonalDataSecretsOrSignaturesInLogsOrErrors(t *testing.T) {
	var sig atomic.Value
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "hmac "), ":")
		sig.Store(parts[2])
		if r.Method == "DELETE" {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"errors":[{"id":"ERR_CANCELLATION_FORBIDDEN","message":"Secret Street 9 +65 8123 4567"}]}`))
			return true
		}
		return false
	})
	var buf syncBuf
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := f.global(WithLogger(lg))
	var texts []string
	if _, err := c.CreateShipment(context.Background(), createReq()); err != nil {
		t.Fatal(err)
	}
	err := c.CancelShipment(context.Background(), courier.ShipmentRef{ID: "O1"})
	texts = append(texts, err.Error(), buf.String(), fmt.Sprintf("%v %+v %#v", creds, creds, creds))
	_, err = c.CreateShipment(context.Background(), func() courier.CreateShipmentRequest { r := createReq(); r.Recipient.Contact.Phone = "0123"; return r }())
	texts = append(texts, err.Error())
	if buf.String() == "" {
		t.Fatal("expected debug logs")
	}
	// Credentials never appear anywhere. Personal data never appears in errors;
	// it may appear in debug records, which carry request and response bodies.
	debugLog := buf.String()
	for _, s := range texts {
		secrets := []string{"sk_test_secret", sig.Load().(string)}
		if s != debugLog {
			secrets = append(secrets, "Secret Street", "Hidden Road", "8123", "60123456789", "Recipient", "leave at door")
		}
		for _, secret := range secrets {
			if strings.Contains(s, secret) {
				t.Errorf("leaked %q in %q", secret, s)
			}
		}
	}
}
