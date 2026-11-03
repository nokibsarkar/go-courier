package carrybee

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
	"github.com/nokibsarkar/go-courier/httpx"
)

var creds = Credentials{ClientID: "cid", ClientSecret: "csec", ClientContext: "cctx", AccountID: "acct-1"}

func retryFast() httpx.RetryPolicy {
	return httpx.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
}

func client(t *testing.T, h http.HandlerFunc, opts ...Option) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(append([]Option{WithCredentials(creds), WithBaseURL(srv.URL), WithRetry(retryFast())}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func ref(kind, id string) courier.LocationRef {
	return courier.LocationRef{Provider: ProviderID, Kind: kind, ID: id}
}

func req(cod string) courier.CreateShipmentRequest {
	w := uint32(700)
	r := courier.CreateShipmentRequest{
		MerchantOrderID: "ORD-1",
		Pickup:          courier.Party{Address: courier.Address{Refs: []courier.LocationRef{ref("store", "st-9")}}},
		Recipient: courier.Party{
			Contact: courier.Contact{Name: "John Doe", Phone: "+8801711111111"},
			Address: courier.Address{Text: "House 44, Road 2/A", Refs: []courier.LocationRef{ref("city", "1"), ref("zone", "5")}},
		},
		Package: courier.Package{WeightGrams: &w, Quantity: 2, Description: "Shirt x 2"},
	}
	if cod != "" {
		m := courier.NewBDT(mustDec(cod))
		r.COD = &m
	}
	return r
}

func TestCreateWireShapeHeadersAndMapping(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	var path, method string
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		hdr, path, method = r.Header.Clone(), r.URL.Path, r.Method
		_, _ = w.Write([]byte(`{"data":{"order":{"consignment_id":"CB-77","store_id":"st-9","delivery_fee":60,"cod_fee":10}}}`))
	})
	sh, err := c.CreateShipment(context.Background(), req("1250"))
	if err != nil {
		t.Fatal(err)
	}
	if method != "POST" || path != "/api/v2/orders" || hdr.Get("Client-Id") != "cid" || hdr.Get("Client-Secret") != "csec" || hdr.Get("Client-Context") != "cctx" {
		t.Fatalf("%s %s %v", method, path, hdr)
	}
	if body["store_id"] != "st-9" || body["recipient_phone"] != "01711111111" || body["item_weight"] != float64(700) ||
		body["item_quantity"] != float64(2) || body["collectable_amount"] != float64(1250) || body["city_id"] != float64(1) || body["zone_id"] != float64(5) {
		t.Fatalf("body %v", body)
	}
	if _, has := body["delivery_type"]; has {
		t.Fatal("unset delivery_type must be omitted, not defaulted")
	}
	if _, has := body["area_id"]; has {
		t.Fatal("unset area must be omitted")
	}
	if sh.Ref.ID != "CB-77" || sh.Ref.Account != "acct-1" || sh.Status != courier.StatusCreated || sh.DeliveryCharge == nil || sh.DeliveryCharge.Amount.String() != "60" {
		t.Fatalf("%+v", sh)
	}
}

func TestNoCODOmitsCollectableAmount(t *testing.T) {
	var body map[string]any
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = w.Write([]byte(`{"data":{"order":{"consignment_id":5}}}`))
	})
	ctx := ContextWithOrderOptions(context.Background(), OrderOptions{DeliveryType: 2, ProductType: 1})
	if _, err := c.CreateShipment(ctx, req("")); err != nil {
		t.Fatal(err)
	}
	if _, has := body["collectable_amount"]; has || body["delivery_type"] != float64(2) {
		t.Fatalf("%v", body)
	}
}

