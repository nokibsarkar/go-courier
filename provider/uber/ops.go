package uber

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/httpx"
)

// asIndeterminate marks an unusable response to a state-changing request as
// indeterminate: the request was sent, so the effect cannot be excluded.
func asIndeterminate(err error) error {
	return &courier.Error{Kind: courier.KindIndeterminate, Provider: ProviderID, Outcome: courier.OutcomeIndeterminate,
		Message: "unusable response to a state-changing request; the operation may have been applied", Err: err}
}

func checkDeliveryID(field, id string) error {
	if !idPattern.MatchString(id) {
		return courier.Validation(field, "invalid delivery ID")
	}
	return nil
}

func refID(ref courier.ShipmentRef) (string, error) {
	if ref.Provider != "" && ref.Provider != ProviderID {
		return "", courier.Validation("ref.provider", "reference belongs to a different provider")
	}
	id := strings.TrimSpace(string(ref.ID))
	return id, checkDeliveryID("ref.id", id)
}

// Quote requests a price. It never creates a delivery (CAP-02); the quote
// merely reserves a short-lived price.
func (c *Client) Quote(ctx context.Context, req courier.QuoteRequest) (*courier.Quote, error) {
	const op = "quote.create"
	body, err := buildQuote(req)
	if err != nil {
		return nil, err
	}
	// A quote is a priced lookup, not a booking: retry-safe.
	resp, creds, err := c.call(ctx, op, http.MethodPost, "delivery_quotes", body, false, true, "")
	if err != nil {
		return nil, err
	}
	var out quoteResponse
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &out); err != nil {
		return nil, err
	}
	if err := embeddedFailure(op, out.Kind, out.Code, out.ID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.ID) == "" {
		return nil, protocol(op, "response lacks a quote id")
	}
	cur, err := providerCurrency(op, out.CurrencyType, out.Currency)
	if err != nil {
		return nil, err
	}
	fee, present, err := minorMoney(op, out.Fee, cur)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, protocol(op, "response lacks a fee")
	}
	return &courier.Quote{
		Ref:       courier.ShipmentRef{Provider: ProviderID, Account: creds.account()},
		ID:        courier.QuoteID(out.ID),
		Fee:       *fee,
		ExpiresAt: parseTime(out.Expires),
		Raw:       resp.Body,
	}, nil
}

// CreateShipment books a delivery. It is an explicit purchase: when
// req.QuoteID is empty no quote is fetched on the caller's behalf and the
// delivery is created without a quote_id. Timeouts and 5xx are indeterminate;
// the request is never retried automatically.
func (c *Client) CreateShipment(ctx context.Context, req courier.CreateShipmentRequest) (*courier.Shipment, error) {
	const op = "shipment.create"
	body, err := buildCreate(req)
	if err != nil {
		return nil, err
	}
	resp, creds, err := c.call(ctx, op, http.MethodPost, "deliveries", body, true, false, "")
	if err != nil {
		return nil, err
	}
	var out deliveryResponse
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &out); err != nil {
		return nil, asIndeterminate(err)
	}
	if err := embeddedFailure(op, out.Kind, out.Code, out.ID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.ID) == "" {
		return nil, asIndeterminate(protocol(op, "response lacks a delivery id"))
	}
	sh, err := mapDelivery(creds.account(), &out, resp.Body)
	if err != nil {
		return nil, asIndeterminate(err)
	}
	return sh, nil
}

func (c *Client) getDelivery(ctx context.Context, op, id string, acct courier.AccountID) (*deliveryResponse, *httpx.Response, Credentials, error) {
	resp, creds, err := c.call(ctx, op, http.MethodGet, "deliveries/"+url.PathEscape(id), nil, false, false, acct)
	if err != nil {
		return nil, nil, creds, err
	}
	var out deliveryResponse
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &out); err != nil {
		return nil, nil, creds, err
	}
	if err := embeddedFailure(op, out.Kind, out.Code, out.ID); err != nil {
		return nil, nil, creds, err
	}
	if out.ID != id {
		return nil, nil, creds, protocol(op, "response delivery id does not match the request")
	}
	return &out, resp, creds, nil
}

// GetShipment fetches a delivery by reference. ref.Account, when set, must
// match the active credentials' account.
func (c *Client) GetShipment(ctx context.Context, ref courier.ShipmentRef) (*courier.Shipment, error) {
	id, err := refID(ref)
	if err != nil {
		return nil, err
	}
	out, resp, creds, err := c.getDelivery(ctx, "shipment.get", id, ref.Account)
	if err != nil {
		return nil, err
	}
	return mapDelivery(creds.account(), out, resp.Body)
}

// TrackShipment reports the current delivery status. Uber exposes no event
// history, so the result is CoverageCurrentOnly with no events. The Uber
// delivery ID is accepted as Code or as a LookupProviderShipmentID lookup.
func (c *Client) TrackShipment(ctx context.Context, req courier.TrackingRequest) (*courier.TrackingResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(req.Code))
	if id == "" {
		v, ok := req.Find(courier.LookupProviderShipmentID)
		if !ok {
			return nil, courier.Unsupported(ProviderID, "tracking.get", "supported lookups: tracking code or provider shipment ID (the Uber delivery ID)")
		}
		id = v
	}
	if err := checkDeliveryID("code", id); err != nil {
		return nil, err
	}
	out, resp, creds, err := c.getDelivery(ctx, "tracking.get", id, req.Account)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.Status) == "" {
		return nil, protocol("tracking.get", "response lacks status")
	}
	return trackingFromDelivery(creds.account(), id, out, resp.Body, time.Now().UTC()), nil
}

