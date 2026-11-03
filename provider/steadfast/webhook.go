package steadfast

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/httpx"
	"github.com/nokibsarkar/go-courier/webhook"
)

// Steadfast POSTs JSON to your callback URL and waits up to 5 seconds for a 2xx.
// A 5xx or an unreachable endpoint is retried after 30 seconds and after 2
// minutes (three attempts in all); a 4xx is never retried. When an auth token is
// set it is sent as "Authorization: Bearer <token>" and every request is signed:
// X-Signature is the hex HMAC-SHA256 of the raw body keyed with that token. An
// Idempotency-Key header carries the same value on every retry of one event.
//
// This adapter requires both the bearer token and a valid signature, so a
// request is authenticated (and attributed to exactly one account) and its body
// is protected against alteration. Replays are possible: use Event.ID (the
// Idempotency-Key) to drop events you have already handled, and keep handlers
// idempotent (HOOK-05). Deliver webhooks over HTTPS only and keep tokens secret.

const (
	notifyDeliveryStatus = "delivery_status"
	notifyTrackingUpdate = "tracking_update"
)

var webhookAck = []byte(`{"status":"success","message":"Webhook received successfully."}`)

// WithWebhookTokens registers the bearer token per account. Tokens must be
// non-empty and unique so a request can be attributed to exactly one account
// (HOOK-02/05). Inbound verification never uses outbound credentials.
func WithWebhookTokens(tokens map[courier.AccountID]auth.Secret) Option {
	cp := make(map[courier.AccountID]auth.Secret, len(tokens))
	for k, v := range tokens {
		cp[k] = v
	}
	return func(s *settings) { s.webhookTokens = cp }
}

type webhookPayload struct {
	NotificationType string      `json:"notification_type"`
	ConsignmentID    json.Number `json:"consignment_id"`
	Invoice          string      `json:"invoice"`
	CODAmount        any         `json:"cod_amount"`
	Status           string      `json:"status"`
	DeliveryCharge   any         `json:"delivery_charge"`
	TrackingMessage  string      `json:"tracking_message"`
	UpdatedAt        string      `json:"updated_at"`
}

func (c *Client) verifyWebhook(h http.Header, body []byte) (courier.AccountID, error) {
	if len(c.webhookTokens) == 0 {
		return "", courier.Configuration(ProviderID, "no webhook tokens configured; webhooks fail closed")
	}
	got, ok := bearer(h.Get("Authorization"))
	if !ok {
		return "", webhook.ErrInvalidSignature(ProviderID)
	}
	var match courier.AccountID
	var key []byte
	n := 0
	for acct, tok := range c.webhookTokens { // constant-time over every token
		if t := tok.Reveal(); t != "" && subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1 {
			match, key = acct, []byte(t)
			n++
		}
	}
	if n != 1 { // none, or (defensively) ambiguous
		return "", webhook.ErrInvalidSignature(ProviderID)
	}
	if !webhook.VerifyHMACSHA256(key, body, strings.TrimSpace(h.Get("X-Signature"))) {
		return "", webhook.ErrInvalidSignature(ProviderID)
	}
	return match, nil
}

func bearer(v string) (string, bool) {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
		return "", false
	}
	return strings.TrimSpace(tok), true
}

// eventID is the Idempotency-Key, which is the same on every retry of one event;
// events of a batch body are told apart by position.
func eventID(h http.Header, k, n int) string {
	key := strings.TrimSpace(h.Get("Idempotency-Key"))
	if key == "" || n == 1 {
		return key
	}
	return key + "#" + strconv.Itoa(k)
}

