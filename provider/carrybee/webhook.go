package carrybee

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/httpx"
	"github.com/nokibsarkar/go-courier/webhook"
)

// SignatureHeader carries the webhook secret. The header value is compared
// directly with the configured secret: it is a shared secret, NOT an HMAC of
// the body. It authenticates the sender (and, with one
// secret per account, the account) but gives no body integrity and is
// replayable. Serve the callback over HTTPS only, use a distinct secret per
// account, and corroborate important events with TrackShipment (HOOK-05). This
// adapter fails closed when no secret is configured.
const SignatureHeader = "X-Carrybee-Webhook-Signature"

// WithWebhookSecrets registers the shared secret per account. Secrets must be
// non-empty and unique so a request is attributable to exactly one account.
func WithWebhookSecrets(secrets map[courier.AccountID]auth.Secret) Option {
	cp := make(map[courier.AccountID]auth.Secret, len(secrets))
	for k, v := range secrets {
		cp[k] = v
	}
	return func(s *settings) { s.hookSec = cp }
}

type hookPayload struct {
	Event           string          `json:"event"`
	ConsignmentID   json.RawMessage `json:"consignment_id"`
	MerchantOrderID string          `json:"merchant_order_id"`
	Reason          string          `json:"reason"`
	Attempt         json.RawMessage `json:"attempt"`
	CollectedAmount any             `json:"collected_amount"`
	InvoiceID       json.RawMessage `json:"invoice_id"`
}

func (c *Client) verify(h http.Header) (courier.AccountID, error) {
	if len(c.hookSec) == 0 {
		return "", courier.Configuration(ProviderID, "no webhook secrets configured; webhooks fail closed")
	}
	got := strings.TrimSpace(h.Get(SignatureHeader))
	if got == "" {
		return "", webhook.ErrInvalidSignature(ProviderID)
	}
	var match courier.AccountID
	n := 0
	for acct, sec := range c.hookSec { // constant-time over every secret
		if t := sec.Reveal(); t != "" && subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1 {
			match = acct
			n++
		}
	}
	if n != 1 {
		return "", webhook.ErrInvalidSignature(ProviderID)
	}
	return match, nil
}

// DecodeWebhook authenticates the callback and decodes it. The payload has no
// event ID or timestamp, so Event.ID is empty and OccurredAt nil rather than
// invented; dedupe on (consignment_id, event). Event.Type is the provider's
// event name, e.g. "order.delivered". Status events carry a TrackingResult;
// order.created and order.paid carry none.
func (c *Client) DecodeWebhook(_ context.Context, header http.Header, body []byte) ([]courier.Event, error) {
	acct, err := c.verify(header)
	if err != nil {
		return nil, err
	}
	var p hookPayload
	if err := httpx.DecodeJSON(ProviderID, "webhook.decode", body, &p); err != nil {
		return nil, err
	}
	ev := strings.TrimSpace(p.Event)
	if ev == "" {
		return nil, protocol("webhook.decode", "payload lacks event")
	}
	out := courier.Event{Provider: ProviderID, Account: acct, Trust: courier.TrustVerified, Type: ev,
		Payload: append(json.RawMessage(nil), body...)}
	id := scalarString(p.ConsignmentID)
	st := mapStatus(ev)
	if id == "" || !idPattern.MatchString(id) || st == courier.StatusUnknown {
		return []courier.Event{out}, nil // e.g. order.created / order.paid, or an unrecognised event
	}
	desc := strings.TrimSpace(p.Reason)
	res := &courier.TrackingResult{
		Ref:    courier.ShipmentRef{Provider: ProviderID, Account: acct, ID: courier.ShipmentID(id)},
		Status: st, ProviderStatus: courier.ProviderStatus{Code: ev, Description: desc},
		Coverage: courier.CoveragePartial, COD: courier.CODUnknown, ObservedAt: time.Now().UTC(), Raw: body,
		Events: []courier.TrackingEvent{{Status: st, ProviderStatus: courier.ProviderStatus{Code: ev, Description: desc}, Description: desc, Raw: body}},
	}
	if st == courier.StatusDelivered {
		res.Delivery = &courier.DeliveryConfirmation{}
		if d, ok, err := courier.ParseAmount(p.CollectedAmount); err == nil && ok && d.IsPositive() {
			res.COD = courier.CODCollected
		}
	}
	out.Tracking = res
	return []courier.Event{out}, nil
}

// WebhookHandler returns the standard handler for Carrybee callbacks. It fails
// closed when no secrets were configured and returns 500 if your handler
// errors so the provider may redeliver; handlers must be idempotent (the
// provider's retry and deduplication behaviour is undocumented).
func (c *Client) WebhookHandler(h courier.EventHandler) (http.Handler, error) {
	if len(c.hookSec) == 0 {
		return nil, courier.Configuration(ProviderID, "no webhook secrets configured; use WithWebhookSecrets")
	}
	return webhook.NewHandler(webhook.Config{Provider: ProviderID, Decode: c.DecodeWebhook, Handler: h, Logger: c.log})
}
