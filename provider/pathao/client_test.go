package pathao

import (
	"bytes"
	"context"
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
)

func okJSON(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }
}

func TestConstructionModes(t *testing.T) {
	for _, k := range []string{"PATHAO_CLIENT_ID", "PATHAO_CLIENT_SECRET", "PATHAO_USERNAME", "PATHAO_PASSWORD", "PATHAO_STORE_ID", "PATHAO_ACCOUNT_ID"} {
		t.Setenv(k, "")
	}
	if _, err := New(); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("missing creds must fail construction: %v", err)
	}
	if _, err := New(WithCredentials(Credentials{ClientID: "x"})); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("partial creds: %v", err)
	}
	t.Setenv("PATHAO_CLIENT_ID", "e-id")
	t.Setenv("PATHAO_CLIENT_SECRET", "e-sec")
	t.Setenv("PATHAO_USERNAME", "e-user")
	t.Setenv("PATHAO_PASSWORD", "e-pw")
	t.Setenv("PATHAO_STORE_ID", "42")
	if c, err := New(); err != nil || c.Mode() != auth.ModeGlobal {
		t.Fatalf("env construction: %v", err)
	}
	// tenant never reads env and rejects WithCredentials
	if c, err := NewTenant(); err != nil || c.Mode() != auth.ModeTenant {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := NewTenant(WithCredentials(creds)); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("tenant must reject WithCredentials: %v", err)
	}
	if _, err := New(WithCredentials(creds), WithSandbox(), WithBaseURL("http://127.0.0.1:1")); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("sandbox+baseurl must conflict: %v", err)
	}
	c, _ := New(WithCredentials(creds))
	if c.baseURL != ProductionBaseURL {
		t.Fatalf("default env is production, got %s", c.baseURL)
	}
	c, _ = New(WithCredentials(creds), WithSandbox())
	if c.baseURL != SandboxBaseURL {
		t.Fatalf("sandbox: %s", c.baseURL)
	}
}

func TestGlobalTenantExclusivityNoHTTP(t *testing.T) {
	f := newFake(t, okJSON(createOK))
	g := global(t, f.srv.URL)
	ctx := ContextWithCredentials(context.Background(), creds)
	if _, err := g.CreateShipment(ctx, createReq("O1", "")); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("global with ctx creds: %v", err)
	}
	if _, err := g.ListCities(ctx); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("global list with ctx creds: %v", err)
	}
	tn, err := NewTenant(WithBaseURL(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tn.CreateShipment(context.Background(), createReq("O1", "")); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("tenant without ctx creds: %v", err)
	}
	if _, err := tn.Quote(context.Background(), courier.QuoteRequest{}); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("tenant quote: %v", err)
	}
	if f.allHits.Load() != 0 {
		t.Fatalf("%d outbound calls on mode violation", f.allHits.Load())
	}
}

