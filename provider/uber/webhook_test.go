package uber

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/webhook"
)

// Payload shapes derived from public docs, not captured from production.
const hookBody = `{"kind":"event.delivery_status","id":"evt_1","delivery_id":"del_1","status":"pickup_complete","created":"2026-11-03T10:05:00Z","data":{"id":"del_1","status":"pickup_complete","updated":"2026-11-03T10:05:00Z"}}`

func hookClient(t *testing.T, keys map[courier.AccountID]auth.Secret) *Client {
	t.Helper()
	// Tenant client with no credentials anywhere: verification must not need them.
	c, err := NewTenant(WithWebhookSigningKeys(keys))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func signed(key, body string) http.Header {
	h := http.Header{}
	h.Set("X-Uber-Signature", webhook.SignHMACSHA256([]byte(key), []byte(body)))
	return h
}

var twoKeys = map[courier.AccountID]auth.Secret{"acct-a": "key-a", "acct-b": "key-b"}

func TestWebhookVerifiedEventAttributedAndMapped(t *testing.T) {
	c := hookClient(t, twoKeys)
	evs, err := c.DecodeWebhook(context.Background(), signed("key-b", hookBody), []byte(hookBody))
	if err != nil || len(evs) != 1 {
		t.Fatalf("%v %v", err, evs)
	}
	e := evs[0]
	if e.Trust != courier.TrustVerified || e.Account != "acct-b" || e.Provider != ProviderID || e.Type != "event.delivery_status" || e.ID != "evt_1" {
		t.Fatalf("%+v", e)
	}
	if e.OccurredAt == nil || e.Tracking == nil || e.Tracking.Status != courier.StatusPickedUp || e.Tracking.ProviderStatus.Code != "pickup_complete" ||
		e.Tracking.Ref.ID != "del_1" || e.Tracking.Ref.Account != "acct-b" || e.Tracking.Coverage != courier.CoverageCurrentOnly || len(e.Tracking.Events) != 0 {
		t.Fatalf("%+v", e.Tracking)
	}
	if string(e.Payload) != hookBody {
		t.Fatal("payload not preserved")
	}
}

func TestWebhookStatusFromDataOnlyAndDelivered(t *testing.T) {
	body := `{"kind":"event.delivery_status","data":{"id":"del_2","status":"delivered"}}`
	evs, err := hookClient(t, twoKeys).DecodeWebhook(context.Background(), signed("key-a", body), []byte(body))
	if err != nil || evs[0].Tracking == nil || evs[0].Tracking.Delivery == nil || evs[0].OccurredAt != nil {
		t.Fatalf("%v %+v", err, evs)
	}
	body = `{"kind":"event.delivery_status","delivery_id":"del_2","status":"brand_new"}`
	evs, _ = hookClient(t, twoKeys).DecodeWebhook(context.Background(), signed("key-a", body), []byte(body))
	if evs[0].Tracking.Status != courier.StatusUnknown || evs[0].Tracking.ProviderStatus.Code != "brand_new" {
		t.Fatalf("%+v", evs[0].Tracking)
	}
	body = `{"kind":"event.delivery_status","status":"delivered"}` // no delivery id: nothing fabricated
	evs, _ = hookClient(t, twoKeys).DecodeWebhook(context.Background(), signed("key-a", body), []byte(body))
	if evs[0].Tracking != nil {
		t.Fatal("tracking without a delivery id")
	}
}

func TestWebhookSignatureNegatives(t *testing.T) {
	c := hookClient(t, twoKeys)
	ctx := context.Background()
	body := []byte(hookBody)
	tampered := []byte(strings.Replace(hookBody, "pickup_complete", "delivered", 1))
	cases := map[string]struct {
		h    http.Header
		body []byte
	}{
		"tampered body": {signed("key-a", hookBody), tampered},
		"wrong key":     {signed("nope", hookBody), body},
		"no header":     {http.Header{}, body},
		"garbage sig":   {http.Header{"X-Uber-Signature": {"zz"}}, body},
		"empty sig":     {http.Header{"X-Uber-Signature": {""}}, body},
	}
	for name, tc := range cases {
		evs, err := c.DecodeWebhook(ctx, tc.h, tc.body)
		if err == nil || len(evs) != 0 || !errors.Is(err, courier.ErrCredentials) {
			t.Errorf("%s: %v %v", name, err, evs)
		}
	}
}

func TestWebhookAmbiguousMultiKeyIsRefused(t *testing.T) {
	c := hookClient(t, map[courier.AccountID]auth.Secret{"acct-a": "same", "acct-b": "same", "acct-c": "other"})
	evs, err := c.DecodeWebhook(context.Background(), signed("same", hookBody), []byte(hookBody))
	if !errors.Is(err, courier.ErrAmbiguous) || len(evs) != 0 {
		t.Fatalf("%v %v", err, evs)
	}
	// Through the HTTP handler: ambiguous is not dispatched and not 200.
	var n int
	h, _ := c.WebhookHandler(courier.EventHandlerFunc(func(context.Context, courier.Event) error { n++; return nil }))
	rec := serve(h, signed("same", hookBody), hookBody)
	if rec.Code == 200 || n != 0 {
		t.Fatalf("code %d dispatched %d", rec.Code, n)
	}
}

func TestWebhookPostmatesHeaderOnlyForStatusAndCourierEvents(t *testing.T) {
	c := hookClient(t, twoKeys)
	pm := func(key, body string) http.Header {
		h := http.Header{}
		h.Set("X-Postmates-Signature", webhook.SignHMACSHA256([]byte(key), []byte(body)))
		return h
	}
	if _, err := c.DecodeWebhook(context.Background(), pm("key-a", hookBody), []byte(hookBody)); err != nil {
		t.Fatalf("delivery status via postmates header: %v", err)
	}
	cu := `{"kind":"event.courier_update","delivery_id":"del_1"}`
	if _, err := c.DecodeWebhook(context.Background(), pm("key-a", cu), []byte(cu)); err != nil {
		t.Fatalf("courier update via postmates header: %v", err)
	}
	refund := `{"kind":"event.refund_request","id":"r1"}`
	if _, err := c.DecodeWebhook(context.Background(), pm("key-a", refund), []byte(refund)); !errors.Is(err, courier.ErrCredentials) {
		t.Fatalf("refund must require x-uber-signature: %v", err)
	}
	evs, err := c.DecodeWebhook(context.Background(), signed("key-a", refund), []byte(refund))
	if err != nil || evs[0].Tracking != nil || evs[0].Type != "event.refund_request" {
		t.Fatalf("%v %+v", err, evs)
	}
}

func TestWebhookBatchAndMalformed(t *testing.T) {
	c := hookClient(t, twoKeys)
	batch := `[` + hookBody + `,{"kind":"event.courier_update","delivery_id":"del_1"}]`
	evs, err := c.DecodeWebhook(context.Background(), signed("key-a", batch), []byte(batch))
	if err != nil || len(evs) != 2 || evs[0].Tracking == nil || evs[1].Tracking != nil || evs[1].Account != "acct-a" {
		t.Fatalf("%v %+v", err, evs)
	}
	for _, bad := range []string{`not json`, `[]`, `{"id":"x"}`, `[1]`, ``} {
		if _, err := c.DecodeWebhook(context.Background(), signed("key-a", bad), []byte(bad)); !errors.Is(err, courier.ErrProtocol) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestWebhookOversizedBody(t *testing.T) {
	c := hookClient(t, twoKeys)
	big := `{"kind":"event.delivery_status","pad":"` + strings.Repeat("a", webhook.DefaultMaxBody) + `"}`
	if _, err := c.DecodeWebhook(context.Background(), signed("key-a", big), []byte(big)); !errors.Is(err, courier.ErrResourceLimit) {
		t.Fatalf("decode: %v", err)
	}
	h, _ := c.WebhookHandler(courier.EventHandlerFunc(func(context.Context, courier.Event) error { t.Error("dispatched"); return nil }))
	if rec := serve(h, signed("key-a", big), big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d", rec.Code)
	}
}

func serve(h http.Handler, hdr http.Header, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/hooks/uber", bytes.NewReader([]byte(body)))
	for k, v := range hdr {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookHandlerStatusCodesAndDispatch(t *testing.T) {
	c := hookClient(t, twoKeys)
	var mu sync.Mutex
	var got []courier.Event
	fail := false
	h, err := c.WebhookHandler(courier.EventHandlerFunc(func(_ context.Context, e courier.Event) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e)
		if fail {
			return errors.New("app down")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if rec := serve(h, signed("key-a", hookBody), hookBody); rec.Code != 200 || len(got) != 1 {
		t.Fatalf("code %d events %d", rec.Code, len(got))
	}
	got = nil
	if rec := serve(h, signed("bad", hookBody), hookBody); rec.Code != 401 || len(got) != 0 {
		t.Fatalf("code %d events %d", rec.Code, len(got))
	}
	if rec := serve(h, signed("key-a", hookBody), hookBody+" "); rec.Code != 401 || len(got) != 0 {
		t.Fatalf("tampered: code %d", rec.Code)
	}
	fail = true // 500 lets Uber retry
	if rec := serve(h, signed("key-a", hookBody), hookBody); rec.Code != 500 {
		t.Fatalf("code %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestWebhookRequiresKeysAndDoesNotNeedOutboundCredentials(t *testing.T) {
	c, _ := NewTenant()
	if _, err := c.WebhookHandler(courier.EventHandlerFunc(func(context.Context, courier.Event) error { return nil })); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.DecodeWebhook(context.Background(), signed("k", hookBody), []byte(hookBody)); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("got %v", err)
	}
	if _, err := NewTenant(WithWebhookSigningKeys(map[courier.AccountID]auth.Secret{"a": ""})); !errors.Is(err, courier.ErrConfiguration) {
		t.Fatalf("empty key: %v", err)
	}
	// Ambient context credentials must not influence verification (HOOK-02).
	tn := hookClient(t, twoKeys)
	ctx := ContextWithCredentials(context.Background(), creds)
	if _, err := tn.DecodeWebhook(ctx, signed("key-a", hookBody), []byte(hookBody)); err != nil {
		t.Fatal(err)
	}
	// Caller map mutation after construction does not change the client.
	keys := map[courier.AccountID]auth.Secret{"a": "k1"}
	cl, _ := NewTenant(WithWebhookSigningKeys(keys))
	keys["a"] = "other"
	if _, err := cl.DecodeWebhook(context.Background(), signed("k1", hookBody), []byte(hookBody)); err != nil {
		t.Fatal(err)
	}
}

// delivery_status webhook body.
const docHookBody = `{"account_id":"","batch_id":"bat_voEiX66nUf-D4XzKRlpJLQ","created":"2023-08-01T06:28:22.695Z","customer_id":"fb109f30-d2f0-5447-a0fa-884a44394axx","data":{"complete":true,"courier_imminent":false,"created":"2023-08-01T06:26:13.896Z","currency":"usd","fee":9200,"id":"del_QbLowiwHQM-b4e8YmOZNOw","kind":"delivery","status":"delivered","updated":"2023-08-01T06:28:22.611Z","uuid":"41B2E8C22C0740CF9BE1EF1898E64D3B"},"delivery_id":"del_QbLowiwHQM-b4e8YmOZNOw","developer_id":"","id":"evt_Bouz7BhPTYGDz9FFQNgODw","kind":"event.delivery_status","live_mode":true,"route_id":"rte_I8-ffNPuT82EP1yaBmG1jg","status":"delivered"}`

func TestWebhookDeliveryStatusAndReturnLeg(t *testing.T) {
	c := hookClient(t, twoKeys)
	evs, err := c.DecodeWebhook(context.Background(), signed("key-a", docHookBody), []byte(docHookBody))
	if err != nil || len(evs) != 1 {
		t.Fatalf("%v %v", err, evs)
	}
	e := evs[0]
	if e.ID != "evt_Bouz7BhPTYGDz9FFQNgODw" || e.OccurredAt == nil || e.OccurredAt.UTC().Format("2006-01-02T15:04:05") != "2023-08-01T06:28:22" ||
		e.Tracking == nil || e.Tracking.Status != courier.StatusDelivered || e.Tracking.Ref.ID != "del_QbLowiwHQM-b4e8YmOZNOw" {
		t.Fatalf("%+v %+v", e, e.Tracking)
	}
	// Return leg ids start with ret_; shopping_completed is documented but has
	// no neutral status, so it stays unknown with the raw value kept.
	body := `{"kind":"event.delivery_status","id":"evt_2","delivery_id":"ret_abc","status":"shopping_completed","created":"2023-08-01T06:28:22.695Z"}`
	evs, err = c.DecodeWebhook(context.Background(), signed("key-a", body), []byte(body))
	if err != nil || evs[0].Tracking == nil || evs[0].Tracking.Ref.ID != "ret_abc" ||
		evs[0].Tracking.Status != courier.StatusUnknown || evs[0].Tracking.ProviderStatus.Code != "shopping_completed" {
		t.Fatalf("%v %+v", err, evs)
	}
}

// courier_update webhook body.
const courierHookBody = `{"created":"2023-07-06T05:09:07.173Z","data":{"id":"del_y_aY8RuTQ0CRKu2aFXe8qQ","status":"pickup","courier_imminent":false,"updated":"2023-07-06T05:08:25.53Z"},"delivery_id":"del_y_aY8RuTQ0CRKu2aFXe8qQ","id":"evt_WNjLziAJT4eiOKgPsLNtbw","kind":"event.courier_update","live_mode":true,"location":{"lat":40.71093,"lng":-74.0119}}`

func TestWebhookCourierUpdate(t *testing.T) {
	evs, err := hookClient(t, twoKeys).DecodeWebhook(context.Background(), signed("key-a", courierHookBody), []byte(courierHookBody))
	if err != nil || len(evs) != 1 {
		t.Fatalf("%v %v", err, evs)
	}
	e := evs[0]
	if e.Type != "event.courier_update" || e.OccurredAt == nil || e.Tracking == nil || e.Tracking.Status != courier.StatusPickupPending ||
		!strings.Contains(string(e.Payload), `"lat":40.71093`) {
		t.Fatalf("%+v %+v", e, e.Tracking)
	}
}

// refund_request webhook body.
const refundHookBody = `{"created":"2023-07-06T05:09:07.173Z","data":{"created_at":"2023-07-06T05:09:07.173Z","currency_code":"GBP","id":"6817a070-115e-4ca9-8266-c245fe18a2d6","refund_fees":[{"category":"DELIVERY","fee_code":"UBER_DELIVERY_FEE","value":7500},{"category":"TAX","fee_code":"PARTNER_TAX","value":0}],"refund_order_items":[{"partner_refund_amount":4000,"party_at_fault":"UBER","reason":"UBER_DAMAGED_ITEM","refund_items":[{"name":"Donuts","quantity":1}],"uber_refund_amount":4000}],"total_partner_refund":4000,"total_uber_refund":11500},"delivery_id":"del_G_8_eeo4Q5WrYpYkATt6SA","external_id":"","external_store_id":"Store abc","id":"evt_nyNh-zBTR-uxdQjq5kYxhg","kind":"event.refund_request","live_mode":true}`

func TestWebhookRefundRequest(t *testing.T) {
	c := hookClient(t, twoKeys)
	evs, err := c.DecodeWebhook(context.Background(), signed("key-a", refundHookBody), []byte(refundHookBody))
	if err != nil || len(evs) != 1 || evs[0].Tracking != nil || evs[0].OccurredAt == nil {
		t.Fatalf("%v %+v", err, evs)
	}
	// Refund events are not delivery-status events: x-postmates-signature must not verify them.
	h := http.Header{}
	h.Set("X-Postmates-Signature", webhook.SignHMACSHA256([]byte("key-a"), []byte(refundHookBody)))
	if _, err := c.DecodeWebhook(context.Background(), h, []byte(refundHookBody)); err == nil {
		t.Fatal("postmates header must not verify refund events")
	}
	r, err := ParseRefundRequest(evs[0])
	if err != nil || r.Currency != "GBP" || r.TotalUberRefund.Amount.String() != "115" || r.TotalPartnerRefund.Amount.String() != "40" ||
		len(r.Fees) != 2 || r.Fees[1].Value.Amount.String() != "0" || r.Items[0].Reason != "UBER_DAMAGED_ITEM" ||
		r.Items[0].Items[0].Name != "Donuts" || r.StoreID != "Store abc" || r.CreatedAt == nil {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := ParseRefundRequest(courier.Event{Provider: ProviderID, Type: "event.delivery_status"}); err == nil {
		t.Fatal("wrong type accepted")
	}
}

// shopping_progress webhook body (note the nonsensical year in "created": it
// must not become a timestamp).
const shoppingHookBody = `{"created":"53927476-05-31T05:00:54.505Z","data":{"manifest_item":{"name":"Bud Light 12x 12oz Cans","price":1578,"quantity":1,"fulfilled_quantity":null,"sku":"098083535562"},"replacement_item":{"name":"Sierra Nevada Pale Ale 6x 12oz Cans","price":1130,"quantity":1,"sku":"083783535419"}},"delivery_id":"16a7622c-7acb-4641-8acb-2862fdb11452","id":"evt_ss69d6NfTwSWPJas2uCJxA","item_status":"suggested","kind":"event.shopping_progress"}`

func TestWebhookShoppingProgress(t *testing.T) {
	evs, err := hookClient(t, twoKeys).DecodeWebhook(context.Background(), signed("key-a", shoppingHookBody), []byte(shoppingHookBody))
	if err != nil || len(evs) != 1 || evs[0].Tracking != nil {
		t.Fatalf("%v %+v", err, evs)
	}
	s, err := ParseShoppingProgress(evs[0])
	if err != nil || s.ItemStatus != "suggested" || *s.Item.PriceMinor != 1578 || s.Item.FulfilledQuantity != nil ||
		s.Replacement == nil || s.Replacement.SKU != "083783535419" || *s.Replacement.PriceMinor != 1130 {
		t.Fatalf("%v %+v", err, s)
	}
	if evs[0].OccurredAt != nil {
		t.Fatal("garbage created must not become a time")
	}
}

// billing policy webhook body: uses type/timestamp instead of kind/created and
// carries no delivery.
const billingHookBody = `{"id":"d1458cc4-dc66-476d-b6a8-87f696646a2f","type":"direct.organization","schema_version":"2.0","timestamp":"2026-03-30T17:45:40.000Z","data":{"organization_uuid":"222516e9-b49d-4e9f-80dd-2c7b4c41e532","organization_name":"org","root_org_uuid":"6ab229b4","tenancy":"uber/testing/direct/6ab229b4","billing_status":"active","is_tax_form_completed":true,"changes":[{"attribute_name":"b2b_billing_info.policies_metadata","operation":"UPDATE","previous_value":"inactive","current_value":"active"}]}}`

func TestWebhookBillingPolicy(t *testing.T) {
	evs, err := hookClient(t, twoKeys).DecodeWebhook(context.Background(), signed("key-b", billingHookBody), []byte(billingHookBody))
	if err != nil || len(evs) != 1 {
		t.Fatalf("%v %+v", err, evs)
	}
	e := evs[0]
	if e.Type != "direct.organization" || e.Account != "acct-b" || e.OccurredAt == nil || e.Tracking != nil || e.ID != "d1458cc4-dc66-476d-b6a8-87f696646a2f" {
		t.Fatalf("%+v", e)
	}
	b, err := ParseBillingPolicyUpdate(e)
	if err != nil || b.BillingStatus != "active" || b.TaxFormCompleted == nil || !*b.TaxFormCompleted ||
		len(b.Changes) != 1 || b.Changes[0].Attribute != AttrBillingPolicy || b.Changes[0].Previous != "inactive" {
		t.Fatalf("%v %+v", err, b)
	}
	h := http.Header{}
	h.Set("X-Postmates-Signature", webhook.SignHMACSHA256([]byte("key-b"), []byte(billingHookBody)))
	if _, err := hookClient(t, twoKeys).DecodeWebhook(context.Background(), h, []byte(billingHookBody)); err == nil {
		t.Fatal("postmates header must not verify organization events")
	}
}
