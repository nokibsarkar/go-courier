package ecourier

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
	"github.com/nokibsarkar/go-courier/httpx"
)

var creds = Credentials{APIKey: "key-1", APISecret: "sec-1", UserID: "user-1"}

type srv struct {
	*httptest.Server
	hits atomic.Int32
}

func newSrv(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body string)) *srv {
	t.Helper()
	s := &srv{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		h(w, r, string(b))
	}))
	t.Cleanup(s.Close)
	return s
}

func global(t *testing.T, url string, opts ...Option) *Client {
	t.Helper()
	c, err := New(append([]Option{WithCredentials(creds), WithBaseURL(url)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func req() courier.CreateShipmentRequest {
	cod, _ := courier.ParseMoney("1500", courier.BDT)
	return courier.CreateShipmentRequest{
		MerchantOrderID: "ORD-1",
		Recipient: courier.Party{
			Contact: courier.Contact{Name: "Debashis", Phone: "01834-795469"},
			Address: courier.Address{Text: "Full Address", City: "Dhaka", Area: "Badda", PostalCode: "1212",
				Refs: []courier.LocationRef{{Provider: ProviderID, Kind: "thana", ID: "Badda"}}},
		},
		COD: &cod, Note: "Please handle carefully", Package: courier.Package{Quantity: 3},
	}
}

func ctxOpts() context.Context {
	return ContextWithOrderOptions(context.Background(), OrderOptions{PackageCode: "#2431"})
}

func TestEnvironmentMustBeExplicit(t *testing.T) {
	if _, err := New(WithCredentials(creds)); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("%v", err)
	}
	if _, err := NewTenant(); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("%v", err)
	}
	c, err := New(WithCredentials(creds), WithSandbox())
	if err != nil || c.baseURL != SandboxURL {
		t.Fatalf("%v", err)
	}
	if c, _ := New(WithCredentials(creds), WithProduction()); c.baseURL != ProductionURL {
		t.Fatal("production url")
	}
}

func TestCreateOrderBodyHeadersAndMapping(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	var path string
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, b string) {
		hdr, path = r.Header.Clone(), r.URL.Path
		dec := json.NewDecoder(strings.NewReader(b))
		dec.UseNumber()
		_ = dec.Decode(&body)
		_, _ = w.Write([]byte(`{"errors":[],"response_code":200,"message":"Order Submitted","ID":"ECR1030102170392"}`))
	})
	sh, err := global(t, s.URL).CreateShipment(ctxOpts(), req())
	if err != nil {
		t.Fatal(err)
	}
	if path != "/order-place" || hdr.Get("Api-Key") != "key-1" || hdr.Get("Api-Secret") != "sec-1" || hdr.Get("User-Id") != "user-1" || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("%s %v", path, hdr)
	}
	want := map[string]string{"recipient_mobile": "01834795469", "recipient_city": "Dhaka", "recipient_thana": "Badda", "recipient_area": "Badda",
		"recipient_zip": "1212", "package_code": "#2431", "payment_method": "COD", "product_id": "ORD-1", "comments": "Please handle carefully"}
	for k, v := range want {
		if fmt.Sprint(body[k]) != v {
			t.Errorf("%s = %v want %s", k, body[k], v)
		}
	}
	if fmt.Sprint(body["product_price"]) != "1500" || fmt.Sprint(body["number_of_item"]) != "3" {
		t.Fatalf("%v", body)
	}
	if sh.Ref.ID != "ECR1030102170392" || sh.TrackingCode != "ECR1030102170392" || sh.MerchantOrderID != "ORD-1" || sh.Ref.Account != "user-1" ||
		sh.COD == nil || sh.COD.Amount.String() != "1500" || sh.CreatedAt != nil {
		t.Fatalf("%+v", sh)
	}
}

