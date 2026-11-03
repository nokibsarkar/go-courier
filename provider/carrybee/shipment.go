package carrybee

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// OrderOptions carries Carrybee inputs with no canonical field. Set them per
// call with ContextWithOrderOptions. The meaning of the integer codes is not
// documented, so the SDK never defaults them: a zero value omits the field and the provider decides.
type OrderOptions struct {
	DeliveryType int
	ProductType  int
}

type optKey struct{}

// ContextWithOrderOptions attaches OrderOptions to ctx.
func ContextWithOrderOptions(ctx context.Context, o OrderOptions) context.Context {
	return context.WithValue(ctx, optKey{}, o)
}

type createBody struct {
	StoreID            string `json:"store_id"`
	MerchantOrderID    string `json:"merchant_order_id,omitempty"`
	DeliveryType       int    `json:"delivery_type,omitempty"`
	ProductType        int    `json:"product_type,omitempty"`
	RecipientPhone     string `json:"recipient_phone"`
	RecipientName      string `json:"recipient_name"`
	RecipientAddress   string `json:"recipient_address"`
	CityID             int64  `json:"city_id"`
	ZoneID             int64  `json:"zone_id"`
	AreaID             *int64 `json:"area_id,omitempty"`
	ItemWeight         uint32 `json:"item_weight"` // grams
	ItemQuantity       int    `json:"item_quantity"`
	CollectableAmount  *int64 `json:"collectable_amount,omitempty"`
	ProductDescription string `json:"product_description,omitempty"`
}

func positive(field, v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, courier.Validation(field, "must be a positive integer")
	}
	return n, nil
}