func TestConcurrentTenantTokenIsolation(t *testing.T) {
	var issued sync.Map // client id -> *atomic.Int32
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		// The store name echoes the bearer token: a leaked token is visible.
		_, _ = fmt.Fprintf(w, `{"type":"success","code":200,"data":{"data":[{"store_id":1,"store_name":%q}],"last_page":1}}`, bearer(r))
	})
	f.issue = func(_ int, tr tokenRequest) string {
		v, _ := issued.LoadOrStore(tr.ClientID, new(atomic.Int32))
		v.(*atomic.Int32).Add(1)
		return "at-for-" + tr.ClientID
	}
	tn, err := NewTenant(WithBaseURL(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	const tenants, perTenant = 8, 15
	var wg sync.WaitGroup
	errs := make(chan error, tenants*perTenant)
	for i := 0; i < tenants; i++ {
		cr := Credentials{ClientID: fmt.Sprintf("client-%d", i), ClientSecret: "s", Username: "u", Password: "p"}
		for j := 0; j < perTenant; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				st, err := tn.ListStores(ContextWithCredentials(context.Background(), cr))
				if err != nil {
					errs <- err
					return
				}
				if len(st) != 1 || st[0].Name != "at-for-"+cr.ClientID {
					errs <- fmt.Errorf("tenant %s saw token %q", cr.ClientID, st[0].Name)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	for i := 0; i < tenants; i++ {
		v, ok := issued.Load(fmt.Sprintf("client-%d", i))
		if !ok || v.(*atomic.Int32).Load() != 1 {
			t.Errorf("tenant %d: token should be issued exactly once", i)
		}
	}
	// same client id, different secret/password => different identity
	a := tn.identity(Credentials{ClientID: "x", ClientSecret: "1", Username: "u", Password: "p"})
	b := tn.identity(Credentials{ClientID: "x", ClientSecret: "2", Username: "u", Password: "p"})
	if a == b {
		t.Fatal("identity must cover every credential field")
	}
}

func TestTokenReuseAndRefreshOnExpiry(t *testing.T) {
	f := newFake(t, okJSON(`{"type":"success","code":200,"data":{"data":[{"city_id":1,"city_name":"Dhaka"}]}}`))
	c := global(t, f.srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := c.ListCities(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if g := f.grantList(); len(g) != 1 || g[0] != "password" {
		t.Fatalf("token should be cached: %v", g)
	}
	// expires_in below the cache skew => every call re-issues, via refresh grant
	f2 := newFake(t, okJSON(`{"type":"success","code":200,"data":{"data":[]}}`))
	f2.expires = 1
	c2 := global(t, f2.srv.URL)
	for i := 0; i < 2; i++ {
		if _, err := c2.ListCities(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	g := f2.grantList()
	if len(g) != 2 || g[0] != "password" || g[1] != "refresh_token" {
		t.Fatalf("want password then refresh_token, got %v", g)
	}
	if f2.tokenReqs[1].RefreshToken != "rt-cid-1" || f2.tokenReqs[1].Password != "" {
		t.Fatalf("refresh request %+v", f2.tokenReqs[1])
	}
}

func TestUnauthorizedReissuesOnceAndRetriesOnce(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, bearer(r))
		mu.Unlock()
		if bearer(r) == "tok-1" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"message":"Unauthenticated."}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"success","code":200,"data":{"data":[{"city_id":1,"city_name":"Dhaka"}]}}`))
	})
	c := global(t, f.srv.URL)
	cities, err := c.ListCities(context.Background())
	if err != nil || len(cities) != 1 {
		t.Fatalf("%v %v", cities, err)
	}
	if strings.Join(seen, ",") != "tok-1,tok-2" || len(f.grantList()) != 2 {
		t.Fatalf("seen=%v grants=%v", seen, f.grantList())
	}
	// persistent 401: exactly one re-issue and one retry, then credentials error
	f2 := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	c2 := global(t, f2.srv.URL)
	_, err = c2.CreateShipment(context.Background(), createReq("O1", ""))
	if !errors.Is(err, courier.ErrCredentials) || courier.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
	if f2.apiHits.Load() != 2 || len(f2.grantList()) != 2 {
		t.Fatalf("api=%d grants=%v", f2.apiHits.Load(), f2.grantList())
	}
}

func TestTokenRejectedIsCredentialsErrorWithoutLeaks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"message":"bad csec-1 pw-1"}`))
	}))
	defer srv.Close()
	_, err := global(t, srv.URL).ListCities(context.Background())
	if !errors.Is(err, courier.ErrCredentials) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "csec-1") || strings.Contains(err.Error(), "pw-1") {
		t.Fatalf("secret leaked: %v", err)
	}
}