func TestOrderValidationBeforeAnyHTTP(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {})
	c := global(t, s.URL)
	mod := func(f func(*courier.CreateShipmentRequest)) courier.CreateShipmentRequest {
		r := req()
		f(&r)
		return r
	}
	frac, _ := courier.ParseMoney("10.50", courier.BDT)
	usd, _ := courier.ParseMoney("10", "USD")
	zero, _ := courier.ParseMoney("0", courier.BDT)
	cases := map[string]courier.CreateShipmentRequest{
		"fractional cod": mod(func(r *courier.CreateShipmentRequest) { r.COD = &frac }),
		"usd cod":        mod(func(r *courier.CreateShipmentRequest) { r.COD = &usd }),
		"zero cod":       mod(func(r *courier.CreateShipmentRequest) { r.COD = &zero }),
		"no city":        mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Address.City = "" }),
		"no area":        mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Address.Area = "" }),
		"no zip":         mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Address.PostalCode = "" }),
		"no thana":       mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Address.Refs = nil }),
		"foreign phone":  mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Contact.Phone = "+14155550123" }),
		"long name":      mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Contact.Name = strings.Repeat("n", 251) }),
		"long product": mod(func(r *courier.CreateShipmentRequest) {
			r.MerchantOrderID = courier.MerchantOrderID(strings.Repeat("p", 21))
		}),
		"long city": mod(func(r *courier.CreateShipmentRequest) { r.Recipient.Address.City = strings.Repeat("c", 41) }),
	}
	for name, r := range cases {
		if _, err := c.CreateShipment(ctxOpts(), r); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	// the missing thana must be an ambiguity error (no guessing)
	if _, err := c.CreateShipment(ctxOpts(), cases["no thana"]); !errors.Is(err, courier.ErrAmbiguous) {
		t.Errorf("thana: %v", err)
	}
	// package code required; payment method required without COD
	if _, err := c.CreateShipment(context.Background(), req()); !errors.Is(err, courier.ErrValidation) {
		t.Errorf("package code: %v", err)
	}
	noCOD := req()
	noCOD.COD = nil
	if _, err := c.CreateShipment(ctxOpts(), noCOD); !errors.Is(err, courier.ErrValidation) {
		t.Errorf("payment method must not be assumed: %v", err)
	}
	if s.hits.Load() != 0 {
		t.Fatalf("%d HTTP calls for invalid input", s.hits.Load())
	}
}

func TestNonCODPaymentUsesDeclaredValue(t *testing.T) {
	var body map[string]any
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, b string) {
		_ = json.Unmarshal([]byte(b), &body)
		_, _ = w.Write([]byte(`{"errors":[],"response_code":200,"message":"Order Submitted","ID":"ECR1"}`))
	})
	r := req()
	r.COD = nil
	dv, _ := courier.ParseMoney("1200", courier.BDT)
	r.Package.DeclaredValue = &dv
	ctx := ContextWithOrderOptions(context.Background(), OrderOptions{PackageCode: "#1", PaymentMethod: "mpay", IsFragile: true})
	if _, err := global(t, s.URL).CreateShipment(ctx, r); err != nil {
		t.Fatal(err)
	}
	if body["payment_method"] != "MPAY" || fmt.Sprint(body["product_price"]) != "1200" || fmt.Sprint(body["is_fragile"]) != "1" {
		t.Fatalf("%v", body)
	}
	bad := ContextWithOrderOptions(context.Background(), OrderOptions{PackageCode: "#1", PaymentMethod: "POS", IsIPay: true})
	if _, err := global(t, s.URL).CreateShipment(bad, r); err == nil {
		t.Fatal("iPay requires COD")
	}
}

func TestOrderErrorsEmbeddedAndHTTP(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   *courier.Error
	}{
		{400, `{"errors":["Invalid Request","Duplicate product id and mobile supplied"],"response_code":400}`, courier.ErrConflict},
		{400, `{"errors":["Invalid Request","recipient_name Parameter either missing or not set correctly. "],"response_code":400}`, courier.ErrValidation},
		{401, `{"errors":["Unauthorized Access"],"response_code":401}`, courier.ErrCredentials},
		{200, `{"errors":["Invalid Package","The Package code does not exist or assigned to you"],"response_code":400}`, courier.ErrValidation},
		{500, `{"errors":["Unable to insert orders"],"response_code":500}`, courier.ErrIndeterminate},
	}
	for _, tc := range cases {
		tc := tc
		s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := global(t, s.URL).CreateShipment(ctxOpts(), req())
		if !errors.Is(err, tc.want) {
			t.Errorf("%s -> %v", tc.body, err)
		}
	}
}