func buildOrder(r courier.CreateShipmentRequest, o OrderOptions) (createBody, error) {
	var b createBody
	var err error
	store := refID(r.Pickup.Address.Refs, "store")
	if store == "" {
		return b, &courier.Error{Kind: courier.KindAmbiguous, Field: "pickup.address.refs",
			Message: `Carrybee needs a store: add LocationRef{Provider: "carrybee", Kind: "store", ID: "<store id>"} on the pickup address (see ListStores)`}
	}
	if !idPattern.MatchString(store) {
		return b, courier.Validation("pickup.address.refs", "invalid carrybee store ID")
	}
	b.StoreID = store
	a := r.Recipient.Address
	for _, k := range []struct {
		kind string
		dst  *int64
	}{{"city", &b.CityID}, {"zone", &b.ZoneID}} {
		id := refID(a.Refs, k.kind)
		if id == "" {
			return b, &courier.Error{Kind: courier.KindAmbiguous, Field: "recipient.address.refs",
				Message: `Carrybee needs explicit city and zone: add LocationRef{Provider: "carrybee", Kind: "city"|"zone", ID: "<id>"} (see ListCities and ListZones)`}
		}
		if *k.dst, err = positive("recipient.address.refs."+k.kind, id); err != nil {
			return b, err
		}
	}
	if id := refID(a.Refs, "area"); id != "" {
		n, err := positive("recipient.address.refs.area", id)
		if err != nil {
			return b, err
		}
		b.AreaID = &n
	}
	if b.RecipientName, err = required("recipient.name", r.Recipient.Contact.Name, 250); err != nil {
		return b, err
	}
	if b.RecipientPhone, err = localMobile("recipient.phone", r.Recipient.Contact.Phone); err != nil {
		return b, err
	}
	addr := strings.TrimSpace(a.Text)
	if addr == "" {
		addr = strings.TrimSpace(strings.Join(nonEmpty(a.Line1, a.Line2), ", "))
	}
	if b.RecipientAddress, err = required("recipient.address.text", addr, 1000); err != nil {
		return b, err
	}
	if id := strings.TrimSpace(string(r.MerchantOrderID)); id != "" {
		if b.MerchantOrderID, err = required("merchant_order_id", id, 100); err != nil {
			return b, err
		}
	}
	if r.Package.WeightGrams == nil || *r.Package.WeightGrams == 0 {
		return b, courier.Validation("package.weight_grams", "required (item_weight is sent in grams); never defaulted")
	}
	b.ItemWeight = *r.Package.WeightGrams
	if r.Package.Quantity < 1 {
		return b, courier.Validation("package.quantity", "required (at least 1)")
	}
	b.ItemQuantity = r.Package.Quantity
	if d := strings.TrimSpace(r.Package.Description); d != "" {
		b.ProductDescription = d
	}
	if r.COD != nil {
		n, err := wholeTaka("cod", *r.COD)
		if err != nil {
			return b, err
		}
		b.CollectableAmount = &n
	}
	b.DeliveryType, b.ProductType = o.DeliveryType, o.ProductType
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

type orderEnvelope struct {
	Data struct {
		Order struct {
			ConsignmentID json.RawMessage `json:"consignment_id"`
			StoreID       json.RawMessage `json:"store_id"`
			DeliveryFee   json.RawMessage `json:"delivery_fee"`
			CODFee        json.RawMessage `json:"cod_fee"`
		} `json:"order"`
	} `json:"data"`
}

func scalarString(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// CreateShipment creates an order (POST /api/v2/orders). Store, city and zone
// come from location refs (Kind "store" on the pickup address, "city", "zone"
// and optionally "area" on the recipient); weight in grams and quantity are
// required and never defaulted. No duplicate protection is documented, so a
// timeout, 5xx or unusable 200 is indeterminate and the SDK never replays a
// create; reconcile with TrackShipment or your own records first. The create
// response carries the delivery and COD fees but no status.
func (c *Client) CreateShipment(ctx context.Context, req courier.CreateShipmentRequest) (*courier.Shipment, error) {
	const op = "shipment.create"
	opts, _ := ctx.Value(optKey{}).(OrderOptions)
	body, err := buildOrder(req, opts)
	if err != nil {
		return nil, err
	}
	resp, creds, err := c.call(ctx, callSpec{op: op, method: http.MethodPost, path: "/orders", payload: body, mutation: true})
	if err != nil {
		return nil, err
	}
	var out orderEnvelope
	if err := jsonDecode(op, resp.Body, &out); err != nil {
		return nil, asIndeterminate(err)
	}
	id := scalarString(out.Data.Order.ConsignmentID)
	if !idPattern.MatchString(id) {
		return nil, asIndeterminate(protocol(op, "response lacks data.order.consignment_id"))
	}
	sh := &courier.Shipment{
		Ref:             courier.ShipmentRef{Provider: ProviderID, Account: creds.AccountID, ID: courier.ShipmentID(id)},
		MerchantOrderID: courier.MerchantOrderID(body.MerchantOrderID),
		Status:          courier.StatusCreated,
		Raw:             resp.Body,
	}
	if req.COD != nil {
		m := *req.COD
		sh.COD = &m
	}
	if d, ok, err := courier.ParseAmount(out.Data.Order.DeliveryFee); err == nil && ok {
		m := courier.NewBDT(d)
		sh.DeliveryCharge = &m
	}
	return sh, nil
}

// TrackShipment reads GET /api/v2/orders/{id}/details: transfer_status,
// collected_amount and attempt. The transfer_status vocabulary is not
// documented, so unrecognised values stay unknown with the raw value kept.
// Only the current state is available. Lookups: consignment ID (Code,
// LookupProviderShipmentID or LookupTrackingCode); merchant-order lookups are
// unsupported.
func (c *Client) TrackShipment(ctx context.Context, req courier.TrackingRequest) (*courier.TrackingResult, error) {
	const op = "tracking.get"
	if err := req.Validate(); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(req.Code))
	if id == "" {
		if v, ok := req.Find(courier.LookupProviderShipmentID); ok {
			id = v
		} else if v, ok := req.Find(courier.LookupTrackingCode); ok {
			id = v
		}
	}
	if id == "" {
		return nil, courier.Unsupported(ProviderID, op, "supported lookups: consignment ID")
	}
	if !idPattern.MatchString(id) {
		return nil, courier.Validation("code", "invalid carrybee consignment ID")
	}
	resp, creds, err := c.call(ctx, callSpec{op: op, method: http.MethodGet, path: "/orders/" + url.PathEscape(id) + "/details"})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			TransferStatus  string          `json:"transfer_status"`
			CollectedAmount json.RawMessage `json:"collected_amount"`
		} `json:"data"`
	}
	if err := jsonDecode(op, resp.Body, &out); err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(out.Data.TransferStatus)
	if raw == "" {
		return nil, protocol(op, "details response lacks data.transfer_status")
	}
	res := &courier.TrackingResult{
		Ref:            courier.ShipmentRef{Provider: ProviderID, Account: creds.AccountID, ID: courier.ShipmentID(id)},
		ProviderStatus: courier.ProviderStatus{Code: raw},
		Status:         mapStatus(raw),
		Coverage:       courier.CoverageCurrentOnly,
		COD:            courier.CODUnknown,
		ObservedAt:     time.Now().UTC(),
		Raw:            resp.Body,
	}
	if res.Status == courier.StatusDelivered {
		res.Delivery = &courier.DeliveryConfirmation{}
		if d, ok, err := courier.ParseAmount(out.Data.CollectedAmount); err == nil && ok && d.IsPositive() {
			res.COD = courier.CODCollected
		}
	}
	return res, nil
}

// CancelShipment cancels an order (POST /api/v2/orders/{id}/cancel).
func (c *Client) CancelShipment(ctx context.Context, ref courier.ShipmentRef) error {
	return c.CancelOrder(ctx, string(ref.ID), "", ref.Provider)
}

// CancelOrder is CancelShipment with a cancellation reason. Whether the
// provider requires a reason is not documented; it is sent as given.
func (c *Client) CancelOrder(ctx context.Context, consignmentID, reason string, provider courier.ProviderID) error {
	const op = "shipment.cancel"
	if provider != "" && provider != ProviderID {
		return courier.Validation("ref.provider", "not a carrybee shipment")
	}
	id := strings.TrimSpace(consignmentID)
	if !idPattern.MatchString(id) {
		return courier.Validation("consignment_id", "invalid carrybee consignment ID")
	}
	body := map[string]string{"cancellation_reason": truncate(reason, 500)}
	resp, _, err := c.call(ctx, callSpec{op: op, method: http.MethodPost, path: "/orders/" + url.PathEscape(id) + "/cancel", payload: body, mutation: true})
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if jsonDecode(op, resp.Body, &fields) != nil {
		return asIndeterminate(protocol(op, "response is not a JSON object"))
	}
	return nil
}