func TestValidationFailuresMakeNoRequest(t *testing.T) {
	var hits atomic.Int32
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	cases := map[string]func(*courier.CreateShipmentRequest){
		"no store": func(r *courier.CreateShipmentRequest) { r.Pickup.Address.Refs = nil },
		"no city": func(r *courier.CreateShipmentRequest) {
			r.Recipient.Address.Refs = []courier.LocationRef{ref("zone", "5")}
		},
		"bad zone":   func(r *courier.CreateShipmentRequest) { r.Recipient.Address.Refs[1].ID = "x" },
		"no weight":  func(r *courier.CreateShipmentRequest) { r.Package.WeightGrams = nil },
		"no qty":     func(r *courier.CreateShipmentRequest) { r.Package.Quantity = 0 },
		"bad phone":  func(r *courier.CreateShipmentRequest) { r.Recipient.Contact.Phone = "123" },
		"no name":    func(r *courier.CreateShipmentRequest) { r.Recipient.Contact.Name = " " },
		"fraction":   func(r *courier.CreateShipmentRequest) { m := courier.NewBDT(mustDec("10.5")); r.COD = &m },
		"no address": func(r *courier.CreateShipmentRequest) { r.Recipient.Address.Text = "" },
	}
	for name, mut := range cases {
		r := req("")
		mut(&r)
		if _, err := c.CreateShipment(context.Background(), r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("validation failures sent %d requests", hits.Load())
	}
}

func TestEmbeddedFailureIn200IsRejection(t *testing.T) {
	for name, body := range map[string]string{
		"errors": `{"message":"Validation error","errors":{"city_id":["The city id field is required."]}}`,
		"false":  `{"status":false,"message":"Store not found"}`,
		"error":  `{"error":"bad context"}`,
	} {
		c, _ := client(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		_, err := c.CreateShipment(context.Background(), req(""))
		if !courier.IsKind(err, courier.KindRejected) {
			t.Errorf("%s: %v", name, err)
		}
	}
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":"Validation error","errors":{"city_id":["required"]}}`))
	})
	_, err := c.CreateShipment(context.Background(), req(""))
	if err == nil || !strings.Contains(err.Error(), "city id: required") {
		t.Fatalf("details must replace the generic message: %v", err)
	}
}

func TestCreateIndeterminateAndNeverReplayed(t *testing.T) {
	var hits atomic.Int32
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(502)
	})
	if _, err := c.CreateShipment(context.Background(), req("")); !courier.IsIndeterminate(err) {
		t.Fatalf("5xx on create must be indeterminate: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("create replayed %d times", hits.Load())
	}
	c2, _ := client(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":{"order":{}}}`)) })
	if _, err := c2.CreateShipment(context.Background(), req("")); !courier.IsIndeterminate(err) {
		t.Fatalf("200 without a consignment id must be indeterminate: %v", err)
	}
}

func TestTrackMapsDetailsAndUnknownStaysUnknown(t *testing.T) {
	var path string
	state := `{"data":{"transfer_status":"delivered","collected_amount":1250.0,"attempt":1}}`
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) { path = r.URL.Path; _, _ = w.Write([]byte(state)) })
	res, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "CB-77"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/v2/orders/CB-77/details" || res.Status != courier.StatusDelivered || res.COD != courier.CODCollected || res.Delivery == nil ||
		res.Coverage != courier.CoverageCurrentOnly || len(res.Events) != 0 {
		t.Fatalf("%s %+v", path, res)
	}
	state = `{"data":{"transfer_status":"mystery"}}`
	res, err = c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "CB-77"})
	if err != nil || res.Status != courier.StatusUnknown || res.ProviderStatus.Code != "mystery" || res.COD != courier.CODUnknown {
		t.Fatalf("%v %+v", err, res)
	}
	state = `{"data":{}}`
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "CB-77"}); !courier.IsKind(err, courier.KindProtocol) {
		t.Fatalf("missing transfer_status: %v", err)
	}
	if _, err := c.TrackShipment(context.Background(), courier.TrackingRequest{Code: "../x"}); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestCancelAndGETRetryVersusPOST(t *testing.T) {
	var path, method string
	var body map[string]string
	var hits atomic.Int32
	mode := "ok"
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		path, method = r.URL.Path, r.Method
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
		}
		switch mode {
		case "ok":
			_, _ = w.Write([]byte(`{"message":"cancelled"}`))
		case "5xx":
			w.WriteHeader(503)
		case "reject":
			_, _ = w.Write([]byte(`{"status":false,"message":"Cannot cancel a picked order"}`))
		}
	})
	if err := c.CancelOrder(context.Background(), "CB-77", "customer asked", ProviderID); err != nil || method != "POST" || path != "/api/v2/orders/CB-77/cancel" || body["cancellation_reason"] != "customer asked" {
		t.Fatalf("%v %s %s %v", err, method, path, body)
	}
	mode = "reject"
	if err := c.CancelShipment(context.Background(), courier.ShipmentRef{Provider: ProviderID, ID: "CB-77"}); !courier.IsKind(err, courier.KindRejected) {
		t.Fatalf("%v", err)
	}
	mode, hits = "5xx", atomic.Int32{}
	if err := c.CancelShipment(context.Background(), courier.ShipmentRef{Provider: ProviderID, ID: "CB-77"}); err == nil || hits.Load() != 1 {
		t.Fatalf("POST must not be retried: err=%v hits=%d", err, hits.Load())
	}
	hits = atomic.Int32{}
	if _, err := c.ListCities(context.Background()); err == nil || hits.Load() != 3 {
		t.Fatalf("GET should retry: err=%v hits=%d", err, hits.Load())
	}
	if err := c.CancelOrder(context.Background(), "CB-77", "", "redx"); err == nil {
		t.Fatal("foreign provider accepted")
	}
}

