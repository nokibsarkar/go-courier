package steadfast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

var creds = Credentials{APIKey: "key-1", SecretKey: "sec-1", AccountID: "acct-1"}

func newServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); h(w, r) }))
	t.Cleanup(s.Close)
	return s, &hits
}

func global(t *testing.T, url string, opts ...Option) *Client {
	t.Helper()
	c, err := New(append([]Option{WithCredentials(creds), WithBaseURL(url)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func req(order string, cod string) courier.CreateShipmentRequest {
	r := courier.CreateShipmentRequest{
		MerchantOrderID: courier.MerchantOrderID(order),
		Recipient: courier.Party{
			Contact: courier.Contact{Name: "Noki", Phone: "01711-111111"},
			Address: courier.Address{Text: "Dhanmondi, Dhaka"},
		},
	}
	if cod != "" {
		m, _ := courier.ParseMoney(cod, courier.BDT)
		r.COD = &m
	}
	return r
}

func TestCreateShipmentExactMoneyHeadersAndMapping(t *testing.T) {
	var body []byte
	var hdr http.Header
	var path string
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr, path = r.Header.Clone(), r.URL.Path
		_, _ = w.Write([]byte(`{"status":200,"message":"ok","consignment":{"consignment_id":1424107,"invoice":"ORD-1","tracking_code":"15BAEB8A","status":"in_review","cod_amount":"1250.50","created_at":"2021-03-21T07:05:31.000000Z"}}`))
	})
	sh, err := global(t, srv.URL).CreateShipment(context.Background(), req("ORD-1", "1250.50"))
	if err != nil {
		t.Fatal(err)
	}
	if path != "/create_order" || hdr.Get("Api-Key") != "key-1" || hdr.Get("Secret-Key") != "sec-1" {
		t.Fatalf("path=%s hdr=%v", path, hdr)
	}
	if !strings.Contains(string(body), `"cod_amount":1250.5,`) || !strings.Contains(string(body), `"recipient_phone":"01711111111"`) {
		t.Fatalf("body %s", body)
	}
	if sh.Ref.ID != "1424107" || sh.Ref.Account != "acct-1" || sh.TrackingCode != "15BAEB8A" || sh.Status != courier.StatusCreated {
		t.Fatalf("%+v", sh)
	}
	if sh.COD == nil || sh.COD.Amount.String() != "1250.5" {
		t.Fatalf("cod %v", sh.COD)
	}
	if sh.CreatedAt == nil || sh.UpdatedAt != nil {
		t.Fatal("absent provider timestamp must stay nil")
	}
}

func TestValidationHappensBeforeAnyHTTP(t *testing.T) {
	srv, hits := newServer(t, func(w http.ResponseWriter, r *http.Request) {})
	c := global(t, srv.URL)
	bad := []courier.CreateShipmentRequest{req("", ""), req("has space", ""), req("O1", "-5")}
	usd := req("O2", "5")
	usd.COD.Currency = "USD"
	noPhone := req("O3", "")
	noPhone.Recipient.Contact.Phone = "4155550123"
	bad = append(bad, usd, noPhone)
	for i, r := range bad {
		if _, err := c.CreateShipment(context.Background(), r); err == nil {
			t.Errorf("case %d: want error", i)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d HTTP calls for invalid input", hits.Load())
	}
}

func TestEmbeddedFailureInHTTP200IsAnError(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":400,"message":"invoice already exists"}`))
	})
	_, err := global(t, srv.URL).CreateShipment(context.Background(), req("ORD-1", ""))
	if !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("got %v", err)
	}
	srv2, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":401,"message":"bad credentials"}`))
	})
	if _, err := global(t, srv2.URL).GetBalance(context.Background()); !errors.Is(err, courier.ErrCredentials) {
		t.Fatalf("got %v", err)
	}
}