func TestCreateHappyPathExactCOD(t *testing.T) {
	var body, path, authz string
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		body, path, authz = readBody(r), r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(createOK))
	})
	sh, err := global(t, f.srv.URL).CreateShipment(context.Background(), createReq("ORD-1", "1250.50"))
	if err != nil {
		t.Fatal(err)
	}
	if path != "/aladdin/api/v1/orders" || authz != "Bearer tok-1" {
		t.Fatalf("path=%s auth=%s", path, authz)
	}
	for _, want := range []string{`"amount_to_collect":1250.5}`, `"recipient_phone":"01711111111"`, `"store_id":777`,
		`"recipient_city":1,`, `"recipient_zone":298,`, `"recipient_area":2104,`, `"item_weight":1.25,`, `"delivery_type":48`, `"item_type":2`, `"item_quantity":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s: %s", want, body)
		}
	}
	if sh.Ref.ID != "DL010224AB12CD" || sh.Ref.Account != "acct-1" || sh.Status != courier.StatusCreated || sh.ProviderStatus.Code != "Pending" {
		t.Fatalf("%+v", sh)
	}
	if sh.DeliveryCharge == nil || sh.DeliveryCharge.Amount.String() != "60" || sh.COD != nil || sh.CreatedAt != nil {
		t.Fatalf("absent values must stay absent: %+v", sh)
	}
	// no COD => explicit zero literal in the request (documented requirement)
	f2 := newFake(t, func(w http.ResponseWriter, r *http.Request) { body = readBody(r); _, _ = w.Write([]byte(createOK)) })
	if _, err := global(t, f2.srv.URL).CreateShipment(context.Background(), createReq("ORD-1", "")); err != nil || !strings.Contains(body, `"amount_to_collect":0}`) {
		t.Fatalf("%v %s", err, body)
	}
}

func TestExplicitLocationRequiredNoHTTP(t *testing.T) {
	f := newFake(t, okJSON(createOK))
	c := global(t, f.srv.URL)
	for _, drop := range []string{"city", "zone", "area"} {
		r := createReq("O1", "")
		var refs []courier.LocationRef
		for _, x := range r.Recipient.Address.Refs {
			if x.Kind != drop {
				refs = append(refs, x)
			}
		}
		r.Recipient.Address.Refs = refs
		if _, err := c.CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrAmbiguous) {
			t.Errorf("missing %s: %v", drop, err)
		}
	}
	// text/city names never substitute for refs
	r := createReq("O1", "")
	r.Recipient.Address.Refs = nil
	r.Recipient.Address.City, r.Recipient.Address.Area = "Dhaka", "Dhanmondi"
	if _, err := c.CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrAmbiguous) {
		t.Errorf("text address: %v", err)
	}
	// other providers' refs are ignored; conflicting pathao refs are ambiguous; non-numeric is validation
	r = createReq("O1", "")
	r.Recipient.Address.Refs = append(r.Recipient.Address.Refs, courier.LocationRef{Provider: "other", Kind: "city", ID: "9"}, ref("city", "2"))
	if _, err := c.CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrAmbiguous) {
		t.Errorf("conflict: %v", err)
	}
	r = createReq("O1", "")
	r.Recipient.Address.Refs[0] = ref("city", "dhaka")
	if _, err := c.CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrValidation) {
		t.Errorf("non numeric: %v", err)
	}
	if _, err := c.Quote(context.Background(), courier.QuoteRequest{Recipient: courier.Party{Address: courier.Address{City: "Dhaka"}}, Package: courier.Package{WeightGrams: weight(1000)}}); !errors.Is(err, courier.ErrAmbiguous) {
		t.Errorf("quote without refs: %v", err)
	}
	if f.allHits.Load() != 0 {
		t.Fatalf("%d HTTP calls for invalid input", f.allHits.Load())
	}
}

func TestOtherValidationBeforeHTTP(t *testing.T) {
	f := newFake(t, okJSON(createOK))
	c := global(t, f.srv.URL)
	usd := createReq("O2", "5")
	usd.COD.Currency = "USD"
	neg := createReq("O3", "-5")
	noW := createReq("O4", "")
	noW.Package.WeightGrams = nil
	heavy := createReq("O5", "")
	heavy.Package.WeightGrams = weight(10001)
	intl := createReq("O6", "")
	intl.Recipient.Contact.Phone = "+14155550123"
	point := createReq("O7", "")
	point.Delivery = courier.DeliveryPoint
	short := createReq("O8", "")
	short.Recipient.Address.Text = "Dhaka"
	quoted := createReq("O9", "")
	quoted.QuoteID = "q"
	noStore := Credentials{ClientID: "a", ClientSecret: "b", Username: "c", Password: "d"}
	for i, r := range []courier.CreateShipmentRequest{usd, neg, noW, heavy, intl, point, short, quoted} {
		if _, err := c.CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrValidation) {
			t.Errorf("case %d: %v", i, err)
		}
	}
	tn, _ := NewTenant(WithBaseURL(f.srv.URL))
	if _, err := tn.CreateShipment(ContextWithCredentials(context.Background(), noStore), createReq("O", "")); !errors.Is(err, courier.ErrValidation) {
		t.Errorf("missing store (tenant never falls back): %v", err)
	}
	if f.allHits.Load() != 0 {
		t.Fatalf("%d HTTP calls", f.allHits.Load())
	}
}

func TestEnvelopeFailureInHTTP200(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		kind error
	}{
		"422":        {`{"type":"error","code":422,"message":"The recipient name field is required."}`, courier.ErrValidation},
		"type-error": {`{"type":"error","code":200,"message":"nope"}`, courier.ErrRejected},
		"401":        {`{"type":"error","code":401,"message":"x"}`, courier.ErrCredentials},
		"429":        {`{"type":"error","code":429,"message":"x"}`, courier.ErrRateLimit},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, okJSON(tc.body))
			c := global(t, f.srv.URL)
			_, err := c.CreateShipment(context.Background(), createReq("O1", ""))
			if !errors.Is(err, tc.kind) || courier.IsIndeterminate(err) {
				t.Fatalf("create: %v", err)
			}
			var ce *courier.Error
			if errors.As(err, &ce) && ce.Outcome != courier.OutcomeRejected {
				t.Fatalf("outcome %q", ce.Outcome)
			}
			if _, err := c.ListStores(context.Background()); !errors.Is(err, tc.kind) {
				t.Fatalf("stores: %v", err)
			}
			if _, err := c.Quote(context.Background(), quoteReq()); !errors.Is(err, tc.kind) {
				t.Fatalf("quote: %v", err)
			}
		})
	}
	// embedded 5xx on a mutation is indeterminate
	f := newFake(t, okJSON(`{"type":"error","code":500,"message":"boom"}`))
	if _, err := global(t, f.srv.URL).CreateShipment(context.Background(), createReq("O1", "")); !courier.IsIndeterminate(err) {
		t.Fatalf("embedded 500: %v", err)
	}
}

func TestCreateIndeterminateNeverReplayed(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) })
	c := global(t, f.srv.URL, WithRetry(httpxRetry(5)))
	_, err := c.CreateShipment(context.Background(), createReq("O1", ""))
	if !courier.IsIndeterminate(err) || f.apiHits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, f.apiHits.Load())
	}
	for name, body := range map[string]string{
		"malformed": `{oops`, "no-data": `{"type":"success","code":200}`,
		"no-id":  `{"type":"success","code":200,"data":{"order_status":"Pending"}}`,
		"bad-id": `{"type":"success","code":200,"data":{"consignment_id":"a/b"}}`,
	} {
		f2 := newFake(t, okJSON(body))
		if _, err := global(t, f2.srv.URL).CreateShipment(context.Background(), createReq("O1", "")); !courier.IsIndeterminate(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f3 := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := global(t, f3.srv.URL).CreateShipment(ctx, createReq("O1", "")); !courier.IsIndeterminate(err) {
		t.Fatalf("timeout must be indeterminate: %v", err)
	}
}

const infoFixture = `{"type":"success","code":200,"data":{"consignment_id":"%s","order_status":"%s","order_status_slug":"%s","updated_at":"2024-02-01 10:00:00","invoice_id":null}}`

func trackServer(t *testing.T) *fake {
	return newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/BADJSON/info"):
			_, _ = w.Write([]byte(`{oops`))
		case strings.HasSuffix(r.URL.Path, "/DL1/info"):
			_, _ = fmt.Fprintf(w, infoFixture, "DL1", "Delivered", "Delivered")
		case strings.HasSuffix(r.URL.Path, "/DL2/info"):
			_, _ = fmt.Fprintf(w, infoFixture, "DL2", "Brand_New_Status", "Brand New")
		case strings.HasSuffix(r.URL.Path, "/NOPE/info"):
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"Not found"}`))
		default:
			_, _ = fmt.Fprintf(w, infoFixture, "DL3", "In_Transit", "In Transit")
		}
	})
}

