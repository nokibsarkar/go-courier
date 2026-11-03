package steadfast

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/webhook"
)

func tokens() Option {
	return WithWebhookTokens(map[courier.AccountID]auth.Secret{"acct-a": "tok-a", "acct-b": "tok-b"})
}

func hook(t *testing.T, h courier.EventHandler, opts ...Option) http.Handler {
	t.Helper()
	c, err := NewTenant(append([]Option{tokens()}, opts...)...) // inbound needs no outbound credentials
	if err != nil {
		t.Fatal(err)
	}
	hh, err := c.WebhookHandler(h)
	if err != nil {
		t.Fatal(err)
	}
	return hh
}

// post sends a correctly signed webhook: Authorization bearer plus the hex
// HMAC-SHA256 of the raw body keyed with the same token, and an Idempotency-Key.
func post(h http.Handler, token, body string) *httptest.ResponseRecorder {
	return postSigned(h, token, body, "")
}

// postSigned lets a test override the signature ("-" sends none).
func postSigned(h http.Handler, token, body, signature string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/webhooks/steadfast", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "idem-1")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
		sig := signature
		if sig == "" {
			sig = webhook.SignHMACSHA256([]byte(token), []byte(body))
		}
		if sig != "-" {
			r.Header.Set("X-Signature", sig)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const deliveryHook = `{"notification_type":"delivery_status","consignment_id":1424107,"invoice":"ORD-1","cod_amount":1250.5,"status":"delivered","delivery_charge":60,"tracking_message":"Delivered to customer","updated_at":"2026-03-21 07:05:31"}`

func TestWebhookAuthenticatesAttributesAndAcks(t *testing.T) {
	var got []courier.Event
	h := hook(t, courier.EventHandlerFunc(func(_ context.Context, e courier.Event) error { got = append(got, e); return nil }))

	for name, tok := range map[string]string{"missing": "", "wrong": "nope", "prefix": "tok-"} {
		if w := post(h, tok, deliveryHook); w.Code != 401 || len(got) != 0 {
			t.Fatalf("%s token: %d events=%d", name, w.Code, len(got))
		}
	}
	w := post(h, "tok-b", deliveryHook)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "success") || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("ack: %d %q", w.Code, w.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("events %d", len(got))
	}
	e := got[0]
	if e.Account != "acct-b" || e.Trust != courier.TrustVerified || e.Type != "delivery_status" || e.Tracking == nil {
		t.Fatalf("%+v", e)
	}
	tr := e.Tracking
	if tr.Status != courier.StatusDelivered || tr.Ref.ID != "1424107" || tr.Ref.Account != "acct-b" || tr.ProviderStatus.Code != "delivered" ||
		tr.ProviderStatus.Description != "Delivered to customer" || tr.COD != courier.CODCollected {
		t.Fatalf("%+v", tr)
	}
	// A zone-less updated_at uses the repository default, Asia/Dhaka.
	if e.OccurredAt == nil || e.OccurredAt.Location().String() != "Asia/Dhaka" || !json.Valid(e.Payload) || !strings.Contains(string(e.Payload), "2026-03-21 07:05:31") {
		t.Fatalf("occurred/payload: %v %s", e.OccurredAt, e.Payload)
	}
}

func TestWebhookMappingsAndRejections(t *testing.T) {
	var got []courier.Event
	h := hook(t, courier.EventHandlerFunc(func(_ context.Context, e courier.Event) error { got = append(got, e); return nil }))

	// delivered_approval_pending is the rider's word, not settled: it stays
	// unknown (raw code kept) until delivered/partial_delivered/cancelled
	post(h, "tok-a", strings.Replace(deliveryHook, `"delivered"`, `"delivered_approval_pending"`, 1))
	if got[0].Tracking.Status != courier.StatusUnknown || got[0].Tracking.ProviderStatus.Code != "delivered_approval_pending" || got[0].Tracking.Delivery != nil {
		t.Fatalf("approval pending: %+v", got[0].Tracking)
	}
	// tracking_update: message only, never invented status or timestamp
	got = nil
	post(h, "tok-a", `{"notification_type":"tracking_update","consignment_id":7,"invoice":"X","tracking_message":"Rider assigned"}`)
	tu := got[0].Tracking
	if tu.Status != courier.StatusUnknown || len(tu.Events) != 1 || tu.Events[0].OccurredAt != nil || got[0].OccurredAt != nil || tu.Events[0].Description != "Rider assigned" {
		t.Fatalf("%+v", tu)
	}
	// unknown notification types are preserved raw, no mapping
	got = nil
	post(h, "tok-a", `{"notification_type":"payout","consignment_id":9}`)
	if len(got) != 1 || got[0].Type != "payout" || got[0].Tracking != nil {
		t.Fatalf("%+v", got)
	}
	// malformed -> 400, nothing dispatched
	got = nil
	for _, body := range []string{`{`, `{"consignment_id":1}`, `{"notification_type":"delivery_status","consignment_id":"abc","status":"x"}`,
		`{"notification_type":"delivery_status","consignment_id":1}`, `[]`} {
		if w := post(h, "tok-a", body); w.Code != 400 {
			t.Errorf("%s -> %d", body, w.Code)
		}
	}
	if len(got) != 0 {
		t.Fatal("malformed events dispatched")
	}
	// batch accepted
	post(h, "tok-a", "["+deliveryHook+","+deliveryHook+"]")
	if len(got) != 2 {
		t.Fatalf("batch: %d", len(got))
	}
}

func TestWebhookFailClosedAndHandlerErrorRedelivers(t *testing.T) {
	c, _ := NewTenant()
	if _, err := c.WebhookHandler(courier.EventHandlerFunc(func(context.Context, courier.Event) error { return nil })); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("no tokens must fail closed: %v", err)
	}
	if _, err := c.DecodeWebhook(context.Background(), http.Header{"Authorization": {"Bearer x"}}, []byte(deliveryHook)); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("decoder without tokens: %v", err)
	}
	for _, bad := range []map[courier.AccountID]auth.Secret{{"a": "same", "b": "same"}, {"a": ""}} {
		if _, err := NewTenant(WithWebhookTokens(bad)); !errors.Is(err, courier.ErrConfiguration) {
			t.Errorf("tokens %v must be rejected", bad)
		}
	}
	h := hook(t, courier.EventHandlerFunc(func(context.Context, courier.Event) error { return errors.New("db down") }))
	if w := post(h, "tok-a", deliveryHook); w.Code != 500 {
		t.Fatalf("handler error must fail the request so the provider redelivers: %d", w.Code)
	}
	if w := httptest.NewRecorder(); true {
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != 405 {
			t.Fatalf("method: %d", w.Code)
		}
	}
	var _ = webhook.DefaultMaxBody
}

