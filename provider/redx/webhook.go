package redx

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

// RedX webhooks carry NO signature. The documented scheme is a credential in
// the callback URL's query string ("https://example.com/callback?token=<token>").
// That is a shared secret, not integrity protection: the body can be altered
// by anyone who can reach the endpoint with the token, events are replayable,
// and the token appears in access logs and proxies. Serve the callback over
// HTTPS only, keep access logs from recording the query string, use a distinct
// token per account, and corroborate important events with TrackShipment before
// acting on them (HOOK-05).

// TokenHeader is the internal header the handler uses to hand the URL token to
// DecodeWebhook. WebhookHandler removes any client-supplied copy first.
// Callers who terminate the callback themselves may set it directly.
const TokenHeader = "X-Courier-Redx-Token"

// WithWebhookTokens registers the callback token per account. Tokens must be
// non-empty and unique so a request is attributable to exactly one account.
func WithWebhookTokens(tokens map[courier.AccountID]auth.Secret) Option {
	cp := make(map[courier.AccountID]auth.Secret, len(tokens))
	for k, v := range tokens {
		cp[k] = v
	}
	return func(s *settings) { s.hookTok = cp }
}

// WithWebhookTokenParam names the query parameter that carries the token
// (default "token"; the merchant chooses the URL).
func WithWebhookTokenParam(name string) Option {
	return func(s *settings) { s.hookParam = strings.TrimSpace(name) }
}

type hookPayload struct {
	TrackingNumber string `json:"tracking_number"`
	Timestamp      string `json:"timestamp"`
	Status         string `json:"status"`
	MessageEN      string `json:"message_en"`
	MessageBN      string `json:"message_bn"`
	InvoiceNumber  string `json:"invoice_number"`
	DeliveryType   string `json:"delivery_type"`
}

// EventParcelStatus is the Event.Type of RedX status callbacks (the callback
// has no event-type field; this is the SDK's label).
const EventParcelStatus = "parcel.status"

func (c *Client) verify(h http.Header) (courier.AccountID, error) {
	if len(c.hookTok) == 0 {
		return "", courier.Configuration(ProviderID, "no webhook tokens configured; webhooks fail closed")
	}
	got := strings.TrimSpace(h.Get(TokenHeader))
	if got == "" {
		return "", webhook.ErrInvalidSignature(ProviderID)
	}
	var match courier.AccountID
	n := 0
	for acct, tok := range c.hookTok { // constant-time over every token
		if t := tok.Reveal(); t != "" && subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1 {
			match = acct
			n++
		}
	}
	if n != 1 {
		return "", webhook.ErrInvalidSignature(ProviderID)
	}
	return match, nil
}

// DecodeWebhook authenticates the callback (token in TokenHeader) and decodes
// it. Events are TrustVerified for sender and account but carry no integrity
// guarantee; see the note above. The callback has no event ID, so Event.ID is
// empty rather than invented; use (tracking_number, status, timestamp) to dedupe.
func (c *Client) DecodeWebhook(_ context.Context, header http.Header, body []byte) ([]courier.Event, error) {
	acct, err := c.verify(header)
	if err != nil {
		return nil, err
	}
	var p hookPayload
	if err := httpx.DecodeJSON(ProviderID, webhookDecodeOp, body, &p); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(p.TrackingNumber)
	if !idPattern.MatchString(id) || strings.TrimSpace(p.Status) == "" {
		return nil, protocol(webhookDecodeOp, "event lacks tracking_number or status")
	}
	now := time.Now().UTC()
	at := parseTime(p.Timestamp)
	desc := strings.TrimSpace(p.MessageEN)
	status := strings.TrimSpace(p.Status)
	ref := courier.ShipmentRef{Provider: ProviderID, Account: acct, ID: courier.ShipmentID(id)}
	res := &courier.TrackingResult{
		Ref: ref, TrackingCode: courier.TrackingCode(id), Status: mapStatus(status),
		ProviderStatus: courier.ProviderStatus{Code: status, Description: desc},
		Coverage:       courier.CoveragePartial, COD: courier.CODUnknown, ObservedAt: now, Raw: body,
		Events: []courier.TrackingEvent{{OccurredAt: at, Status: mapStatus(status),
			ProviderStatus: courier.ProviderStatus{Code: status, Description: desc}, Description: desc, Raw: body}},
	}
	if res.Status == courier.StatusDelivered {
		res.Delivery = &courier.DeliveryConfirmation{DeliveredAt: at}
	}
	return []courier.Event{{Provider: ProviderID, Account: acct, Trust: courier.TrustVerified, Type: EventParcelStatus,
		OccurredAt: at, Tracking: res, Payload: append(json.RawMessage(nil), body...)}}, nil
}

// WebhookHandler returns the standard handler for RedX callbacks. It reads the
// token from the configured query parameter, drops any client-supplied
// TokenHeader, and fails closed when no tokens were configured. It returns 500
// if your handler errors so RedX may redeliver; handlers must be idempotent.
func (c *Client) WebhookHandler(h courier.EventHandler) (http.Handler, error) {
	if len(c.hookTok) == 0 {
		return nil, courier.Configuration(ProviderID, "no webhook tokens configured; use WithWebhookTokens")
	}
	inner, err := webhook.NewHandler(webhook.Config{Provider: ProviderID, Decode: c.DecodeWebhook, Handler: h, Logger: c.log})
	if err != nil {
		return nil, err
	}
	param := c.hookParam
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.Header.Del(TokenHeader)
		if tok := r.URL.Query().Get(param); tok != "" {
			r2.Header.Set(TokenHeader, tok)
		}
		inner.ServeHTTP(w, r2)
	}), nil
}
