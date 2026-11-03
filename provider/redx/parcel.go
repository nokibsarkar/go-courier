package redx

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/shopspring/decimal"
)

// ParcelItem is an optional parcel_details_json entry.
type ParcelItem struct {
	Name     string
	Category string
	// Value is the declared value of the item, in whole taka.
	Value courier.Money
}

// OrderOptions carries RedX-specific create inputs with no canonical field.
// Set them per call with ContextWithOrderOptions. Delivery area and pickup
// store IDs come from location refs (see CreateShipment).
type OrderOptions struct {
	// IsClosedBox: true = customer cannot open the parcel before paying.
	// Left nil, the field is omitted (the provider default applies).
	IsClosedBox *bool
	// PaidReturnCharge is collected from the customer if the parcel is
	// returned; nil or omitted when not applicable.
	PaidReturnCharge *courier.Money
	// Type marks reverse shipments (provider-defined string).
	Type  string
	Items []ParcelItem
}

type optKey struct{}

// ContextWithOrderOptions attaches OrderOptions to ctx.
func ContextWithOrderOptions(ctx context.Context, o OrderOptions) context.Context {
	return context.WithValue(ctx, optKey{}, o)
}

func buildParcel(r courier.CreateShipmentRequest, o OrderOptions) (createBody, error) {
	var b createBody
	var err error
	if b.CustomerName, err = limited("recipient.name", r.Recipient.Contact.Name, 250); err != nil {
		return b, err
	}
	if b.CustomerPhone, err = localMobile("recipient.phone", r.Recipient.Contact.Phone); err != nil {
		return b, err
	}
	a := r.Recipient.Address
	if b.DeliveryArea, err = limited("recipient.address.area", a.Area, 100); err != nil {
		return b, err
	}
	areaID := refID(a.Refs, "area")
	if areaID == "" {
		return b, &courier.Error{Kind: courier.KindAmbiguous, Field: "recipient.address.refs",
			Message: `RedX needs an explicit delivery area: add LocationRef{Provider: "redx", Kind: "area", ID: "<id>"} (see ListAreas)`}
	}
	if b.DeliveryAreaID, err = strconv.ParseInt(areaID, 10, 64); err != nil || b.DeliveryAreaID <= 0 {
		return b, courier.Validation("recipient.address.refs", "redx area ID must be a positive integer")
	}
	addr := strings.TrimSpace(a.Text)
	if addr == "" {
		addr = strings.Join(nonEmpty(a.Line1, a.Line2), ", ")
	}
	if b.CustomerAddress, err = limited("recipient.address.text", addr, 1000); err != nil {
		return b, err
	}
	if id := strings.TrimSpace(string(r.MerchantOrderID)); id != "" {
		if b.MerchantInvoiceID, err = limited("merchant_order_id", id, 100); err != nil {
			return b, err
		}
	}
	b.CashCollectionAmount = "0" // required by the provider; "0" is its way to say no COD
	if r.COD != nil {
		if b.CashCollectionAmount, err = wholeTaka("cod", *r.COD); err != nil {
			return b, err
		}
	}
	if r.Package.WeightGrams == nil || *r.Package.WeightGrams == 0 {
		return b, courier.Validation("package.weight_grams", "required (sent in grams)")
	}
	b.ParcelWeight = *r.Package.WeightGrams
	if r.Package.DeclaredValue == nil {
		return b, courier.Validation("package.declared_value", "required by RedX for compensation; never defaulted")
	}
	v, err := wholeTaka("package.declared_value", *r.Package.DeclaredValue)
	if err != nil {
		return b, err
	}
	b.Value = json.Number(v)
	if n := strings.TrimSpace(r.Note); n != "" {
		b.Instruction = n
	}
	if st := refID(r.Pickup.Address.Refs, "pickup_store"); st != "" {
		id, err := strconv.ParseInt(st, 10, 64)
		if err != nil || id <= 0 {
			return b, courier.Validation("pickup.address.refs", "redx pickup store ID must be a positive integer")
		}
		b.PickupStoreID = &id
	}
	b.IsClosedBox = o.IsClosedBox
	b.Type = strings.TrimSpace(o.Type)
	if o.PaidReturnCharge != nil {
		s, err := wholeTaka("paid_return_charge", *o.PaidReturnCharge)
		if err != nil {
			return b, err
		}
		b.PaidReturnCharge = json.Number(s)
	}
	for i, it := range o.Items {
		name, err := limited("items.name", it.Name, 250)
		if err != nil {
			return b, err
		}
		cat, err := limited("items.category", it.Category, 100)
		if err != nil {
			return b, err
		}
		iv, err := wholeTaka("items.value", it.Value)
		if err != nil {
			return b, err
		}
		_ = i
		b.ParcelDetails = append(b.ParcelDetails, parcelDetail{Name: name, Category: cat, Value: json.Number(iv)})
	}
	return b, nil
}