func TestWebhookDecoderWorksWithoutHTTPAndWithGlobalClient(t *testing.T) {
	c, err := New(WithCredentials(creds), WithWebhookTokens(map[courier.AccountID]auth.Secret{"a": "t1"}))
	if err != nil {
		t.Fatal(err)
	}
	signed := func(tok, body string) http.Header {
		return http.Header{"Authorization": {"bearer " + tok}, "X-Signature": {webhook.SignHMACSHA256([]byte(tok), []byte(body))}}
	}
	evs, err := c.DecodeWebhook(context.Background(), signed("t1", deliveryHook), []byte(deliveryHook))
	if err != nil || len(evs) != 1 || evs[0].Trust != courier.TrustVerified {
		t.Fatalf("%v %v", evs, err)
	}
	if _, err := c.DecodeWebhook(context.Background(), http.Header{"Authorization": {"Basic t1"}}, []byte(deliveryHook)); !errors.Is(err, courier.ErrCredentials) {
		t.Fatalf("scheme: %v", err)
	}
}

func TestReturnsCreateGetList(t *testing.T) {
	var gotBody map[string]string
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/create_return_request":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &gotBody)
			_, _ = w.Write([]byte(`{"id":5,"consignment_id":1424107,"reason":"changed mind","status":"pending","created_at":"2026-03-21T07:05:31.000000Z"}`))
		case r.URL.Path == "/get_return_request/5":
			_, _ = w.Write([]byte(`{"id":5,"consignment_id":1424107,"status":"approved"}`))
		case r.URL.Path == "/get_return_requests":
			if r.URL.Query().Get("page") != "2" {
				t.Errorf("page query %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"data":[{"id":5,"status":"pending"},{"id":4,"status":"approved"}]}`))
		default:
			w.WriteHeader(404)
		}
	})
	c := global(t, srv.URL)
	rc, err := c.RequestReturn(context.Background(), courier.ReturnRequest{Shipment: courier.ShipmentRef{ID: "1424107"}, Reason: " changed mind "})
	if err != nil || rc.ID != "5" || rc.Status.Code != "pending" || gotBody["consignment_id"] != "1424107" || gotBody["reason"] != "changed mind" {
		t.Fatalf("%+v %v %v", rc, err, gotBody)
	}
	info, err := c.GetReturnRequest(context.Background(), "5")
	if err != nil || info.Status != "approved" || info.CreatedAt != nil {
		t.Fatalf("%+v %v", info, err)
	}
	list, err := c.ListReturnRequests(context.Background(), 2)
	if err != nil || len(list) != 2 || list[1].ID != "4" {
		t.Fatalf("%+v %v", list, err)
	}
	// local validation, no HTTP
	for _, tg := range []ReturnTarget{{}, {ConsignmentID: "1", Invoice: "A"}, {ConsignmentID: "x"}, {Invoice: "bad id"}} {
		if _, err := c.CreateReturnRequest(context.Background(), tg, ""); !errors.Is(err, courier.ErrValidation) {
			t.Errorf("%+v: %v", tg, err)
		}
	}
	if _, err := c.RequestReturn(context.Background(), courier.ReturnRequest{Shipment: courier.ShipmentRef{Provider: "pathao", ID: "1"}}); err == nil {
		t.Fatal("foreign shipment")
	}
}

