package redx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/shopspring/decimal"
)

func newTestClient(t *testing.T, h http.Handler, opts ...Option) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts = append([]Option{WithBaseURL(srv.URL), WithCredentials(Credentials{Token: "tok-1", AccountID: "acct"})}, opts...)
	c, err := New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func bdt(s string) *courier.Money {
	d := decimal.RequireFromString(s)
	m := courier.NewBDT(d)
	return &m
}

func w500() *uint32 { v := uint32(500); return &v }

func TestParseTimeDefaultsToAsiaDhaka(t *testing.T) {
	got := parseTime("2026-11-03 10:00:00")
	if got == nil || got.Location().String() != "Asia/Dhaka" || got.Hour() != 10 {
		t.Fatalf("got %v", got)
	}
}

func goodReq() courier.CreateShipmentRequest {
	return courier.CreateShipmentRequest{
		MerchantOrderID: "ACBD1234TEST",
		Pickup:          courier.Party{Address: courier.Address{Refs: []courier.LocationRef{{Provider: ProviderID, Kind: "pickup_store", ID: "1"}}}},
		Recipient: courier.Party{
			Contact: courier.Contact{Name: "Test Customer", Phone: "01987654321"},
			Address: courier.Address{Text: "Test Address", Area: "Mirpur DOHS", Refs: []courier.LocationRef{{Provider: ProviderID, Kind: "area", ID: "12"}}},
		},
		Package: courier.Package{WeightGrams: w500(), DeclaredValue: bdt("1000")},
		COD:     bdt("13293"),
		Note:    "ring twice",
	}
}

func TestCreateShipmentWireAndAuth(t *testing.T) {
	var got map[string]any
	var hdr string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Get("API-ACCESS-TOKEN")
		if r.Method != http.MethodPost || r.URL.Path != "/parcel" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"tracking_id":"20A312THJDJ8"}`))
	}))
	closed := true
	ctx := ContextWithOrderOptions(context.Background(), OrderOptions{IsClosedBox: &closed, Items: []ParcelItem{{Name: "Shirt", Category: "clothing", Value: *bdt("500")}}})
	sh, err := c.CreateShipment(ctx, goodReq())
	if err != nil {
		t.Fatal(err)
	}
	if hdr != "Bearer tok-1" {
		t.Fatalf("auth header %q", hdr)
	}
	if sh.Ref.ID != "20A312THJDJ8" || sh.Ref.Account != "acct" || sh.MerchantOrderID != "ACBD1234TEST" || sh.COD == nil || sh.COD.Amount.String() != "13293" || sh.Status != courier.StatusCreated {
		t.Fatalf("%+v", sh)
	}
	checks := map[string]any{"customer_name": "Test Customer", "customer_phone": "01987654321", "delivery_area": "Mirpur DOHS", "delivery_area_id": float64(12),
		"customer_address": "Test Address", "merchant_invoice_id": "ACBD1234TEST", "cash_collection_amount": "13293", "parcel_weight": float64(500),
		"value": float64(1000), "instruction": "ring twice", "is_closed_box": true, "pickup_store_id": float64(1)}
	for k, v := range checks {
		if got[k] != v {
			t.Errorf("%s = %#v, want %#v", k, got[k], v)
		}
	}
	if d, _ := got["parcel_details_json"].([]any); len(d) != 1 {
		t.Errorf("parcel_details_json %#v", got["parcel_details_json"])
	}
}

func TestCreateShipmentNoCODSendsZeroAndValidates(t *testing.T) {
	var got map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"tracking_id":"T1"}`))
	}))
	r := goodReq()
	r.COD = nil
	if _, err := c.CreateShipment(context.Background(), r); err != nil || got["cash_collection_amount"] != "0" {
		t.Fatalf("%v %v", err, got)
	}
	bad := func(name string, mut func(*courier.CreateShipmentRequest), kind courier.Kind) {
		t.Helper()
		var calls int32
		c2, _ := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&calls, 1) }))
		rr := goodReq()
		mut(&rr)
		_, err := c2.CreateShipment(context.Background(), rr)
		if err == nil || !courier.IsKind(err, kind) || atomic.LoadInt32(&calls) != 0 {
			t.Errorf("%s: err=%v calls=%d", name, err, calls)
		}
	}
	bad("no area ref", func(r *courier.CreateShipmentRequest) { r.Recipient.Address.Refs = nil }, courier.KindAmbiguous)
	bad("bad area ref", func(r *courier.CreateShipmentRequest) { r.Recipient.Address.Refs[0].ID = "x" }, courier.KindValidation)
	bad("no weight", func(r *courier.CreateShipmentRequest) { r.Package.WeightGrams = nil }, courier.KindValidation)
	bad("no declared value", func(r *courier.CreateShipmentRequest) { r.Package.DeclaredValue = nil }, courier.KindValidation)
	bad("fractional cod", func(r *courier.CreateShipmentRequest) { r.COD = bdt("10.5") }, courier.KindValidation)
	bad("foreign phone", func(r *courier.CreateShipmentRequest) { r.Recipient.Contact.Phone = "+14155550100" }, courier.KindValidation)
	bad("usd cod", func(r *courier.CreateShipmentRequest) {
		m, _ := courier.NewMoney(decimal.NewFromInt(5), "USD")
		r.COD = &m
	}, courier.KindValidation)
}