func TestCreateIndeterminateAndNeverReplayed(t *testing.T) {
	srv, hits := newServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) })
	c := global(t, srv.URL, WithRetry(httpxRetry(5)))
	_, err := c.CreateShipment(context.Background(), req("ORD-1", ""))
	if !courier.IsIndeterminate(err) || hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
	// 200 with malformed JSON after a mutation is also indeterminate
	srv2, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{oops`)) })
	if _, err := global(t, srv2.URL).CreateShipment(context.Background(), req("ORD-1", "")); !courier.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
	// timeout
	srv3, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := global(t, srv3.URL).CreateShipment(ctx, req("ORD-1", "")); !courier.IsIndeterminate(err) {
		t.Fatalf("timeout must be indeterminate: %v", err)
	}
}

func TestBulkPerItemReceipts(t *testing.T) {
	var sent []orderRequest
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		var br bulkRequest
		_ = json.NewDecoder(r.Body).Decode(&br)
		sent = br.Data
		// answer out of order, reject one, and omit one
		_, _ = w.Write([]byte(`[{"invoice":"B","status":"error","error":"invalid address"},{"invoice":"A","consignment_id":11,"tracking_code":"T1","status":"success"}]`))
	})
	reqs := []courier.CreateShipmentRequest{req("A", "10"), req("B", ""), req("C", ""), req("bad id", "")}
	res, err := global(t, srv.URL).CreateShipments(context.Background(), reqs)
	if err == nil || res == nil || len(res.Items) != 4 {
		t.Fatalf("res=%v err=%v", res, err)
	}
	want := []courier.BatchStatus{courier.BatchSucceeded, courier.BatchFailed, courier.BatchIndeterminate, courier.BatchFailed}
	for i, w := range want {
		if res.Items[i].Status != w || res.Items[i].Index != i {
			t.Errorf("item %d = %s want %s", i, res.Items[i].Status, w)
		}
	}
	if res.Items[0].Shipment == nil || res.Items[0].Shipment.Ref.ID != "11" || len(sent) != 3 {
		t.Fatalf("shipment/sent: %+v %d", res.Items[0].Shipment, len(sent))
	}
	if _, err := global(t, srv.URL).CreateShipments(context.Background(), nil); err == nil {
		t.Fatal("empty batch")
	}
	if _, err := global(t, srv.URL).CreateShipments(context.Background(), make([]courier.CreateShipmentRequest, 501)); err == nil {
		t.Fatal("over limit")
	}
}

func TestTrackingStatusMapping(t *testing.T) {
	cases := map[string]courier.Status{
		"pending": courier.StatusCreated, "in_review": courier.StatusCreated, "hold": courier.StatusOnHold,
		"delivered": courier.StatusDelivered, "cancelled": courier.StatusCancelled,
		"delivered_approval_pending": courier.StatusUnknown, "partial_delivered": courier.StatusUnknown,
		"cancelled_approval_pending": courier.StatusUnknown, "exceptional": courier.StatusUnknown, "unknown_approval_pending": courier.StatusUnknown, "partial_delivered_approval_pending": courier.StatusUnknown, "brand_new_status": courier.StatusUnknown,
	}
	for raw, want := range cases {
		raw, want := raw, want
		srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"status":200,"delivery_status":%q}`, raw)
		})
		res, err := global(t, srv.URL).TrackShipment(context.Background(), courier.TrackingRequest{Code: "X"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != want || res.ProviderStatus.Code != raw || res.Coverage != courier.CoverageCurrentOnly || len(res.Events) != 0 {
			t.Errorf("%s -> %+v", raw, res)
		}
		if (res.Delivery != nil) != (want == courier.StatusDelivered) {
			t.Errorf("%s delivery confirmation %v", raw, res.Delivery)
		}
	}
}