func TestReturnCreateIsIndeterminateAndNotReplayed(t *testing.T) {
	srv, hits := newServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	_, err := global(t, srv.URL, WithRetry(httpxRetry(4))).CreateReturnRequest(context.Background(), ReturnTarget{Invoice: "A"}, "")
	if !courier.IsIndeterminate(err) || hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
	srv2, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	if _, err := global(t, srv2.URL).CreateReturnRequest(context.Background(), ReturnTarget{Invoice: "A"}, ""); !courier.IsIndeterminate(err) {
		t.Fatalf("a 200 without a return id after a mutation is indeterminate: %v", err)
	}
}

func TestPaymentsAndPoliceStationsAndReturnStatus(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/payments":
			_, _ = w.Write([]byte(`{"status":1,"alertClass":"success","message":"Fetched successfully!","payments":[{"payment_id":"SFC-88213","amount":12450,"method":"bKash","due_bills":0,"paid_bills":3200,"charges":124,"total":12450,"status_label":"Paid","created_at":"2026-09-18 11:04:22","ready_at":"2026-09-18 12:00:00","paid_at":"2026-09-19 10:31:07"},{"payment_id":"SFC-88214"}]}`))
		case "/payments/88213":
			_, _ = w.Write([]byte(`{"status":200,"message":"Payment fetched successfully.","payment":{"payment_id":"SFC-88213","amount":12450,"method":"bKash","due_bills":0,"paid_bills":3200,"charges":124,"total":12450.25,"status_label":"Paid","created_at":"2026-09-18 11:04:22","ready_at":"2026-09-18 12:00:00","paid_at":"2026-09-19 10:31:07","consignments":[{"consignment_id":9,"invoice":"ORD-1"}]}}`))
		case "/police_stations":
			_, _ = w.Write([]byte(`[{"id":66,"name":"Bagerhat","search_tags":null,"policestations":[{"id":127,"name":"Bagerhat sadar","status":1,"district_id":66,"search_tags":null}]}]`))
		case "/status_with_return_status_by_cid/12":
			_, _ = w.Write([]byte(`{"status":200,"delivery_status":"cancelled_return_rider_assigned"}`))
		case "/status_with_return_status_by_cid/13":
			_, _ = w.Write([]byte(`{"status":200,"delivery_status":"pending"}`))
		default:
			w.WriteHeader(404)
		}
	})
	c := global(t, srv.URL)
	ps, err := c.ListPayments(context.Background(), 0)
	if err != nil || len(ps) != 2 || ps[0].ID != "SFC-88213" || ps[0].Amount == nil || ps[0].Amount.Amount.String() != "12450" ||
		ps[0].Method != "bKash" || ps[0].StatusLabel != "Paid" || ps[0].Charges == nil || ps[0].Charges.Amount.String() != "124" ||
		ps[0].DueBills == nil || !ps[0].DueBills.Amount.IsZero() || ps[1].Amount != nil || ps[1].Charges != nil {
		t.Fatalf("%+v %v (absent amounts must stay nil, a real 0 must stay 0)", ps, err)
	}
	// Provider timestamps without a timezone use Asia/Dhaka.
	if ps[0].CreatedAt != "2026-09-18 11:04:22" || ps[0].PaidAt != "2026-09-19 10:31:07" {
		t.Fatalf("timestamps %q %q", ps[0].CreatedAt, ps[0].PaidAt)
	}
	// the id may be "SFC-88213" or 88213: only the digits are sent
	p, err := c.GetPayment(context.Background(), "SFC-88213")
	if err != nil || p.ID != "SFC-88213" || p.Total == nil || p.Total.Amount.String() != "12450.25" || p.Method != "bKash" || p.PaidAt != "2026-09-19 10:31:07" || !strings.Contains(string(p.Raw), "consignments") {
		t.Fatalf("%+v %v", p, err)
	}
	st, err := c.ListPoliceStations(context.Background())
	if err != nil || len(st) != 1 || st[0].ID != "66" || st[0].Name != "Bagerhat" ||
		len(st[0].PoliceStations) != 1 || st[0].PoliceStations[0].Name != "Bagerhat sadar" {
		t.Fatalf("%+v %v", st, err)
	}
	res, rs, err := c.TrackWithReturnStatus(context.Background(), "12")
	if err != nil || res.Status != courier.StatusReturning || rs == nil || *rs != "cancelled_return_rider_assigned" || res.Ref.ID != "12" {
		t.Fatalf("%+v %v %v", res, rs, err)
	}
	if res, rs, err := c.TrackWithReturnStatus(context.Background(), "13"); err != nil || rs != nil || res.Status != courier.StatusCreated {
		t.Fatalf("a normal delivery status is not a return status: %v %v %v", res, rs, err)
	}
	for _, bad := range []func() error{
		func() error { _, err := c.GetPayment(context.Background(), "x"); return err },
		func() error { _, err := c.ListPayments(context.Background(), -1); return err },
		func() error { _, _, err := c.TrackWithReturnStatus(context.Background(), "1a"); return err },
	} {
		if err := bad(); !errors.Is(err, courier.ErrValidation) {
			t.Errorf("want validation: %v", err)
		}
	}
}