func TestCreateNeverReplayedAndIndeterminate(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(503) })
	c := global(t, s.URL, WithRetry(httpx.RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}))
	if _, err := c.CreateShipment(ctxOpts(), req()); !courier.IsIndeterminate(err) || s.hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, s.hits.Load())
	}
	s2 := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"response_code":200,"message":"Order Submitted"}`))
	})
	if _, err := global(t, s2.URL).CreateShipment(ctxOpts(), req()); !courier.IsIndeterminate(err) {
		t.Fatalf("a success without an order ID is indeterminate: %v", err)
	}
	s3 := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(`{nope`)) })
	if _, err := global(t, s3.URL).CreateShipment(ctxOpts(), req()); !courier.IsIndeterminate(err) {
		t.Fatalf("malformed 200: %v", err)
	}
	s4 := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(ctxOpts(), 40*time.Millisecond)
	defer cancel()
	if _, err := global(t, s4.URL).CreateShipment(ctx, req()); !courier.IsIndeterminate(err) {
		t.Fatalf("timeout: %v", err)
	}
}

const trackFound = `{"response_code":200,"message":"Success","query_data":[{"status":[["Delivered","","2017-01-20 12:04:55"],["On the way to Delivery","Status has been changed","2017-01-20 12:03:39"],["In Transit","Status has been changed","2017-01-20 12:03:25"],["Picked Up","Status has been changed","2017-01-20 12:01:37"],["Initiated","An order has been made by ecommerce","2017-01-20 12:00:09"]],"REFID":"ECR1022001175478","product_id":"1234","paymentmethod":"COD","paymentclear":"Clear","shipping_price":"60","cod":100}]}`

func TestTrackingHistoryPreservedAndCurrentStatusFromTimestamps(t *testing.T) {
	var body map[string]string
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, b string) {
		body = nil
		_ = json.Unmarshal([]byte(b), &body)
		_, _ = w.Write([]byte(trackFound))
	})
	c := global(t, s.URL)
	res, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "ECR1022001175478"})
	if err != nil {
		t.Fatal(err)
	}
	if body["ecr"] != "ECR1022001175478" || res.Status != courier.StatusDelivered || res.ProviderStatus.Code != "Delivered" || res.Coverage != courier.CoverageFull ||
		len(res.Events) != 5 || res.Events[0].ProviderStatus.Code != "Delivered" || res.Events[4].Status != courier.StatusCreated || res.Delivery == nil {
		t.Fatalf("%+v", res)
	}
	for _, e := range res.Events {
		if e.OccurredAt == nil {
			t.Fatal("timestamps without a timezone must use Asia/Dhaka")
		}
		if len(e.Raw) == 0 {
			t.Fatal("raw entry must be kept")
		}
	}
	// Explicit location override
	dhaka := time.FixedZone("BDT", 6*3600)
	res, _ = global(t, s.URL, WithEventLocation(dhaka)).TrackShipment(context.Background(), courier.TrackingRequest{Code: "ECR1022001175478"})
	if res.Events[0].OccurredAt == nil || res.Events[0].OccurredAt.Format(time.RFC3339) != "2017-01-20T06:04:55Z" || res.Delivery.DeliveredAt == nil {
		t.Fatalf("%+v", res.Events[0])
	}
	// merchant order lookup sends product_id
	_, _ = c.TrackShipment(context.Background(), courier.TrackingRequest{Lookups: []courier.Lookup{{Type: courier.LookupMerchantOrderID, Value: "1234"}}})
	if body["product_id"] != "1234" || body["ecr"] != "" {
		t.Fatalf("%v", body)
	}
}

func TestTrackingEdgeCases(t *testing.T) {
	one := func(resp string) error {
		s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(resp)) })
		_, err := global(t, s.URL).TrackShipment(context.Background(), courier.TrackingRequest{Code: "ECR1"})
		return err
	}
	if err := one(`{"response_code":200,"message":"Success","query_data":"No Data Found"}`); !errors.Is(err, courier.ErrNotFound) {
		t.Errorf("no data: %v", err)
	}
	two := `{"response_code":200,"query_data":[{"REFID":"ECR1","status":[["Initiated","","2020-01-01 00:00:00"]]},{"REFID":"ECR2","status":[["Initiated","","2020-01-01 00:00:00"]]}]}`
	if err := one(two); !errors.Is(err, courier.ErrAmbiguous) {
		t.Errorf("several parcels: %v", err)
	}
	if err := one(`{"response_code":200,"query_data":[{"REFID":"ECR1","status":[[null,"x","2020-01-01 00:00:00"]]}]}`); !errors.Is(err, courier.ErrProtocol) {
		t.Errorf("malformed status entry must not be skipped: %v", err)
	}
	if err := one(`{"response_code":200,"query_data":[{"status":[]}]}`); !errors.Is(err, courier.ErrProtocol) {
		t.Errorf("no REFID: %v", err)
	}
	if err := one(`{"errors":["Invalid Data","Please Set product id or ecr number or both"],"response_code":400}`); !errors.Is(err, courier.ErrValidation) {
		t.Errorf("400: %v", err)
	}
	// unknown status stays unknown; no events -> unknown coverage
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"response_code":200,"query_data":[{"REFID":"ECR1","status":[["Held at Customs","","2020-01-01 00:00:00"]]}]}`))
	})
	res, err := global(t, s.URL).TrackShipment(context.Background(), courier.TrackingRequest{Code: "ECR1"})
	if err != nil || res.Status != courier.StatusUnknown || res.ProviderStatus.Code != "Held at Customs" || res.Delivery != nil {
		t.Fatalf("%+v %v", res, err)
	}
	// local validation
	if _, err := global(t, s.URL).TrackShipment(context.Background(), courier.TrackingRequest{Code: "bad code!"}); !errors.Is(err, courier.ErrValidation) {
		t.Errorf("code: %v", err)
	}
	if _, err := global(t, s.URL).TrackShipment(context.Background(), courier.TrackingRequest{Lookups: []courier.Lookup{{Type: courier.LookupPhone, Value: "0171"}}}); !errors.Is(err, courier.ErrUnsupported) {
		t.Errorf("phone lookup: %v", err)
	}
}