func TestCreateShipmentIndeterminate(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"unexpected":true}`))
	}))
	_, err := c.CreateShipment(context.Background(), goodReq())
	if err == nil || !courier.IsIndeterminate(err) {
		t.Fatalf("want indeterminate, got %v", err)
	}
	c2, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"message":"boom"}`, 503)
	}))
	if _, err := c2.CreateShipment(context.Background(), goodReq()); err == nil || !courier.IsIndeterminate(err) {
		t.Fatalf("5xx must be indeterminate: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("create replayed: %d calls", n)
	}
}

const infoBody = `{"parcel":{"tracking_id":"21A427TU4BN3R","customer_address":"Test Addtess","delivery_area":"Mirpur DOHS","delivery_area_id":12,"charge":60,"customer_name":"Test Customer","customer_phone":"01987654321","cash_collection_amount":13293,"parcel_weight":500,"merchant_invoice_id":"ACBD1234TEST","status":"delivery-in-progress","instruction":"","created_at":"2021-04-27T08:29:14.000Z","delivery_type":"regular","value":"0"}}`
const trackBodyJSON = `{"tracking":[{"message_en":"Package is created successfully","message_bn":"x","time":"2020-02-04T21:19:41.000Z"},{"message_en":"Package is picked up","message_bn":"y","time":"2020-02-05T11:41:03.000Z"}]}`

func trackServer(t *testing.T, calls *int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		switch r.URL.Path {
		case "/parcel/info/21A427TU4BN3R":
			_, _ = w.Write([]byte(infoBody))
		case "/parcel/track/21A427TU4BN3R":
			_, _ = w.Write([]byte(trackBodyJSON))
		default:
			http.Error(w, `{"message":"not found"}`, 404)
		}
	})
}