func nonEmpty(v ...string) []string {
	var out []string
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// CreateShipment creates a parcel (POST /parcel). RedX-specific inputs come
// from location refs on the addresses — LocationRef{Provider:"redx",
// Kind:"area"} on the recipient (required) and Kind:"pickup_store" on the
// pickup — and from OrderOptions. Declared value and weight are required and
// never defaulted. RedX documents no duplicate protection; a timeout, 5xx or
// unusable 200 is indeterminate and the SDK never replays the create.
func (c *Client) CreateShipment(ctx context.Context, req courier.CreateShipmentRequest) (*courier.Shipment, error) {
	const op = "shipment.create"
	opts, _ := ctx.Value(optKey{}).(OrderOptions)
	body, err := buildParcel(req, opts)
	if err != nil {
		return nil, err
	}
	resp, creds, err := c.call(ctx, callSpec{op: op, method: http.MethodPost, path: "/parcel", payload: body, mutation: true})
	if err != nil {
		return nil, err
	}
	var out struct {
		TrackingID string `json:"tracking_id"`
	}
	if err := jsonDecode(op, resp.Body, &out); err != nil {
		return nil, asIndeterminate(err)
	}
	id := strings.TrimSpace(out.TrackingID)
	if !idPattern.MatchString(id) {
		return nil, asIndeterminate(protocol(op, "response lacks a valid tracking_id"))
	}
	sh := &courier.Shipment{
		Ref:             courier.ShipmentRef{Provider: ProviderID, Account: creds.AccountID, ID: courier.ShipmentID(id)},
		MerchantOrderID: courier.MerchantOrderID(body.MerchantInvoiceID),
		TrackingCode:    courier.TrackingCode(id),
		Status:          courier.StatusCreated, // the create response carries no status
		Raw:             resp.Body,
	}
	if req.COD != nil {
		m := *req.COD
		sh.COD = &m
	}
	return sh, nil
}

// TrackShipment tracks by RedX tracking ID (Code, LookupTrackingCode or
// LookupProviderShipmentID). Two reads are made: GET /parcel/info for the
// current status and COD amount, and GET /parcel/track for the message
// history. History entries carry messages only, so event statuses are unknown;
// the raw entry is kept. Merchant-order lookups are unsupported (no endpoint).
func (c *Client) TrackShipment(ctx context.Context, req courier.TrackingRequest) (*courier.TrackingResult, error) {
	const op = "tracking.get"
	if err := req.Validate(); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(req.Code))
	if id == "" {
		if v, ok := req.Find(courier.LookupTrackingCode); ok {
			id = v
		} else if v, ok := req.Find(courier.LookupProviderShipmentID); ok {
			id = v
		}
	}
	if id == "" {
		return nil, courier.Unsupported(ProviderID, op, "supported lookups: RedX tracking ID (code)")
	}
	if !idPattern.MatchString(id) {
		return nil, courier.Validation("code", "invalid RedX tracking ID")
	}
	info, creds, err := c.call(ctx, callSpec{op: op, method: http.MethodGet, path: "/parcel/info/" + id})
	if err != nil {
		return nil, err
	}
	var ip infoResponse
	if err := jsonDecode(op, info.Body, &ip); err != nil {
		return nil, err
	}
	if ip.Parcel == nil || strings.TrimSpace(ip.Parcel.Status) == "" {
		return nil, protocol(op, "info response lacks parcel.status")
	}
	hist, _, err := c.call(ctx, callSpec{op: op, method: http.MethodGet, path: "/parcel/track/" + id})
	if err != nil {
		return nil, err
	}
	var tr trackResponse
	if err := jsonDecode(op, hist.Body, &tr); err != nil {
		return nil, err
	}
	res := &courier.TrackingResult{
		Ref:            courier.ShipmentRef{Provider: ProviderID, Account: creds.AccountID, ID: courier.ShipmentID(id)},
		TrackingCode:   courier.TrackingCode(id),
		ProviderStatus: courier.ProviderStatus{Code: strings.TrimSpace(ip.Parcel.Status)},
		Status:         mapStatus(ip.Parcel.Status),
		Coverage:       courier.CoverageFull,
		COD:            courier.CODUnknown, // the amount is known, its collection state is not
		ObservedAt:     time.Now().UTC(),
		Raw:            info.Body,
	}
	for _, e := range tr.Tracking {
		raw, _ := json.Marshal(e)
		res.Events = append(res.Events, courier.TrackingEvent{OccurredAt: parseTime(e.Time), Status: courier.StatusUnknown,
			Description: strings.TrimSpace(e.MessageEN), Raw: raw})
	}
	if len(res.Events) == 0 {
		res.Coverage = courier.CoverageCurrentOnly
	}
	if res.Status == courier.StatusDelivered {
		res.Delivery = &courier.DeliveryConfirmation{}
	}
	return res, nil
}