// CancelShipment cancels a delivery. Timeouts, 5xx and unusable responses
// are indeterminate; the request is never retried automatically.
func (c *Client) CancelShipment(ctx context.Context, ref courier.ShipmentRef) error {
	const op = "shipment.cancel"
	id, err := refID(ref)
	if err != nil {
		return err
	}
	resp, _, err := c.call(ctx, op, http.MethodPost, "deliveries/"+url.PathEscape(id)+"/cancel", nil, true, false, ref.Account)
	if err != nil {
		return err
	}
	var out deliveryResponse
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &out); err != nil {
		return asIndeterminate(err)
	}
	if err := embeddedFailure(op, out.Kind, out.Code, out.ID); err != nil {
		return err
	}
	if mapStatus(out.Status) != courier.StatusCancelled {
		return asIndeterminate(protocol(op, "response does not confirm cancellation"))
	}
	return nil
}

// DeliveryFilter narrows ListDeliveries; zero fields are omitted.
type DeliveryFilter struct {
	// State is one of pending, pickup, pickup_complete, dropoff, delivered, canceled, returned.
	State           string
	ExternalStoreID string
	Start, End      *time.Time
	Limit, Offset   int
}

// DeliveryPage is one page of deliveries. NextHref is Uber's link to the next
// page (empty when none); total_count is deprecated upstream and ignored.
type DeliveryPage struct {
	Shipments []courier.Shipment
	NextHref  string
}

var deliveryStates = map[string]bool{"pending": true, "pickup": true, "pickup_complete": true, "dropoff": true, "delivered": true, "canceled": true, "returned": true}

// ListDeliveries lists deliveries for the customer (GET deliveries).
func (c *Client) ListDeliveries(ctx context.Context, f DeliveryFilter) (*DeliveryPage, error) {
	const op = "shipment.list"
	q := url.Values{}
	if f.State != "" {
		if !deliveryStates[f.State] {
			return nil, courier.Validation("state", "unknown delivery state")
		}
		q.Set("filter", f.State)
	}
	if f.ExternalStoreID != "" {
		q.Set("external_store_id", f.ExternalStoreID)
	}
	if f.Start != nil {
		q.Set("start_dt", f.Start.UTC().Format(time.RFC3339))
	}
	if f.End != nil {
		q.Set("end_dt", f.End.UTC().Format(time.RFC3339))
	}
	if f.Limit < 0 || f.Offset < 0 {
		return nil, courier.Validation("limit", "limit and offset must not be negative")
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Offset > 0 {
		q.Set("Offset", strconv.Itoa(f.Offset)) // the spec's parameter name is capitalised
	}
	path := "deliveries"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	resp, creds, err := c.call(ctx, op, http.MethodGet, path, nil, false, false, "")
	if err != nil {
		return nil, err
	}
	var out listResponse
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &out); err != nil {
		return nil, err
	}
	page := &DeliveryPage{NextHref: out.NextHref, Shipments: make([]courier.Shipment, 0, len(out.Data))}
	for _, raw := range out.Data {
		var d deliveryResponse
		if err := httpx.DecodeJSON(ProviderID, op, raw, &d); err != nil {
			return nil, err
		}
		sh, err := mapDelivery(creds.account(), &d, raw)
		if err != nil {
			return nil, err // never silently skip a malformed record
		}
		page.Shipments = append(page.Shipments, *sh)
	}
	return page, nil
}

// ProofWaypoint and ProofType are the values accepted by Proof of Delivery.
type (
	ProofWaypoint string
	ProofType     string
)

const (
	ProofPickup  ProofWaypoint = "pickup"
	ProofDropoff ProofWaypoint = "dropoff"
	ProofReturn  ProofWaypoint = "return"

	ProofPicture   ProofType = "picture"
	ProofSignature ProofType = "signature"
	ProofPincode   ProofType = "pincode"
)

// GetProofOfDeliveryDocument fetches the proof image Uber renders for a
// delivery created with a verification requirement. The response is a Base64
// PNG (per the spec); it is decoded into Document.Body (the caller closes it).
// This is a POST that only reads, so it is not auto-retried (TRK-13). It is not
// the generic courier.ProofOfDeliveryReader because the waypoint and proof type
// are required inputs Uber does not default.
func (c *Client) GetProofOfDeliveryDocument(ctx context.Context, ref courier.ShipmentRef, wp ProofWaypoint, kind ProofType) (*courier.Document, error) {
	const op = "proof.get"
	id, err := refID(ref)
	if err != nil {
		return nil, err
	}
	switch wp {
	case ProofPickup, ProofDropoff, ProofReturn:
	default:
		return nil, courier.Validation("waypoint", "must be pickup, dropoff or return")
	}
	switch kind {
	case ProofPicture, ProofSignature, ProofPincode:
	default:
		return nil, courier.Validation("type", "must be picture, signature or pincode")
	}
	resp, _, err := c.call(ctx, op, http.MethodPost, "deliveries/"+url.PathEscape(id)+"/proof-of-delivery",
		podRequest{Waypoint: string(wp), Type: string(kind)}, false, false, ref.Account)
	if err != nil {
		return nil, err
	}
	var out podResponse
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.Document) == "" {
		return nil, protocol(op, "response lacks a document")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out.Document))
	if err != nil {
		return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: op, Message: "document is not valid Base64", Err: err}
	}
	return &courier.Document{ContentType: "image/png", Body: io.NopCloser(bytes.NewReader(raw))}, nil
}