func TestTrackingLookupRoutesAndValidation(t *testing.T) {
	var path string
	srv, hits := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"status":200,"delivery_status":"pending"}`))
	})
	c := global(t, srv.URL)
	for want, tr := range map[string]courier.TrackingRequest{
		"/status_by_trackingcode/ABC": {Code: "ABC"},
		"/status_by_invoice/ORD-1":    {Lookups: []courier.Lookup{{Type: courier.LookupMerchantOrderID, Value: "ORD-1"}}},
		"/status_by_cid/123":          {Lookups: []courier.Lookup{{Type: courier.LookupProviderShipmentID, Value: "123"}}},
	} {
		if _, err := c.TrackShipment(context.Background(), tr); err != nil || path != want {
			t.Errorf("%s: %v (%s)", want, err, path)
		}
	}
	before := hits.Load()
	for _, tr := range []courier.TrackingRequest{
		{Lookups: []courier.Lookup{{Type: courier.LookupProviderShipmentID, Value: "12x"}}},
		{Lookups: []courier.Lookup{{Type: courier.LookupPhone, Value: "0171"}}},
	} {
		if _, err := c.TrackShipment(context.Background(), tr); err == nil {
			t.Error("want error")
		}
	}
	if hits.Load() != before {
		t.Fatal("invalid lookups reached the network")
	}
}

func TestTrackerConformance(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "BAD") {
			_, _ = w.Write([]byte(`{"status":200}`)) // success envelope without delivery_status
			return
		}
		_, _ = w.Write([]byte(`{"status":200,"delivery_status":"delivered"}`))
	})
	bad := courier.TrackingRequest{Code: "BAD"}
	contracttest.RunShipmentTracker(t, contracttest.TrackerSuite{
		Tracker:          global(t, srv.URL),
		Cases:            []contracttest.TrackerCase{{Name: "delivered", Request: courier.TrackingRequest{Code: "OK1"}}},
		MalformedRequest: &bad,
	})
}

func TestGlobalModeRejectsContextCredentialsWithoutDispatch(t *testing.T) {
	srv, hits := newServer(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := ContextWithCredentials(context.Background(), Credentials{APIKey: "t", SecretKey: "t"})
	c := global(t, srv.URL)
	if _, err := c.GetBalance(ctx); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.CreateShipment(ctx, req("O", "")); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("got %v", err)
	}
	if hits.Load() != 0 || c.Mode() != auth.ModeGlobal {
		t.Fatal("outbound call made / wrong mode")
	}
}

func TestTenantModeNeverFallsBack(t *testing.T) {
	t.Setenv("STEADFAST_API_KEY", "env-key")
	t.Setenv("STEADFAST_SECRET_KEY", "env-secret")
	srv, hits := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":200,"current_balance":5}`))
	})
	c, err := NewTenant(WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetBalance(context.Background()); !errors.Is(err, courier.ErrAuthMode) || hits.Load() != 0 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
	// empty tenant credentials also fail locally
	if _, err := c.GetBalance(ContextWithCredentials(context.Background(), Credentials{})); err == nil || hits.Load() != 0 {
		t.Fatalf("empty creds: %v", err)
	}
	if _, err := NewTenant(WithCredentials(creds)); err == nil {
		t.Fatal("tenant constructor must reject global credentials")
	}
}

func TestGlobalConstructionRequiresLocalCredentialsAndMakesNoCalls(t *testing.T) {
	t.Setenv("STEADFAST_API_KEY", "")
	t.Setenv("STEADFAST_SECRET_KEY", "")
	if _, err := New(); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("got %v", err)
	}
	t.Setenv("STEADFAST_API_KEY", "ek")
	t.Setenv("STEADFAST_SECRET_KEY", "es")
	var hdr http.Header
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		_, _ = w.Write([]byte(`{"status":200,"current_balance":"1000.00"}`))
	})
	t.Setenv("STEADFAST_BASE_URL", srv.URL)
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.GetBalance(context.Background())
	if err != nil || b.Amount.String() != "1000 BDT" || hdr.Get("Api-Key") != "ek" {
		t.Fatalf("%v %v %v", b, err, hdr)
	}
}

func TestConcurrentTenantsNeverLeakCredentials(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		k, s := r.Header.Get("Api-Key"), r.Header.Get("Secret-Key")
		if strings.TrimPrefix(k, "k-") != strings.TrimPrefix(s, "s-") {
			http.Error(w, "mixed credentials", 500)
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":200,"current_balance":%q}`, strings.TrimPrefix(k, "k-"))
	})
	c, _ := NewTenant(WithBaseURL(srv.URL))
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprint(i)
			ctx := ContextWithCredentials(context.Background(), Credentials{APIKey: auth.Secret("k-" + id), SecretKey: auth.Secret("s-" + id), AccountID: courier.AccountID("a" + id)})
			b, err := c.GetBalance(ctx)
			if err != nil || b.Amount.Amount.String() != id || b.Account != courier.AccountID("a"+id) {
				t.Errorf("tenant %s: %v %v", id, b, err)
			}
		}()
	}
	wg.Wait()
}

func TestRedirectNeverForwardsCredentials(t *testing.T) {
	var leaked atomic.Bool
	evil, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != "" {
			leaked.Store(true)
		}
	})
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, evil.URL, 302) })
	if _, err := global(t, srv.URL).GetBalance(context.Background()); !errors.Is(err, courier.ErrProtocol) || leaked.Load() {
		t.Fatalf("err=%v leaked=%v", err, leaked.Load())
	}
}