func TestMalformedListRecordIsNotSilentlySkipped(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"ok"},{"district":"no name"}]`))
	})
	if _, err := global(t, srv.URL).ListPoliceStations(context.Background()); !errors.Is(err, courier.ErrProtocol) {
		t.Fatalf("%v", err)
	}
	srv2, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":401,"message":"no"}`)) })
	if _, err := global(t, srv2.URL).ListPayments(context.Background(), 0); !errors.Is(err, courier.ErrCredentials) {
		t.Fatalf("embedded failure in 200: %v", err)
	}
}

func TestExtrasCredentialModeRules(t *testing.T) {
	srv, hits := newServer(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := ContextWithCredentials(context.Background(), Credentials{APIKey: "t", SecretKey: "t"})
	if _, err := global(t, srv.URL).ListPayments(ctx, 0); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatal(err)
	}
	tn, _ := NewTenant(WithBaseURL(srv.URL))
	if _, err := tn.ListPoliceStations(context.Background()); !errors.Is(err, courier.ErrAuthMode) || hits.Load() != 0 {
		t.Fatal(err)
	}
	_ = time.Second
}

// A callback without notification_type but with a status is treated as a
// delivery-status update (the field is normally present; this is leniency).
func TestWebhookWithoutNotificationTypeIsDeliveryStatus(t *testing.T) {
	c, err := NewTenant(WithWebhookTokens(map[courier.AccountID]auth.Secret{"a": "tok"}))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(body []byte) http.Header {
		h := http.Header{}
		h.Set("Authorization", "Bearer tok")
		h.Set("X-Signature", webhook.SignHMACSHA256([]byte("tok"), body))
		return h
	}
	body := []byte(`{"consignment_id":1424107,"invoice":"INV-1","status":"delivered","cod_amount":"1500.00","updated_at":"2026-11-03 10:00:00"}`)
	evs, err := c.DecodeWebhook(context.Background(), sign(body), body)
	if err != nil || len(evs) != 1 || evs[0].Type != "delivery_status" || evs[0].Tracking == nil || evs[0].Tracking.Status != courier.StatusDelivered || evs[0].Tracking.Ref.ID != "1424107" {
		t.Fatalf("%v %+v", err, evs)
	}
	neither := []byte(`{"consignment_id":1,"invoice":"x"}`)
	if _, err := c.DecodeWebhook(context.Background(), sign(neither), neither); err == nil {
		t.Fatal("payload with neither type nor status accepted")
	}
}