func TestTrackingPOSTIsNotAutoRetried(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(503) })
	c := global(t, s.URL, WithRetry(httpx.RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}))
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "ECR1"}); err == nil || s.hits.Load() != 1 {
		t.Fatalf("hits=%d err=%v (TRK-13)", s.hits.Load(), err)
	}
}

func TestTrackerConformance(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, b string) {
		if strings.Contains(b, "BAD") {
			_, _ = w.Write([]byte(`{"response_code":200,"query_data":[{"REFID":"BAD","status":[[null,"","x"]]}]}`))
			return
		}
		_, _ = w.Write([]byte(trackFound))
	})
	bad := courier.TrackingRequest{Code: "BAD"}
	contracttest.RunShipmentTracker(t, contracttest.TrackerSuite{
		Tracker: global(t, s.URL), MalformedRequest: &bad,
		Cases: []contracttest.TrackerCase{{Name: "history", Request: courier.TrackingRequest{Code: "ECR1022001175478"}}},
	})
}

func TestCancelOutcomes(t *testing.T) {
	var body map[string]string
	do := func(resp string) error {
		s := newSrv(t, func(w http.ResponseWriter, r *http.Request, b string) {
			_ = json.Unmarshal([]byte(b), &body)
			_, _ = w.Write([]byte(resp))
		})
		return global(t, s.URL).CancelShipment(context.Background(), courier.ShipmentRef{ID: "ECR1"})
	}
	if err := do(`{"success":"true","message":"Order canceled"}`); err != nil || body["tracking"] != "ECR1" || body["comment"] != "Cancelled by merchant" {
		t.Fatalf("%v %v", err, body)
	}
	if err := do(`{"success":true,"message":"Order canceled"}`); err != nil {
		t.Fatalf("bool success: %v", err)
	}
	if err := do(`{"success":"false","message":" Order not canceled"}`); !errors.Is(err, courier.ErrConflict) {
		t.Fatalf("not canceled: %v", err)
	}
	if err := do(`{"success":"false","message":"Invalid Tracking"}`); !errors.Is(err, courier.ErrNotFound) {
		t.Fatalf("invalid tracking: %v", err)
	}
	if err := do(`{"message":"ok"}`); !courier.IsIndeterminate(err) {
		t.Fatalf("no verdict must be indeterminate: %v", err)
	}
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(502) })
	if err := global(t, s.URL).CancelShipment(context.Background(), courier.ShipmentRef{ID: "ECR1"}); !courier.IsIndeterminate(err) || s.hits.Load() != 1 {
		t.Fatalf("502: %v", err)
	}
	c := global(t, s.URL)
	for _, e := range []error{c.CancelOrder(context.Background(), "bad id", "x"), c.CancelOrder(context.Background(), "ECR1", ""),
		c.CancelOrder(context.Background(), "ECR1", strings.Repeat("x", 256)), c.CancelShipment(context.Background(), courier.ShipmentRef{Provider: "pathao", ID: "1"})} {
		if !errors.Is(e, courier.ErrValidation) {
			t.Errorf("%v", e)
		}
	}
	if _, err := New(WithCredentials(creds), WithBaseURL("http://127.0.0.1"), WithCancelComment(strings.Repeat("x", 256))); err == nil {
		t.Fatal("long cancel comment")
	}
}