func TestReadsRetryButCredentialRejectionDoesNot(t *testing.T) {
	var n atomic.Int32
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"status":200,"current_balance":1}`))
	})
	if _, err := global(t, srv.URL, WithRetry(httpxRetry(3))).GetBalance(context.Background()); err != nil || n.Load() != 3 {
		t.Fatalf("err=%v n=%d", err, n.Load())
	}
	var m atomic.Int32
	srv2, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) { m.Add(1); w.WriteHeader(401) })
	if _, err := global(t, srv2.URL, WithRetry(httpxRetry(3))).GetBalance(context.Background()); !errors.Is(err, courier.ErrCredentials) || m.Load() != 1 {
		t.Fatalf("err=%v n=%d", err, m.Load())
	}
}

func TestSecretsNeverAppearInErrorsOrDefaultLogs(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"message":"denied"}`))
	})
	_, err := global(t, srv.URL).GetBalance(context.Background())
	if err == nil || strings.Contains(err.Error(), "key-1") || strings.Contains(err.Error(), "sec-1") {
		t.Fatalf("leak in %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v", creds), "key-1") {
		t.Fatal("credentials print secrets")
	}
}

func TestFieldLimitsRejectedNotTruncated(t *testing.T) {
	srv, hits := newServer(t, func(w http.ResponseWriter, r *http.Request) {})
	c := global(t, srv.URL)
	long := func(f func(*courier.CreateShipmentRequest), n int) courier.CreateShipmentRequest {
		r := req("O1", "")
		f(&r)
		_ = n
		return r
	}
	for name, r := range map[string]courier.CreateShipmentRequest{
		"invoice": long(func(r *courier.CreateShipmentRequest) {
			r.MerchantOrderID = courier.MerchantOrderID(strings.Repeat("a", 101))
		}, 0),
		"name":    long(func(r *courier.CreateShipmentRequest) { r.Recipient.Contact.Name = strings.Repeat("n", 101) }, 0),
		"address": long(func(r *courier.CreateShipmentRequest) { r.Recipient.Address.Text = strings.Repeat("x", 491) }, 0),
		"note":    long(func(r *courier.CreateShipmentRequest) { r.Note = strings.Repeat("x", 481) }, 0),
	} {
		if _, err := c.CreateShipment(context.Background(), r); !errors.Is(err, courier.ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := req("O1", "")
	ok.Recipient.Address.Text = strings.Repeat("x", 490)
	if _, err := buildOrder(ok); err != nil {
		t.Fatalf("490-char address must be accepted: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("network reached")
	}
}

func TestPingNeedsNoCredentials(t *testing.T) {
	var hdr http.Header
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) { hdr = r.Header.Clone() })
	c, _ := NewTenant(WithBaseURL(srv.URL)) // tenant client, no ctx credentials
	if err := c.Ping(context.Background()); err != nil || hdr.Get("Api-Key") != "" || hdr.Get("Secret-Key") != "" {
		t.Fatalf("err=%v hdr=%v", err, hdr)
	}
}

func TestConfirmedDeliveryMarksCODOwed(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":200,"delivery_status":"delivered"}`))
	})
	res, err := global(t, srv.URL).TrackShipment(context.Background(), courier.TrackingRequest{Code: "X"})
	if err != nil || res.COD != courier.CODCollected {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestManifestAndInterfaces(t *testing.T) {
	m := Manifest()
	if m.Provider != ProviderID || m.Capabilities["create_shipment"] != courier.VerifiedImplemented || len(m.Unverified) == 0 {
		t.Fatalf("%+v", m)
	}
	var c any = &Client{}
	if _, ok := c.(courier.ShipmentReader); ok {
		t.Fatal("must not advertise unsupported capabilities")
	}
	if _, ok := c.(courier.CustomerIncidentReporter); ok {
		t.Fatal("must not advertise reporting")
	}
}

// create_order response: integer cod_amount, microsecond timestamps, recipient
// fields and a note.
func TestCreateShipmentResponseShape(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":200,"message":"Consignment has been created successfully.","consignment":{"consignment_id":1424107,"invoice":"Aa12-das4","tracking_code":"15BAEB8A","recipient_name":"John Smith","recipient_phone":"01234567890","recipient_address":"Fla#A1, House#17/1, Road#3/A, Dhanmondi, Dhaka-1209","cod_amount":1060,"status":"in_review","note":"Deliver within 3PM","created_at":"2021-03-21T07:05:31.000000Z","updated_at":"2021-03-21T07:05:31.000000Z"}}`))
	})
	sh, err := global(t, srv.URL).CreateShipment(context.Background(), req("Aa12-das4", "1060"))
	if err != nil {
		t.Fatal(err)
	}
	if sh.Ref.ID != "1424107" || sh.TrackingCode != "15BAEB8A" || sh.MerchantOrderID != "Aa12-das4" || sh.Status != courier.StatusCreated {
		t.Fatalf("%+v", sh)
	}
	if sh.COD == nil || sh.COD.Amount.String() != "1060" {
		t.Fatalf("cod %v", sh.COD)
	}
	if sh.CreatedAt == nil || sh.CreatedAt.UTC().Format("2006-01-02T15:04:05") != "2021-03-21T07:05:31" || sh.UpdatedAt == nil {
		t.Fatalf("timestamps %v %v", sh.CreatedAt, sh.UpdatedAt)
	}
}