func TestCustomerScoreKeepsUnknownAsNil(t *testing.T) {
	var gotPath string
	body := `{"status":200,"phone":"01712345678","delivery_ratio":92,"cancellation_ratio":7,"volume_band":"high","volume_range":"50+","delivered_count":58,"cancelled_count":5,"total_reports":0,"fraud_categories":{"no_response":3,"refused":1},"score":null,"level":null,"reasons":[],"scoring_disabled":true,"doubtful_reports":false}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := global(t, srv.URL)
	s, err := c.GetCustomerScore(context.Background(), "+8801712345678")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/fraud_check/score/01712345678" || s.DeliveryRatio == nil || *s.DeliveryRatio != 92 || *s.CancellationRatio != 7 ||
		s.VolumeBand != "high" || s.VolumeRange == nil || *s.VolumeRange != "50+" || *s.DeliveredCount != 58 || *s.CancelledCount != 5 ||
		s.TotalReports == nil || *s.TotalReports != 0 || s.FraudCategories["no_response"] != 3 || s.FraudCategories["refused"] != 1 ||
		s.DoubtfulReports == nil || *s.DoubtfulReports {
		t.Fatalf("path=%s %+v", gotPath, s)
	}
	// nothing finished for this number, and counts not shown: null is "unknown", never zero
	body = `{"status":200,"phone":"01712345678","delivery_ratio":null,"cancellation_ratio":null,"volume_band":"none","volume_range":null,"delivered_count":null,"cancelled_count":null,"total_reports":null,"fraud_categories":[],"score":null,"level":null,"reasons":[],"scoring_disabled":true,"doubtful_reports":false}`
	s, err = c.GetCustomerScore(context.Background(), "01712345678")
	if err != nil {
		t.Fatal(err)
	}
	if s.DeliveryRatio != nil || s.CancellationRatio != nil || s.VolumeRange != nil || s.DeliveredCount != nil || s.CancelledCount != nil || s.TotalReports != nil || len(s.FraudCategories) != 0 {
		t.Fatalf("null must stay nil: %+v", s)
	}
	if _, err := c.GetCustomerScore(context.Background(), "123"); err == nil {
		t.Fatal("invalid phone accepted")
	}
}

func TestZeroBalanceIsRealZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":200,"current_balance":0}`))
	}))
	defer srv.Close()
	b, err := global(t, srv.URL).GetBalance(context.Background())
	if err != nil || !b.Amount.Amount.IsZero() {
		t.Fatalf("%v %+v", err, b)
	}
}

func TestCreateReturnRequestShape(t *testing.T) {
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":1,"user_id":1,"consignment_id":10000042,"status":"pending","updated_at":"2025-07-30T23:11:45.000000Z"}`))
	}))
	defer srv.Close()
	info, err := global(t, srv.URL).CreateReturnRequest(context.Background(), ReturnTarget{TrackingCode: "15BAEB8A"}, "damaged")
	if err != nil {
		t.Fatal(err)
	}
	if body["tracking_code"] != "15BAEB8A" || body["reason"] != "damaged" || len(body) != 2 {
		t.Fatalf("body %v", body)
	}
	if info.ID != "1" || info.UserID != "1" || info.ConsignmentID != "10000042" || info.Status != "pending" || info.UpdatedAt == nil || info.CreatedAt != nil {
		t.Fatalf("%+v", info)
	}
}