func TestLabelReturnsURLWithoutFetching(t *testing.T) {
	var external atomic.Int32
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { external.Add(1) }))
	defer ext.Close()
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"success":true,"response_code":200,"query_data":"https://staging.ecourier.com.bd/invoices/print_slip1.pdf"}`))
	})
	doc, err := global(t, s.URL).GetShipmentLabel(context.Background(), courier.ShipmentRef{ID: "ECR1"})
	if err != nil || doc.URL != "https://staging.ecourier.com.bd/invoices/print_slip1.pdf" || doc.Body != nil || doc.ExpiresAt != nil || external.Load() != 0 {
		t.Fatalf("%+v %v", doc, err)
	}
	for resp, want := range map[string]*courier.Error{
		`{"success":false,"response_code":200,"query_data":"Parcel not found"}`:             courier.ErrNotFound,
		`{"success":true,"response_code":200,"query_data":"http://insecure.example/x.pdf"}`: courier.ErrProtocol,
		`{"success":true,"response_code":200,"query_data":""}`:                              courier.ErrProtocol,
	} {
		resp := resp
		s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte(resp)) })
		if _, err := global(t, s.URL).GetShipmentLabel(context.Background(), courier.ShipmentRef{ID: "ECR1"}); !errors.Is(err, want) {
			t.Errorf("%s: %v", resp, err)
		}
	}
}

func TestFraudStatusCheck(t *testing.T) {
	var body map[string]string
	respond := func(resp string) (*courier.CustomerRisk, error) {
		s := newSrv(t, func(w http.ResponseWriter, r *http.Request, b string) {
			_ = json.Unmarshal([]byte(b), &body)
			_, _ = w.Write([]byte(resp))
		})
		return global(t, s.URL).GetCustomerRisk(context.Background(), courier.CustomerRiskRequest{Phone: "+880 1711-111111"})
	}
	r, err := respond(`{"success":true,"response_code":200,"customer_status":"Warning!","message":["Reason of fraud"]}`)
	if err != nil || r.Assessment != "Warning!" || body["number"] != "01711111111" || r.Counts != nil || r.Rating != nil || r.Source != ProviderID {
		t.Fatalf("%+v %v %v (no counts or score may be invented)", r, err, body)
	}
	if r, err = respond(`{"success":true,"response_code":200,"customer_status":"New Customer for eCourier","message":""}`); err != nil || r.Assessment != "New Customer for eCourier" {
		t.Fatalf("%+v %v", r, err)
	}
	for resp, want := range map[string]*courier.Error{
		`{"success":false,"response_code":203,"message":"Invalid number. Please provide a valid number."}`:             courier.ErrValidation,
		`{"success":false,"response_code":203,"message":"Please contact with eCourier support to enjoy this service"}`: courier.ErrPermission,
		`{"success":false,"response_code":203,"message":" Insufficient COD Balance"}`:                                  courier.ErrRejected,
		`{"success":true,"response_code":200,"message":""}`:                                                            courier.ErrProtocol,
	} {
		if _, err := respond(resp); !errors.Is(err, want) {
			t.Errorf("%s -> %v", resp, err)
		}
	}
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(503) })
	c := global(t, s.URL, WithRetry(httpx.RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}))
	if _, err := c.GetCustomerRisk(context.Background(), courier.CustomerRiskRequest{Phone: "01711111111"}); err == nil || s.hits.Load() != 1 {
		t.Fatalf("billable lookup must not be retried: hits=%d", s.hits.Load())
	}
	if _, err := c.GetCustomerRisk(context.Background(), courier.CustomerRiskRequest{Phone: "12345"}); err == nil {
		t.Fatal("bad phone")
	}
}

func TestNativeListsAndPaymentStatus(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, b string) {
		switch r.URL.Path {
		case "/packages":
			_, _ = w.Write([]byte(`[{"package_name":"SAMEDAY_Inside_120tk","package_code":"#2505","shipping_charge":"120","weight":"Up To 500gm","delivery_time":"Same Day(8Hr)","coverage_id":"1","coverage":"Inside Dhaka"},{"package_name":"X","package_code":"#9"}]`))
		case "/city-list":
			_, _ = w.Write([]byte(`[{"name":"Dhaka","value":"Dhaka"}]`))
		case "/thana-list":
			if !strings.Contains(b, "Barishal") {
				t.Errorf("body %s", b)
			}
			_, _ = w.Write([]byte(`{"success":true,"message":[{"name":"Agailjhara","value":"Agailjhara"}]}`))
		case "/area-by-district":
			_, _ = w.Write([]byte(`{"success":true,"data":[{"id":2,"hub_id":1,"post_code":"1212","name":"Gulshan-2"}]}`))
		case "/hub-info-by-area":
			_, _ = w.Write([]byte(`{"success":true,"message":"Hub details","data":{"hub_name":"Badda Hub","hub_id":18490,"hub_mobile":"01402876962","address":null,"latitude":"23.56456","longitude":"90.4344"}}`))
		case "/payment-status":
			_, _ = w.Write([]byte(`{"success":true,"message":{"payment_id":20,"payment_mode":"dbbl","collected_amount":500,"payment_date":"2020-12-07 18:05:18","shipping_charge":130,"booking_price":8,"fragile_price":0,"cash_on_delivery":148}}`))
		}
	})
	c := global(t, s.URL)
	ctx := context.Background()
	pk, err := c.ListPackages(ctx)
	if err != nil || len(pk) != 2 || pk[0].ShippingCharge == nil || pk[0].ShippingCharge.Amount.String() != "120" || pk[1].ShippingCharge != nil || pk[0].CoverageID != "1" {
		t.Fatalf("%+v %v", pk, err)
	}
	if v, err := c.ListCities(ctx); err != nil || len(v) != 1 || v[0].Value != "Dhaka" {
		t.Fatalf("%v %v", v, err)
	}
	if v, err := c.ListThanas(ctx, "Barishal"); err != nil || len(v) != 1 {
		t.Fatalf("%v %v", v, err)
	}
	if v, err := c.ListAreasByDistrict(ctx, "Dhaka"); err != nil || len(v) != 1 || v[0].HubID != "1" || v[0].PostCode != "1212" {
		t.Fatalf("%v %v", v, err)
	}
	h, err := c.HubByArea(ctx, "Gulshan-2")
	if err != nil || h.ID != "18490" || h.Address != nil || h.Latitude != "23.56456" {
		t.Fatalf("%+v %v", h, err)
	}
	ps, err := c.GetPaymentStatus(ctx, "ECR1")
	if err != nil || ps.PaymentID != "20" || ps.CollectedAmount.Amount.String() != "500" || ps.FragilePrice == nil || !ps.FragilePrice.Amount.IsZero() || ps.PaymentDate == "" {
		t.Fatalf("%+v %v", ps, err)
	}
	for _, f := range []func() error{
		func() error { _, e := c.ListThanas(ctx, ""); return e },
		func() error { _, e := c.ListAreasByDistrict(ctx, " "); return e },
		func() error { _, e := c.HubByArea(ctx, ""); return e },
		func() error { _, e := c.GetPaymentStatus(ctx, "no spaces"); return e },
	} {
		if !errors.Is(f(), courier.ErrValidation) {
			t.Error("want validation")
		}
	}
	s2 := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"success":false,"message":"Order not Found"}`))
	})
	if _, err := global(t, s2.URL).GetPaymentStatus(ctx, "ECR1"); !errors.Is(err, courier.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	s3 := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`[{"package_name":"no code"}]`))
	})
	if _, err := global(t, s3.URL).ListPackages(ctx); !errors.Is(err, courier.ErrProtocol) {
		t.Fatalf("malformed record must not be skipped: %v", err)
	}
}