func TestTrackShipmentCombinesInfoAndHistory(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, trackServer(t, &calls))
	res, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "21A427TU4BN3R"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != courier.StatusOutForDelivery || res.ProviderStatus.Code != "delivery-in-progress" || res.Coverage != courier.CoverageFull || res.COD != courier.CODUnknown || len(res.Events) != 2 {
		t.Fatalf("%+v", res)
	}
	if res.Events[0].OccurredAt == nil || res.Events[0].OccurredAt.UTC().Format(time.RFC3339) != "2020-02-04T21:19:41Z" || res.Events[0].Status != courier.StatusUnknown || res.Events[1].Description != "Package is picked up" {
		t.Fatalf("%+v", res.Events)
	}
	if res.Ref.Account != "acct" || res.Delivery != nil {
		t.Fatalf("%+v", res)
	}
	// Lookup types, unsupported merchant-order lookup, invalid id.
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Lookups: []courier.Lookup{{Type: courier.LookupTrackingCode, Value: "21A427TU4BN3R"}}}); err != nil {
		t.Fatal(err)
	}
	_, err = c.TrackShipment(context.Background(), courier.TrackingRequest{Lookups: []courier.Lookup{{Type: courier.LookupMerchantOrderID, Value: "X"}}})
	if !courier.IsKind(err, courier.KindUnsupported) {
		t.Fatalf("%v", err)
	}
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "../x"}); !courier.IsKind(err, courier.KindValidation) {
		t.Fatalf("%v", err)
	}
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "NOPE"}); !courier.IsKind(err, courier.KindNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestTrackShipmentUnknownStatusAndEmptyHistory(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/parcel/info/") {
			_, _ = w.Write([]byte(`{"parcel":{"tracking_id":"T","status":"paid"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"tracking":[]}`))
	}))
	res, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "T"})
	if err != nil || res.Status != courier.StatusUnknown || res.ProviderStatus.Code != "paid" || res.Coverage != courier.CoverageCurrentOnly {
		t.Fatalf("%v %+v", err, res)
	}
	c2, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"parcel":null}`)) }))
	if _, err := c2.TrackShipment(context.Background(), courier.TrackingRequest{Code: "T"}); !courier.IsKind(err, courier.KindProtocol) {
		t.Fatalf("%v", err)
	}
}

func TestTrackingRetriesReadsButNeverCreate(t *testing.T) {
	var gets, posts int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if atomic.AddInt32(&gets, 1) == 1 {
				http.Error(w, "x", 503)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/parcel/info/") {
				_, _ = w.Write([]byte(infoBody))
			} else {
				_, _ = w.Write([]byte(trackBodyJSON))
			}
			return
		}
		atomic.AddInt32(&posts, 1)
		http.Error(w, "x", 503)
	}), WithRetry(retryFast()))
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "21A427TU4BN3R"}); err != nil {
		t.Fatalf("GET should be retried: %v", err)
	}
	_, _ = c.CreateShipment(context.Background(), goodReq())
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("POST replayed %d times", posts)
	}
}

func TestCancel(t *testing.T) {
	var got map[string]any
	reply := `{"success":true,"message":"Request Accepted"}`
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/parcels" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(reply))
	}))
	ref := courier.ShipmentRef{Provider: ProviderID, ID: "21A427TU4BN3R"}
	if err := c.CancelShipment(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	ud, _ := got["update_details"].(map[string]any)
	if got["entity_type"] != "parcel-tracking-id" || got["entity_id"] != "21A427TU4BN3R" || ud["property_name"] != "status" || ud["new_value"] != "cancelled" {
		t.Fatalf("%v", got)
	}
	reply = `{"success":false,"message":"cannot cancel"}`
	if err := c.CancelShipment(context.Background(), ref); !courier.IsKind(err, courier.KindRejected) {
		t.Fatalf("%v", err)
	}
	reply = `{"hello":1}`
	if err := c.CancelShipment(context.Background(), ref); !courier.IsIndeterminate(err) {
		t.Fatalf("%v", err)
	}
	if err := c.CancelShipment(context.Background(), courier.ShipmentRef{Provider: "other", ID: "x"}); !courier.IsKind(err, courier.KindValidation) {
		t.Fatalf("%v", err)
	}
}

func TestNativeAreasStoresCharge(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/areas":
			if r.URL.Query().Get("post_code") != "1206" {
				t.Errorf("query %v", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"areas":[{"id":13,"name":"Kochukhet","post_code":1206,"division_name":"Dhaka","zone_id":1}]}`))
		case r.URL.Path == "/pickup/store" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `"phone":"01898000999"`) || !strings.Contains(string(b), `"area_id":1`) {
				t.Errorf("body %s", b)
			}
			_, _ = w.Write([]byte(`{"id":1,"name":"Test Pickup Store","address":"Test Address","area_name":"Mohammadpur(Dhaka)","area_id":1,"phone":"8801898000999"}`))
		case r.URL.Path == "/pickup/stores":
			_, _ = w.Write([]byte(`{"pickup_stores":[{"id":1,"name":"A","address":"B","area_name":"C","area_id":1,"phone":"p","created_at":"x"}]}`))
		case r.URL.Path == "/pickup/store/info/1":
			_, _ = w.Write([]byte(`{"pickup_store":{"id":1,"name":"Test Pickup Store","address":"Test Address","area_name":"M","area_id":1,"phone":"8801898000999"}}`))
		case r.URL.Path == "/charge/charge_calculator":
			q := r.URL.Query()
			if q.Get("delivery_area_id") != "12" || q.Get("pickup_area_id") != "1" || q.Get("cash_collection_amount") != "0" || q.Get("weight") != "500" {
				t.Errorf("query %v", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"deliveryCharge":60,"codCharge":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()
	areas, err := c.ListAreas(ctx, AreaFilter{PostCode: 1206})
	if err != nil || len(areas) != 1 || areas[0].ID != 13 || areas[0].PostCode != 1206 {
		t.Fatalf("%v %+v", err, areas)
	}
	if _, err := c.ListAreas(ctx, AreaFilter{PostCode: 1, DistrictName: "x"}); !courier.IsKind(err, courier.KindValidation) {
		t.Fatal(err)
	}
	st, err := c.CreatePickupStore(ctx, PickupStoreInput{Name: "Test Pickup Store", Phone: "01898000999", Address: "Test Address", AreaID: 1})
	if err != nil || st.ID != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	if list, err := c.ListPickupStores(ctx); err != nil || len(list) != 1 {
		t.Fatalf("%v %+v", err, list)
	}
	if one, err := c.GetPickupStore(ctx, 1); err != nil || one.AreaName != "M" {
		t.Fatalf("%v %+v", err, one)
	}
	ch, err := c.CalculateCharge(ctx, ChargeQuery{DeliveryAreaID: 12, PickupAreaID: 1, WeightGrams: 500})
	if err != nil || ch.Delivery.Amount.String() != "60" || ch.COD == nil || !ch.COD.Amount.IsZero() {
		t.Fatalf("%v %+v", err, ch)
	}
	if _, err := c.CalculateCharge(ctx, ChargeQuery{}); !courier.IsKind(err, courier.KindValidation) {
		t.Fatal(err)
	}
}

func TestModesAndEnvironment(t *testing.T) {
	if _, err := New(WithCredentials(Credentials{Token: "t"})); !courier.IsKind(err, courier.KindConfiguration) {
		t.Fatalf("environment must be explicit: %v", err)
	}
	if _, err := New(WithSandbox()); !courier.IsKind(err, courier.KindConfiguration) {
		t.Fatalf("global mode needs credentials: %v", err)
	}
	t.Setenv("REDX_API_TOKEN", "from-env")
	c, err := New(WithSandbox())
	if err != nil || c.Mode() != auth.ModeGlobal {
		t.Fatalf("%v", err)
	}
	if _, err := NewTenant(WithSandbox(), WithCredentials(Credentials{Token: "t"})); !courier.IsKind(err, courier.KindConfiguration) {
		t.Fatalf("tenant must not hold credentials: %v", err)
	}
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("API-ACCESS-TOKEN")
		_, _ = w.Write([]byte(`{"areas":[]}`))
	}))
	defer srv.Close()
	tc, err := NewTenant(WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.ListAreas(context.Background(), AreaFilter{}); !courier.IsKind(err, courier.KindAuthMode) {
		t.Fatalf("missing tenant credentials must fail before any call: %v", err)
	}
	if seen != "" {
		t.Fatal("a request was sent without credentials")
	}
	if _, err := tc.ListAreas(ContextWithCredentials(context.Background(), Credentials{Token: "tenant-tok"}), AreaFilter{}); err != nil || seen != "Bearer tenant-tok" {
		t.Fatalf("%v %q", err, seen)
	}
	if _, err := tc.ListAreas(ContextWithCredentials(context.Background(), Credentials{Token: "bad token"}), AreaFilter{}); !courier.IsKind(err, courier.KindConfiguration) {
		t.Fatalf("%v", err)
	}
}

func TestMapStatusAndManifest(t *testing.T) {
	for in, want := range map[string]courier.Status{"pickup-pending": courier.StatusPickupPending, "ready-for-delivery": courier.StatusPickedUp,
		"delivery-in-progress": courier.StatusOutForDelivery, "delivered": courier.StatusDelivered, "agent-hold": courier.StatusOnHold,
		"agent-returning": courier.StatusReturning, "returned": courier.StatusReturned, "cancelled": courier.StatusCancelled,
		"paid": courier.StatusUnknown, "agent-area-change": courier.StatusUnknown, "new-thing": courier.StatusUnknown} {
		if got := mapStatus(in); got != want {
			t.Errorf("%s: %s want %s", in, got, want)
		}
	}
	m := Manifest()
	if m.Provider != ProviderID || m.Capabilities["webhook_status"] != courier.VerifiedImplemented || len(m.Unverified) == 0 {
		t.Fatalf("%+v", m)
	}
}