// CancelShipment asks RedX to cancel a parcel (PATCH /parcels with status
// "cancelled"). The provider answers "Request Accepted": the request was
// accepted, which is not proof the parcel is cancelled yet. Confirm with
// TrackShipment. success=false is a confirmed rejection.
func (c *Client) CancelShipment(ctx context.Context, ref courier.ShipmentRef) error {
	return c.CancelParcel(ctx, string(ref.ID), "", ref.Provider)
}

// CancelParcel is CancelShipment with an optional reason (logged by RedX).
func (c *Client) CancelParcel(ctx context.Context, trackingID, reason string, provider courier.ProviderID) error {
	const op = "shipment.cancel"
	if provider != "" && provider != ProviderID {
		return courier.Validation("ref.provider", "not a redx shipment")
	}
	id := strings.TrimSpace(trackingID)
	if !idPattern.MatchString(id) {
		return courier.Validation("tracking_id", "invalid RedX tracking ID")
	}
	b := updateBody{EntityType: "parcel-tracking-id", EntityID: id,
		UpdateDetails: updateDetails{PropertyName: "status", NewValue: "cancelled", Reason: truncate(reason, 255)}}
	resp, _, err := c.call(ctx, callSpec{op: op, method: http.MethodPatch, path: "/parcels", payload: b, mutation: true})
	if err != nil {
		return err
	}
	var out struct {
		Success *bool  `json:"success"`
		Message string `json:"message"`
	}
	if err := jsonDecode(op, resp.Body, &out); err != nil {
		return asIndeterminate(err)
	}
	switch {
	case out.Success != nil && *out.Success:
		return nil
	case out.Success != nil:
		return &courier.Error{Kind: courier.KindRejected, Provider: ProviderID, Op: op, Outcome: courier.OutcomeRejected, Message: truncate(out.Message, 300)}
	}
	return asIndeterminate(protocol(op, "response does not state whether the request was accepted"))
}

var _ = decimal.Zero