func TestTrackerConformance(t *testing.T) {
	f := trackServer(t)
	c := global(t, f.srv.URL)
	bad := courier.TrackingRequest{Code: "BADJSON"}
	contracttest.RunShipmentTracker(t, contracttest.TrackerSuite{
		Tracker: c, MalformedRequest: &bad,
		Cases: []contracttest.TrackerCase{
			{Name: "delivered", Request: courier.TrackingRequest{Code: "DL1"}, Check: func(t *testing.T, r *courier.TrackingResult) {
				if r.Status != courier.StatusDelivered || r.Delivery == nil || r.Delivery.DeliveredAt != nil {
					t.Fatalf("%+v", r)
				}
				if r.Coverage != courier.CoverageCurrentOnly || len(r.Events) != 0 || r.Ref.Account != "acct-1" || r.Ref.ID != "DL1" {
					t.Fatalf("%+v", r)
				}
			}},
			{Name: "unknown status stays unknown", Request: courier.TrackingRequest{Code: "DL2"}, Check: func(t *testing.T, r *courier.TrackingResult) {
				if r.Status != courier.StatusUnknown || r.ProviderStatus.Code != "Brand_New_Status" || r.ProviderStatus.Description != "Brand New" || r.Delivery != nil {
					t.Fatalf("%+v", r)
				}
			}},
			{Name: "provider shipment id lookup", Request: courier.TrackingRequest{Lookups: []courier.Lookup{{Type: courier.LookupProviderShipmentID, Value: "DL3"}}}, Check: func(t *testing.T, r *courier.TrackingResult) {
				if r.Status != courier.StatusInTransit {
					t.Fatalf("%+v", r)
				}
			}},
		},
	})
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "NOPE"}); !errors.Is(err, courier.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	before := f.allHits.Load()
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Lookups: []courier.Lookup{{Type: courier.LookupMerchantOrderID, Value: "ORD-1"}}}); !errors.Is(err, courier.ErrUnsupported) {
		t.Fatalf("merchant order lookup must be unsupported: %v", err)
	}
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "../x"}); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("path injection: %v", err)
	}
	if f.allHits.Load() != before {
		t.Fatal("unsupported/invalid lookups must not call the API")
	}
}