func TestGlobalAndTenantModes(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`[{"name":"` + r.Header.Get("User-Id") + `","value":"v"}]`))
	})
	tctx := ContextWithCredentials(context.Background(), Credentials{APIKey: "t", APISecret: "t", UserID: "tenant"})
	g := global(t, s.URL)
	if _, err := g.ListCities(tctx); !errors.Is(err, courier.ErrAuthMode) || s.hits.Load() != 0 || g.Mode() != auth.ModeGlobal {
		t.Fatalf("global must reject ctx credentials without dispatch: %v", err)
	}
	t.Setenv("ECOURIER_API_KEY", "env")
	t.Setenv("ECOURIER_API_SECRET", "env")
	t.Setenv("ECOURIER_USER_ID", "env")
	tn, err := NewTenant(WithBaseURL(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tn.ListCities(context.Background()); !errors.Is(err, courier.ErrAuthMode) || s.hits.Load() != 0 {
		t.Fatalf("tenant must not use env credentials: %v", err)
	}
	if _, err := tn.ListCities(ContextWithCredentials(context.Background(), Credentials{})); err == nil || s.hits.Load() != 0 {
		t.Fatalf("empty tenant credentials: %v", err)
	}
	if _, err := NewTenant(WithBaseURL(s.URL), WithCredentials(creds)); err == nil {
		t.Fatal("tenant constructor must reject global credentials")
	}
	if c, err := New(WithBaseURL(s.URL)); err != nil { // env credentials
		t.Fatal(err)
	} else if v, err := c.ListCities(context.Background()); err != nil || v[0].Name != "env" {
		t.Fatalf("%v %v", v, err)
	}
	t.Setenv("ECOURIER_USER_ID", "")
	if _, err := New(WithBaseURL(s.URL)); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("missing env credentials must fail construction: %v", err)
	}
}