// tracking_update payload.
func TestWebhookTrackingUpdateShape(t *testing.T) {
	var got []courier.Event
	h := hook(t, courier.EventHandlerFunc(func(_ context.Context, e courier.Event) error { got = append(got, e); return nil }))
	w := post(h, "tok-a", `{"notification_type":"tracking_update","consignment_id":12345,"invoice":"INV-67890","tracking_message":"Package arrived at the sorting center.","updated_at":"2025-03-02 13:15:00"}`)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"status":"success","message":"Webhook received successfully."}` || len(got) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	tu := got[0].Tracking
	if got[0].Type != "tracking_update" || tu.Ref.ID != "12345" || tu.Status != courier.StatusUnknown || len(tu.Events) != 1 ||
		tu.Events[0].Description != "Package arrived at the sorting center." || tu.Events[0].OccurredAt == nil || got[0].OccurredAt == nil {
		t.Fatalf("%+v", got[0])
	}
}

// delivery_status payload: the status may be capitalised ("Delivered").
func TestWebhookDeliveryStatusShape(t *testing.T) {
	var got []courier.Event
	h := hook(t, courier.EventHandlerFunc(func(_ context.Context, e courier.Event) error { got = append(got, e); return nil }))
	w := post(h, "tok-a", `{"notification_type":"delivery_status","consignment_id":12345,"invoice":"INV-67890","cod_amount":1500.00,"status":"Delivered","delivery_charge":100.00,"tracking_message":"Your package has been delivered successfully.","updated_at":"2025-03-02 12:45:30"}`)
	if w.Code != 200 || len(got) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	tr := got[0].Tracking
	if tr.Status != courier.StatusDelivered || tr.Ref.ID != "12345" || tr.ProviderStatus.Code != "Delivered" ||
		tr.ProviderStatus.Description != "Your package has been delivered successfully." || got[0].OccurredAt == nil {
		t.Fatalf("%+v", got[0])
	}
}

func TestWebhookRequiresValidSignatureAndUsesIdempotencyKey(t *testing.T) {
	var got []courier.Event
	h := hook(t, courier.EventHandlerFunc(func(_ context.Context, e courier.Event) error { got = append(got, e); return nil }))
	// right bearer token but no signature, a wrong signature, or a signature of another body
	other := webhook.SignHMACSHA256([]byte("tok-a"), []byte(`{"notification_type":"delivery_status"}`))
	for name, sig := range map[string]string{"missing": "-", "wrong": "deadbeef", "other body": other} {
		if w := postSigned(h, "tok-a", deliveryHook, sig); w.Code != 401 || len(got) != 0 {
			t.Fatalf("%s signature: %d events=%d", name, w.Code, len(got))
		}
	}
	// a valid signature made with another account's token does not authenticate
	if w := postSigned(h, "tok-a", deliveryHook, webhook.SignHMACSHA256([]byte("tok-b"), []byte(deliveryHook))); w.Code != 401 {
		t.Fatalf("cross-account signature accepted: %d", w.Code)
	}
	if w := post(h, "tok-a", deliveryHook); w.Code != 200 || len(got) != 1 {
		t.Fatalf("valid request refused: %d", w.Code)
	}
	if got[0].ID != "idem-1" || got[0].Account != "acct-a" {
		t.Fatalf("event id must be the Idempotency-Key: %+v", got[0])
	}
}

func TestWebhookPassesThroughOtherNotificationTypes(t *testing.T) {
	var got []courier.Event
	h := hook(t, courier.EventHandlerFunc(func(_ context.Context, e courier.Event) error { got = append(got, e); return nil }))
	for _, body := range []string{
		`{"notification_type":"consignment_update","event_id":"0b8f","consignment_id":12345,"invoice":"INV-67890","tracking_id":"SFR1","changed":["cus_address"],"values":{"cus_address":"House 12, Road 5, Mirpur"},"updated_at":"2025-03-02 13:20:00"}`,
		`{"notification_type":"payment_request","payment_request_id":77,"amount":"500.00","method":"bKash","status":"pending","note":"n"}`,
		`{"notification_type":"pickup_request","pickup_request_id":5,"address_id":42,"pickup_location":"Dhanmondi","service_type":"regular","estimated_qty":25}`,
		`{"notification_type":"return_list_accepted","return_list_id":3,"status":"accepted","item_count":2}`,
		`{"notification_type":"user_update"}`,
	} {
		got = nil
		if w := post(h, "tok-a", body); w.Code != 200 || len(got) != 1 {
			t.Fatalf("%s: %d events=%d", body, w.Code, len(got))
		}
		if got[0].Tracking != nil || !json.Valid(got[0].Payload) || got[0].Type == "" {
			t.Fatalf("other types carry no invented tracking: %+v", got[0])
		}
	}
	// parcel events still need a consignment id
	if w := post(h, "tok-a", `{"notification_type":"tracking_update","tracking_message":"x"}`); w.Code == 200 {
		t.Fatal("tracking_update without consignment_id accepted")
	}
}

func TestCreatePickupRequest(t *testing.T) {
	var body map[string]any
	var status = 201
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/create_pickup_request" || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		if status == 409 {
			_, _ = w.Write([]byte(`{"message":"PICKUP_REQUEST_EXISTS"}`))
			return
		}
		_, _ = w.Write([]byte(`{"message":"Pickup request created successfully.","data":{"id":9081,"user_id":12345,"user_address_id":42,"police_station_id":17,"pickup_location":"House 17/1, Road 3/A, Dhanmondi, Dhaka","contact_number":"01712345678","note":null,"estim_qty":25,"req_status":0,"created_at":"2026-09-20T07:05:31.000000Z"}}`))
	}))
	defer srv.Close()
	c := global(t, srv.URL)
	qty := 25
	req := PickupRequest{AddressID: 42, PoliceStationID: 17, Address: "House 17/1, Road 3/A, Dhanmondi, Dhaka", ContactNumber: "01712345678", EstimatedQty: &qty}
	p, err := c.CreatePickupRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if body["address_id"] != float64(42) || body["police_station_id"] != float64(17) || body["contact_number"] != "01712345678" || body["estim_qty"] != float64(25) {
		t.Fatalf("body %v", body)
	}
	if _, has := body["note"]; has {
		t.Fatal("empty note must be omitted")
	}
	if p.ID != "9081" || p.AddressID != "42" || p.PoliceStationID != "17" || p.Note != nil || p.EstimatedQty == nil || *p.EstimatedQty != 25 || p.Status != "0" {
		t.Fatalf("%+v", p)
	}
	status = 409
	if _, err := c.CreatePickupRequest(context.Background(), req); !errors.Is(err, courier.ErrConflict) {
		t.Fatalf("a pending request must be a conflict: %v", err)
	}
	for name, mut := range map[string]func(*PickupRequest){
		"no address id": func(r *PickupRequest) { r.AddressID = 0 },
		"no station":    func(r *PickupRequest) { r.PoliceStationID = 0 },
		"no address":    func(r *PickupRequest) { r.Address = " " },
		"phone 012":     func(r *PickupRequest) { r.ContactNumber = "01212345678" },
		"long note":     func(r *PickupRequest) { r.Note = strings.Repeat("x", 501) },
		"negative qty":  func(r *PickupRequest) { n := -1; r.EstimatedQty = &n },
	} {
		r := req
		mut(&r)
		if _, err := c.CreatePickupRequest(context.Background(), r); !errors.Is(err, courier.ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTrackingHistory(t *testing.T) {
	var path string
	body := `{"status":200,"tracking":[{"consignment_id":1424107,"tracking_type":1,"text":"Consignment created by Sender(API).","created_at":"2026-09-20T07:05:31.000000Z"},{"consignment_id":1424107,"tracking_type":2,"text":"Parcel received at Dhanmondi hub.","created_at":"2026-09-20T11:22:04.000000Z"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { path = r.URL.Path; _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	c := global(t, srv.URL)
	res, err := c.TrackingHistory(context.Background(), "ORD-10231")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/trackings_by_invoice/ORD-10231" || res.Ref.ID != "1424107" || res.Coverage != courier.CoverageFull || res.Status != courier.StatusUnknown || len(res.Events) != 2 {
		t.Fatalf("%s %+v", path, res)
	}
	e := res.Events[1]
	if e.Description != "Parcel received at Dhanmondi hub." || e.Status != courier.StatusUnknown || e.ProviderStatus.Code != "2" || e.OccurredAt == nil || e.OccurredAt.UTC().Hour() != 11 {
		t.Fatalf("%+v", e)
	}
	body = `{"status":200,"tracking":[]}`
	if res, err := c.TrackingHistory(context.Background(), "ORD-10231"); err != nil || res.Coverage != courier.CoverageUnknown || len(res.Events) != 0 {
		t.Fatalf("an empty history must not claim full coverage: %v %+v", err, res)
	}
	if _, err := c.TrackingHistory(context.Background(), "bad id"); !errors.Is(err, courier.ErrValidation) {
		t.Fatalf("%v", err)
	}
}