func TestStatusMapping(t *testing.T) {
	cases := map[string]courier.Status{
		"Pending": courier.StatusCreated, "Pickup_Requested": courier.StatusPickupPending, "Pickup": courier.StatusPickedUp,
		"In_Transit": courier.StatusInTransit, "In Transit": courier.StatusInTransit, "Delivered": courier.StatusDelivered,
		"Cancel": courier.StatusCancelled, "Returned": courier.StatusReturned, "On_Hold": courier.StatusOnHold,
		"Return": courier.StatusUnknown, "Partial_Delivery": courier.StatusUnknown, "": courier.StatusUnknown, "weird": courier.StatusUnknown,
	}
	for in, want := range cases {
		if got := mapStatusValue(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

func quoteReq() courier.QuoteRequest {
	return courier.QuoteRequest{
		Recipient: courier.Party{Address: courier.Address{Refs: []courier.LocationRef{ref("city", "1"), ref("zone", "298")}}},
		Package:   courier.Package{WeightGrams: weight(500)},
	}
}

func TestQuote(t *testing.T) {
	var body, method string
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		body, method = readBody(r), r.URL.Path
		_, _ = w.Write([]byte(`{"type":"success","code":200,"data":{"price":80,"discount":0,"plan_id":1,"cod_enabled":1,"cod_percentage":0.01,"final_price":"79.90"}}`))
	})
	q, err := global(t, f.srv.URL).Quote(context.Background(), quoteReq())
	if err != nil {
		t.Fatal(err)
	}
	if method != "/aladdin/api/v1/merchant/price-plan" || !strings.Contains(body, `"item_weight":0.5`) || !strings.Contains(body, `"store_id":777`) {
		t.Fatalf("%s %s", method, body)
	}
	if q.Fee.String() != "79.9 BDT" || q.ExpiresAt != nil || q.ID != "" {
		t.Fatalf("%+v", q)
	}
	f2 := newFake(t, okJSON(`{"type":"success","code":200,"data":{"price":80}}`))
	if _, err := global(t, f2.srv.URL).Quote(context.Background(), quoteReq()); !errors.Is(err, courier.ErrProtocol) {
		t.Fatalf("missing final_price must not become zero: %v", err)
	}
	// price-plan POST is never auto-retried, even on 5xx with retries enabled
	f3 := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	if _, err := global(t, f3.srv.URL, WithRetry(httpxRetry(4))).Quote(context.Background(), quoteReq()); !errors.Is(err, courier.ErrUnavailable) || f3.apiHits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, f3.apiHits.Load())
	}
}

