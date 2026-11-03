package carrybee

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
)

func mustDec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func hook(t *testing.T, h courier.EventHandler) http.Handler {
	t.Helper()
	c, err := NewTenant(WithSandbox(), WithWebhookSecrets(map[courier.AccountID]auth.Secret{"acct-a": "sec-a", "acct-b": "sec-b"}))
	if err != nil {
		t.Fatal(err)
	}
	hh, err := c.WebhookHandler(h)
	if err != nil {
		t.Fatal(err)
	}
	return hh
}

func send(h http.Handler, secret, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/hook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if secret != "" {
		r.Header.Set(SignatureHeader, secret)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWebhookAuthAttributionAndEvents(t *testing.T) {
	var got []courier.Event
	h := hook(t, courier.EventHandlerFunc(func(_ context.Context, e courier.Event) error { got = append(got, e); return nil }))
	if w := send(h, "", `{"event":"order.picked","consignment_id":"CB-1"}`); w.Code == 200 || len(got) != 0 {
		t.Fatalf("missing secret accepted: %d", w.Code)
	}
	if w := send(h, "wrong", `{"event":"order.picked","consignment_id":"CB-1"}`); w.Code == 200 || len(got) != 0 {
		t.Fatalf("wrong secret accepted: %d", w.Code)
	}
	if w := send(h, "sec-b", `{"event":"order.delivered","consignment_id":"CB-1","merchant_order_id":"ORD-1","collected_amount":1250}`); w.Code != 200 || len(got) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	e := got[0]
	if e.Account != "acct-b" || e.Type != "order.delivered" || e.ID != "" || e.OccurredAt != nil || e.Tracking == nil ||
		e.Tracking.Status != courier.StatusDelivered || e.Tracking.COD != courier.CODCollected || e.Tracking.Ref.ID != "CB-1" {
		t.Fatalf("%+v", e)
	}
	got = nil
	send(h, "sec-a", `{"event":"order.delivery-failed","consignment_id":"CB-2","reason":"customer unreachable"}`)
	if got[0].Tracking.Status != courier.StatusDeliveryFailed || got[0].Tracking.ProviderStatus.Description != "customer unreachable" {
		t.Fatalf("%+v", got[0].Tracking)
	}
	got = nil
	send(h, "sec-a", `{"event":"order.paid","consignment_id":"CB-2","invoice_id":"INV-9"}`)
	if len(got) != 1 || got[0].Tracking != nil || got[0].Type != "order.paid" {
		t.Fatalf("paid event must carry no invented status: %+v", got)
	}
	if w := send(h, "sec-a", `{"consignment_id":"CB-2"}`); w.Code == 200 {
		t.Fatal("payload without event accepted")
	}
}

func TestWebhookFailsClosedWithoutSecrets(t *testing.T) {
	c, err := NewTenant(WithSandbox())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WebhookHandler(courier.EventHandlerFunc(func(context.Context, courier.Event) error { return nil })); !courier.IsKind(err, courier.KindConfiguration) {
		t.Fatalf("%v", err)
	}
	if _, err := c.DecodeWebhook(context.Background(), http.Header{SignatureHeader: {"x"}}, []byte(`{"event":"order.picked"}`)); !courier.IsKind(err, courier.KindConfiguration) {
		t.Fatalf("decode without secrets must fail closed: %v", err)
	}
}