// Bulk error body: wrapped in "data", padded values, literal "null" strings and
// a null consignment_id.
func TestBulkErrorWrapped(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"invoice":"SFC-1234j56","recipient_name":" John Smith ","recipient_phone":"+88012345678","recipient_address":"Dhanmondi, Zigatola","cod_amount":"3010","note":"null","consignment_id":null,"tracking_code":" null ","status":" error "}]}`))
	})
	res, err := global(t, srv.URL).CreateShipments(context.Background(), []courier.CreateShipmentRequest{req("SFC-1234j56", "3010")})
	if err == nil || res == nil || len(res.Items) != 1 {
		t.Fatalf("res=%v err=%v", res, err)
	}
	it := res.Items[0]
	if it.Status != courier.BatchFailed || it.Shipment != nil || !courier.IsKind(it.Err, courier.KindRejected) {
		t.Fatalf("item %+v", it)
	}
}

// Bulk success body (wrapped in "data", per-item status "success", string
// cod_amount, numeric consignment_id).
func TestBulkSuccessWrapped(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":200,"message":"We have a response for you.","data":[{"invoice":"SFC-1234j56","recipient_name":" John Smith ","recipient_phone":"+88012345678","recipient_address":"Dhanmondi, Zigatola","cod_amount":"3010","status":"success","note":"Hello World","consignment_id":106833902,"tracking_code":"65E27EE263A"},{"invoice":"SFC-12i3456","recipient_name":" John Smith ","recipient_phone":"+88012345678","recipient_address":"Dhanmondi, Zigatola","cod_amount":"3010","status":"success","note":"Hello World","consignment_id":106833903,"tracking_code":"65E27EFFEC0"}]}`))
	})
	res, err := global(t, srv.URL).CreateShipments(context.Background(), []courier.CreateShipmentRequest{req("SFC-1234j56", "3010"), req("SFC-12i3456", "3010")})
	if err != nil || len(res.Items) != 2 {
		t.Fatalf("res=%v err=%v", res, err)
	}
	for i, id := range []string{"106833902", "106833903"} {
		it := res.Items[i]
		if it.Status != courier.BatchSucceeded || it.Shipment == nil || it.Shipment.Ref.ID != courier.ShipmentID(id) || it.Shipment.Status != courier.StatusCreated || it.Shipment.ProviderStatus.Code != "success" {
			t.Fatalf("item %d %+v", i, it)
		}
		if it.Shipment.COD == nil || it.Shipment.COD.Amount.String() != "3010" {
			t.Fatalf("cod %v", it.Shipment.COD)
		}
	}
}

// Bulk success body as a bare array with "0.00" COD and a null note. Invoices
// must be unique because items are matched by invoice.
func TestBulkBareArraySuccess(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"invoice":"230822-1","recipient_name":"John Doe","recipient_address":"House 44, Road 2/A, Dhanmondi, Dhaka 1209","recipient_phone":"0171111111","cod_amount":"0.00","note":null,"consignment_id":11543968,"tracking_code":"B025A038","status":"success"},{"invoice":"230822-2","recipient_name":"John Doe","recipient_address":"House 44, Road 2/A","recipient_phone":"0171111111","cod_amount":"0.00","note":null,"consignment_id":11543969,"tracking_code":"B025A1DC","status":"success"}]`))
	})
	res, err := global(t, srv.URL).CreateShipments(context.Background(), []courier.CreateShipmentRequest{req("230822-1", ""), req("230822-2", "")})
	if err != nil || len(res.Items) != 2 {
		t.Fatalf("res=%v err=%v", res, err)
	}
	for i, id := range []string{"11543968", "11543969"} {
		it := res.Items[i]
		if it.Status != courier.BatchSucceeded || it.Shipment.Ref.ID != courier.ShipmentID(id) || it.Shipment.Status != courier.StatusCreated {
			t.Fatalf("item %d %+v", i, it)
		}
		if it.Shipment.COD == nil || !it.Shipment.COD.Amount.IsZero() {
			t.Fatalf("explicit 0.00 COD must be a real zero: %v", it.Shipment.COD)
		}
	}
}

// Bulk error body: "data" wrapper, real JSON nulls, status "error".
func TestBulkErrorWithRealNulls(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"invoice":"230822-1","recipient_name":"John Doe","recipient_address":"House 44, Road 2/A, Dhanmondi, Dhaka 1209","recipient_phone":"0171111111","cod_amount":"0.00","note":null,"consignment_id":null,"tracking_code":null,"status":"error"}]}`))
	})
	res, err := global(t, srv.URL).CreateShipments(context.Background(), []courier.CreateShipmentRequest{req("230822-1", "")})
	if err == nil || res == nil || len(res.Items) != 1 {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if it := res.Items[0]; it.Status != courier.BatchFailed || it.Shipment != nil || !courier.IsKind(it.Err, courier.KindRejected) {
		t.Fatalf("item %+v", it)
	}
}