func TestNativeListings(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		switch r.URL.Path {
		case "/aladdin/api/v1/stores":
			if r.URL.Query().Get("page") == "2" {
				_, _ = w.Write([]byte(`{"type":"success","code":200,"data":{"data":[{"store_id":2,"store_name":"B","is_active":0}],"current_page":2,"last_page":2}}`))
				return
			}
			_, _ = w.Write([]byte(`{"type":"success","code":200,"data":{"data":[{"store_id":1,"store_name":"A","store_address":"addr","is_active":1}],"current_page":1,"last_page":2}}`))
		case "/aladdin/api/v1/city-list":
			_, _ = w.Write([]byte(`{"type":"success","code":200,"data":{"data":[{"city_id":1,"city_name":"Dhaka"},{"city_id":"2","city_name":"Chittagong"}]}}`))
		case "/aladdin/api/v1/cities/1/zone-list":
			_, _ = w.Write([]byte(`{"type":"success","code":200,"data":{"data":[{"zone_id":298,"zone_name":"Dhanmondi"}]}}`))
		case "/aladdin/api/v1/zones/298/area-list":
			_, _ = w.Write([]byte(`{"type":"success","code":200,"data":{"data":[{"area_id":2104,"area_name":"Road 1","home_delivery_available":true,"pickup_available":0}]}}`))
		default:
			w.WriteHeader(404)
		}
	})
	c := global(t, f.srv.URL)
	ctx := context.Background()
	st, err := c.ListStores(ctx)
	if err != nil || len(st) != 2 || st[0].ID != "1" || st[0].IsActive == nil || !*st[0].IsActive || *st[1].IsActive || st[0].Address != "addr" {
		t.Fatalf("%+v %v", st, err)
	}
	cs, err := c.ListCities(ctx)
	if err != nil || len(cs) != 2 || cs[1].ID != "2" || cs[0].Ref() != ref("city", "1") {
		t.Fatalf("%+v %v", cs, err)
	}
	zs, err := c.ListZones(ctx, "1")
	if err != nil || zs[0].Ref() != ref("zone", "298") {
		t.Fatalf("%+v %v", zs, err)
	}
	as, err := c.ListAreas(ctx, "298")
	if err != nil || as[0].Ref() != ref("area", "2104") || !*as[0].HomeDeliveryAvailable || *as[0].PickupAvailable {
		t.Fatalf("%+v %v", as, err)
	}
	before := f.allHits.Load()
	if _, err := c.ListZones(ctx, "1/../x"); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("%v", err)
	}
	if _, err := c.ListAreas(ctx, ""); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("%v", err)
	}
	if f.allHits.Load() != before {
		t.Fatal("invalid IDs must not hit the API")
	}
}