// DecodeWebhook authenticates the request (bearer token and X-Signature) and
// decodes its events. Events are TrustVerified with the raw notification type
// and payload preserved, and Event.ID is the Idempotency-Key header (stable
// across retries). delivery_status and tracking_update carry a TrackingResult;
// the other notification types (consignment_update, return_list_accepted,
// payment_request, cancel_request, return_request, pickup_request,
// user_update) are passed through with their payload for you to interpret.
func (c *Client) DecodeWebhook(_ context.Context, header http.Header, body []byte) ([]courier.Event, error) {
	acct, err := c.verifyWebhook(header, body)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if t := strings.TrimSpace(string(body)); strings.HasPrefix(t, "[") {
		if err := httpx.DecodeJSON(ProviderID, "webhook.decode", body, &items); err != nil {
			return nil, err
		}
	} else {
		items = []json.RawMessage{json.RawMessage(body)}
	}
	if len(items) == 0 {
		return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "webhook.decode", Message: "empty event list"}
	}
	now := time.Now().UTC()
	events := make([]courier.Event, 0, len(items))
	for k, raw := range items {
		var p webhookPayload
		if err := httpx.DecodeJSON(ProviderID, "webhook.decode", raw, &p); err != nil {
			return nil, err
		}
		// notification_type is normally present. To stay lenient, a payload that
		// omits it but carries a status is treated as a delivery-status update;
		// one with neither is rejected.
		if strings.TrimSpace(p.NotificationType) == "" && strings.TrimSpace(p.Status) != "" {
			p.NotificationType = notifyDeliveryStatus
		}
		if strings.TrimSpace(p.NotificationType) == "" {
			return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "webhook.decode", Message: "event lacks notification_type"}
		}
		parcelEvent := p.NotificationType == notifyDeliveryStatus || p.NotificationType == notifyTrackingUpdate
		if parcelEvent && !digitsPattern.MatchString(p.ConsignmentID.String()) {
			return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "webhook.decode", Message: "event lacks a consignment_id"}
		}
		ev := courier.Event{Provider: ProviderID, Account: acct, Trust: courier.TrustVerified, ID: eventID(header, k, len(items)),
			Type: p.NotificationType, OccurredAt: parseTime(p.UpdatedAt), Payload: append(json.RawMessage(nil), raw...)}
		ref := courier.ShipmentRef{Provider: ProviderID, Account: acct, ID: courier.ShipmentID(p.ConsignmentID.String())}
		switch p.NotificationType {
		case notifyDeliveryStatus:
			if strings.TrimSpace(p.Status) == "" {
				return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "webhook.decode", Message: "delivery_status event lacks status"}
			}
			tr := mapStatus(acct, courier.TrackingRequest{}, p.Status, raw, now)
			tr.Ref = ref
			tr.ProviderStatus.Description = p.TrackingMessage
			ev.Tracking = tr
		case notifyTrackingUpdate:
			// A progress message with no status: kept as one event, status unknown.
			ev.Tracking = &courier.TrackingResult{
				Ref: ref, Status: courier.StatusUnknown, Coverage: courier.CoveragePartial, COD: courier.CODUnknown, ObservedAt: now, Raw: raw,
				Events: []courier.TrackingEvent{{OccurredAt: parseTime(p.UpdatedAt), Status: courier.StatusUnknown, Description: p.TrackingMessage, Raw: raw}},
			}
		}
		events = append(events, ev)
	}
	return events, nil
}

// WebhookHandler returns the standard handler for Steadfast notifications. It
// fails closed (a configuration error) when no tokens were configured, answers
// with the success body Steadfast expects, and returns 500 if your handler
// errors so the provider can redeliver. Handlers must be idempotent.
func (c *Client) WebhookHandler(h courier.EventHandler) (http.Handler, error) {
	if len(c.webhookTokens) == 0 {
		return nil, courier.Configuration(ProviderID, "no webhook tokens configured; use WithWebhookTokens")
	}
	return webhook.NewHandler(webhook.Config{Provider: ProviderID, Decode: c.DecodeWebhook, Handler: h, Logger: c.log, AckBody: webhookAck})
}

var (
	_ courier.WebhookHandlerProvider = (*Client)(nil)
	_ courier.WebhookDecoder         = (*Client)(nil)
)