func TestBulkSendsArrayAndMatchesByPosition(t *testing.T) {
	var sent []orderRequest
	var raw map[string]json.RawMessage
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &raw)
		var br bulkRequest
		_ = json.Unmarshal(b, &br)
		sent = br.Data
		// the guide's standard bulk answer: input order, JSON-encoded error codes, status null on failure
		_, _ = w.Write([]byte(`{"status":200,"message":"We have a response for you.","data":[{"invoice":"ORD-10231","consignment_id":1424107,"tracking_code":"15BAEB8A","status":"success","tracking_link":"https://x/tl/a1"},{"invoice":"ORD-10232","consignment_id":null,"tracking_code":null,"status":null,"error":"[\"THIS_INVOICE_ALREADY_EXISTS\"]"}]}`))
	})
	res, err := global(t, srv.URL).CreateShipments(context.Background(), []courier.CreateShipmentRequest{req("ORD-10231", "1060"), req("ORD-10232", "450")})
	if err == nil || res == nil || len(res.Items) != 2 {
		t.Fatalf("a failed item must surface an error: res=%v err=%v", res, err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw["data"])), "[") || len(sent) != 2 {
		t.Fatalf("data must be a JSON array, not an encoded string: %s", raw["data"])
	}
	if res.Items[0].Status != courier.BatchSucceeded || res.Items[0].Shipment.Ref.ID != "1424107" {
		t.Fatalf("%+v", res.Items[0])
	}
	f := res.Items[1]
	if f.Status != courier.BatchFailed || f.Shipment != nil || !courier.IsKind(f.Err, courier.KindRejected) || !strings.Contains(f.Err.Error(), "THIS_INVOICE_ALREADY_EXISTS") {
		t.Fatalf("%+v", f)
	}
	// a response that disagrees with the order sent is not guessed at
	srv2, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":200,"data":[{"invoice":"OTHER","consignment_id":5,"tracking_code":"T","status":"success"},{"invoice":"ORD-2","consignment_id":6,"tracking_code":"U","status":"success"}]}`))
	})
	res, err = global(t, srv2.URL).CreateShipments(context.Background(), []courier.CreateShipmentRequest{req("ORD-1", ""), req("ORD-2", "")})
	if err == nil || res.Items[0].Status == courier.BatchSucceeded || res.Items[1].Status != courier.BatchSucceeded {
		t.Fatalf("mismatched invoice must not be accepted as ORD-1: %+v err=%v", res.Items, err)
	}
}

func TestBulkExtendedStyleErrorsAreSentences(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":200,"message":"Processed bulk order entries.","data":[{"invoice":"ORD-1","status":"error","error":["The recipient phone format is invalid."],"consignment_id":null,"tracking_code":null}]}`))
	})
	res, err := global(t, srv.URL).CreateShipments(context.Background(), []courier.CreateShipmentRequest{req("ORD-1", "")})
	if err == nil || res.Items[0].Status != courier.BatchFailed || !strings.Contains(res.Items[0].Err.Error(), "recipient phone format is invalid") {
		t.Fatalf("%+v err=%v", res.Items[0], err)
	}
}

func TestCODAboveTheLimitIsRejectedLocally(t *testing.T) {
	var hits int
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) { hits++ })
	if _, err := global(t, srv.URL).CreateShipment(context.Background(), req("ORD-1", "1000001")); !errors.Is(err, courier.ErrValidation) || hits != 0 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
}