func TestNativeLists(t *testing.T) {
	var paths []string
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		_, _ = w.Write([]byte(`{"data":[{"id":1,"name":"Dhaka"},{"id":"2","name":"Chattogram"}]}`))
	})
	cities, err := c.ListCities(context.Background())
	if err != nil || len(cities) != 2 || cities[0].ID != "1" || cities[1].ID != "2" || cities[1].Name != "Chattogram" || len(cities[0].Raw) == 0 {
		t.Fatalf("%v %+v", err, cities)
	}
	if _, err := c.ListZones(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListAreas(context.Background(), "1", "5"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AreaSuggestion(context.Background(), "mirpur 10"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListStores(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"/api/v2/cities", "/api/v2/cities/1/zones", "/api/v2/cities/1/zones/5/areas", "/api/v2/area-suggestion?search=mirpur+10", "/api/v2/stores"}
	for i, w := range want {
		if paths[i] != w {
			t.Errorf("path %d = %q want %q", i, paths[i], w)
		}
	}
	if _, err := c.ListZones(context.Background(), "1/../2"); err == nil {
		t.Fatal("non-numeric city accepted")
	}
}

func TestModesAndEnvironment(t *testing.T) {
	if _, err := New(WithCredentials(creds)); !courier.IsKind(err, courier.KindConfiguration) {
		t.Fatalf("environment must be explicit: %v", err)
	}
	if _, err := New(WithSandbox(), WithCredentials(Credentials{ClientID: "a"})); !courier.IsKind(err, courier.KindConfiguration) {
		t.Fatalf("incomplete credentials: %v", err)
	}
	if _, err := NewTenant(WithSandbox(), WithCredentials(creds)); !courier.IsKind(err, courier.KindConfiguration) {
		t.Fatalf("tenant with global credentials: %v", err)
	}
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Client-Id")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	tc, err := NewTenant(WithBaseURL(srv.URL))
	if err != nil || tc.Mode() != auth.ModeTenant {
		t.Fatalf("%v", err)
	}
	if _, err := tc.ListCities(context.Background()); !courier.IsKind(err, courier.KindAuthMode) {
		t.Fatalf("tenant call without credentials must fail before HTTP: %v", err)
	}
	if _, err := tc.ListCities(ContextWithCredentials(context.Background(), creds)); err != nil || seen != "cid" {
		t.Fatalf("%v %q", err, seen)
	}
}

func TestStatusMapping(t *testing.T) {
	for in, want := range map[string]courier.Status{
		"order.picked": courier.StatusPickedUp, "In Transit": courier.StatusInTransit, "order.assigned-for-delivery": courier.StatusOutForDelivery,
		"order.at-the-sorting-hub": courier.StatusInTransit, "order.delivered": courier.StatusDelivered, "order.delivery-failed": courier.StatusDeliveryFailed,
		"order.returned-to-merchant": courier.StatusReturned, "cancelled": courier.StatusCancelled, "order.paid": courier.StatusUnknown, "brand-new": courier.StatusUnknown,
	} {
		if got := mapStatus(in); got != want {
			t.Errorf("%q = %s want %s", in, got, want)
		}
	}
}
