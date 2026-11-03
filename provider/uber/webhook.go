package uber

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/webhook"
)

const (
	hdrUber     = "x-uber-signature"
	hdrPostmate = "x-postmates-signature"

	evDeliveryStatus = "event.delivery_status"
	evCourierUpdate  = "event.courier_update"
	evRefundRequest  = "event.refund_request"
	evShopping       = "event.shopping_progress"
	evOrganization   = "direct.organization"
)

// WebhookHandler returns the standard handler for Uber webhooks (HOOK-01).
// It requires WithWebhookSigningKeys; verification uses no outbound
// credentials (HOOK-02). Uber retries only on 500/502/503/504, timeouts and
// network errors (up to 3 events), so an application error yields 500 and a
// rejected signature 401 (not retried).
func (c *Client) WebhookHandler(h courier.EventHandler) (http.Handler, error) {
	if len(c.hookKeys) == 0 {
		return nil, courier.Configuration(ProviderID, "webhook signing keys are required (WithWebhookSigningKeys)")
	}
	return webhook.NewHandler(webhook.Config{Provider: ProviderID, Decode: c.DecodeWebhook, Handler: h, Logger: c.log})
}

// DecodeWebhook verifies the HMAC-SHA256 signature over the original body
// and decodes the events. Events are TrustVerified with Account set only
// when exactly one configured key verifies the signature; no match is an
// invalid-signature error and more than one match is ErrAmbiguous (the
// tenant is never guessed). x-postmates-signature is honoured only for
// delivery-status and courier-update events.
func (c *Client) DecodeWebhook(_ context.Context, header http.Header, body []byte) ([]courier.Event, error) {
	if len(c.hookKeys) == 0 {
		return nil, courier.Configuration(ProviderID, "webhook signing keys are required (WithWebhookSigningKeys)")
	}
	if len(body) > webhook.DefaultMaxBody {
		return nil, &courier.Error{Kind: courier.KindResourceLimit, Provider: ProviderID, Op: webhookDecodeOp, Message: "webhook body too large"}
	}
	sigs := append([]string(nil), header.Values(hdrUber)...)
	if peekAllowsPostmates(body) {
		sigs = append(sigs, header.Values(hdrPostmate)...)
	}
	accounts := make([]string, 0, len(c.hookKeys))
	for a := range c.hookKeys {
		accounts = append(accounts, string(a))
	}
	sort.Strings(accounts)
	var matched []courier.AccountID
	for _, a := range accounts { // evaluate every key; no early exit
		key := []byte(c.hookKeys[courier.AccountID(a)].Reveal())
		ok := false
		for _, s := range sigs {
			if webhook.VerifyHMACSHA256(key, body, s) {
				ok = true
			}
		}
		if ok {
			matched = append(matched, courier.AccountID(a))
		}
	}
	switch len(matched) {
	case 0:
		return nil, webhook.ErrInvalidSignature(ProviderID)
	case 1:
	default:
		return nil, &courier.Error{Kind: courier.KindAmbiguous, Provider: ProviderID, Op: "webhook.verify",
			Message: "signature verifies under more than one configured account key; refusing to attribute"}
	}
	return decodeEvents(matched[0], body)
}

func splitEvents(body []byte) ([]json.RawMessage, error) {
	t := bytes.TrimSpace(body)
	if len(t) > 0 && t[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(t, &arr); err != nil || len(arr) == 0 {
			return nil, protocol(webhookDecodeOp, "malformed event batch")
		}
		return arr, nil
	}
	if len(t) == 0 || t[0] != '{' {
		return nil, protocol(webhookDecodeOp, "malformed event")
	}
	return []json.RawMessage{json.RawMessage(t)}, nil
}

func eventType(e hookEvent) string {
	if e.Kind != "" {
		return e.Kind
	}
	return firstNonEmpty(e.EventType, e.Type)
}

// peekAllowsPostmates inspects the body only to decide
// which header may carry the signature; nothing is dispatched from it.
func peekAllowsPostmates(body []byte) bool {
	els, err := splitEvents(body)
	if err != nil {
		return false
	}
	for _, el := range els {
		var e hookEvent
		if json.Unmarshal(el, &e) != nil {
			return false
		}
		if t := eventType(e); t != evDeliveryStatus && t != evCourierUpdate {
			return false
		}
	}
	return true
}

func decodeEvents(acct courier.AccountID, body []byte) ([]courier.Event, error) {
	els, err := splitEvents(body)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]courier.Event, 0, len(els))
	for _, el := range els {
		var e hookEvent
		if err := json.Unmarshal(el, &e); err != nil {
			return nil, protocol(webhookDecodeOp, "malformed event")
		}
		typ := eventType(e)
		if typ == "" {
			return nil, protocol(webhookDecodeOp, "event lacks a type")
		}
		var data hookData
		if hasValue(e.Data) {
			_ = json.Unmarshal(e.Data, &data) // lenient: data shape is unverified
		}
		ev := courier.Event{Provider: ProviderID, Account: acct, Trust: courier.TrustVerified, ID: e.ID, Type: typ,
			Payload: append(json.RawMessage(nil), el...)}
		var created string
		if hasValue(e.Created) {
			_ = json.Unmarshal(e.Created, &created)
		}
		if created == "" {
			created = e.Timestamp
		}
		if t := parseTime(created); t != nil {
			ev.OccurredAt = t
		} else {
			ev.OccurredAt = parseTime(data.Updated)
		}
		if typ == evDeliveryStatus || typ == evCourierUpdate {
			// courier_update repeats the full delivery in data (every ~20s while
			// a courier is assigned); only the current status is surfaced.
			id := firstNonEmpty(e.DeliveryID, data.ID)
			status := firstNonEmpty(e.Status, data.Status)
			if id != "" && status != "" && idPattern.MatchString(id) {
				ev.Tracking = trackingResult(acct, id, id, status, append([]byte(nil), el...), now)
			}
		}
		out = append(out, ev)
	}
	return out, nil
}

func firstNonEmpty(a, b string) string {
	if a = strings.TrimSpace(a); a != "" {
		return a
	}
	return strings.TrimSpace(b)
}