func TestRedirectDoesNotForwardAuthorization(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
		leaked.Add(100)
	}))
	defer other.Close()
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	})
	_, err := global(t, f.srv.URL).ListCities(context.Background())
	if !errors.Is(err, courier.ErrProtocol) {
		t.Fatalf("redirect should be a protocol error: %v", err)
	}
	if leaked.Load() != 0 {
		t.Fatalf("redirect was followed (%d)", leaked.Load())
	}
}

func TestNoSensitiveDataInLogsOrErrors(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("X-Fail"), "1") {
			return
		}
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"type":"error","code":422,"message":"Invalid phone 01711111111 for House 1"}`))
	})
	c := global(t, f.srv.URL, WithLogger(lg))
	_, err := c.CreateShipment(context.Background(), createReq("O1", "10"))
	f.api = okJSON(createOK)
	_, err2 := c.CreateShipment(context.Background(), createReq("O1", "10"))
	if err == nil || err2 != nil {
		t.Fatalf("%v %v", err, err2)
	}
	// credentials never appear in logs or errors; personal data never appears
	// in errors (debug records carry bodies by design)
	all := buf.String() + err.Error()
	for _, s := range []string{"tok-1", "csec-1", "pw-1"} {
		if strings.Contains(all, s) {
			t.Errorf("credential %q leaked into logs/errors", s)
		}
	}
	for _, s := range []string{"01711111111", "Dhanmondi", "Noki", "u1@example.test"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("personal data %q leaked into an error", s)
		}
	}
	if buf.Len() == 0 {
		t.Fatal("expected debug logs to be produced")
	}
}

func TestCapabilityAssertions(t *testing.T) {
	var c any = &Client{}
	if _, ok := c.(courier.ShipmentCreator); !ok {
		t.Error("ShipmentCreator")
	}
	if _, ok := c.(courier.ShipmentTracker); !ok {
		t.Error("ShipmentTracker")
	}
	if _, ok := c.(courier.Quoter); !ok {
		t.Error("Quoter")
	}
	if _, ok := c.(courier.BatchShipmentCreator); ok {
		t.Error("must not be a BatchShipmentCreator")
	}
	if _, ok := c.(courier.WebhookHandlerProvider); ok {
		t.Error("must not be a WebhookHandlerProvider")
	}
	if _, ok := c.(courier.WebhookDecoder); ok {
		t.Error("must not be a WebhookDecoder")
	}
	if _, ok := c.(courier.CustomerIncidentReporter); ok {
		t.Error("must not be a CustomerIncidentReporter")
	}
	if _, ok := c.(courier.ShipmentReader); ok {
		t.Error("must not be a ShipmentReader")
	}
}

func TestManifest(t *testing.T) {
	m := Manifest()
	if m.Provider != ProviderID {
		t.Fatalf("%+v", m)
	}
	if m.Capabilities["webhooks"] == courier.VerifiedImplemented || m.Capabilities["create_shipments"] == courier.VerifiedImplemented {
		t.Fatal("unimplemented capabilities must not be verified")
	}
}