func TestConcurrentTenantsNeverLeak(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		k, sec, u := r.Header.Get("Api-Key"), r.Header.Get("Api-Secret"), r.Header.Get("User-Id")
		if strings.TrimPrefix(k, "k-") != strings.TrimPrefix(sec, "s-") || strings.TrimPrefix(k, "k-") != strings.TrimPrefix(u, "u-") {
			http.Error(w, "mixed", 500)
			return
		}
		_, _ = fmt.Fprintf(w, `[{"name":%q,"value":"v"}]`, u)
	})
	c, _ := NewTenant(WithBaseURL(s.URL))
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprint(i)
			ctx := ContextWithCredentials(context.Background(), Credentials{APIKey: auth.Secret("k-" + id), APISecret: auth.Secret("s-" + id), UserID: "u-" + id})
			v, err := c.ListCities(ctx)
			if err != nil || len(v) != 1 || v[0].Name != "u-"+id {
				t.Errorf("tenant %s: %v %v", id, v, err)
			}
		}()
	}
	wg.Wait()
}

func TestRedirectNeverForwardsCredentials(t *testing.T) {
	var leaked atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != "" {
			leaked.Store(true)
		}
	}))
	defer evil.Close()
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) { http.Redirect(w, r, evil.URL, 307) })
	if _, err := global(t, s.URL).ListCities(context.Background()); !errors.Is(err, courier.ErrProtocol) || leaked.Load() {
		t.Fatalf("%v leaked=%v", err, leaked.Load())
	}
}

func TestSecretsNeverInErrors(t *testing.T) {
	s := newSrv(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"errors":["Unauthorized Access"],"response_code":401}`))
	})
	_, err := global(t, s.URL).ListCities(context.Background())
	if err == nil || strings.Contains(err.Error(), "key-1") || strings.Contains(err.Error(), "sec-1") || strings.Contains(fmt.Sprintf("%+v", creds), "sec-1") {
		t.Fatalf("leak: %v", err)
	}
}

func TestCapabilitiesAndManifest(t *testing.T) {
	var c any = &Client{}
	for name, ok := range map[string]bool{
		"BatchShipmentCreator": isA[courier.BatchShipmentCreator](c), "ShipmentReader": isA[courier.ShipmentReader](c),
		"Quoter": isA[courier.Quoter](c), "WebhookHandlerProvider": isA[courier.WebhookHandlerProvider](c),
		"CustomerIncidentReporter": isA[courier.CustomerIncidentReporter](c), "BalanceReader": isA[courier.BalanceReader](c),
		"ProofOfDeliveryReader": isA[courier.ProofOfDeliveryReader](c),
	} {
		if ok {
			t.Errorf("must not advertise %s", name)
		}
	}
	m := Manifest()
	if m.Provider != ProviderID || m.Version != "5.5" || m.Capabilities["place_order"] != courier.VerifiedImplemented ||
		m.Capabilities["webhooks"] != courier.UnknownUnverified || len(m.Unverified) == 0 {
		t.Fatalf("%+v", m)
	}
}

func isA[T any](v any) bool { _, ok := v.(T); return ok }
