package pathao

import (
	"context"
	"net/http"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// CreateShipment creates one order (consignment). Money must be BDT. City,
// zone and area must be explicit pathao LocationRefs. A timeout, 5xx or
// unusable response is indeterminate; reconcile before replaying (no
// idempotency key is documented, so IdempotencyKey is not forwarded).
func (c *Client) CreateShipment(ctx context.Context, req courier.CreateShipmentRequest) (*courier.Shipment, error) {
	// Credentials are resolved first so mode violations win over input errors
	// and so DefaultStoreID is available; no HTTP happens before validation.
	creds, err := c.creds.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	if err := creds.validate(); err != nil {
		return nil, err
	}
	order, err := buildOrder(req, creds)
	if err != nil {
		return nil, err
	}
	const op = "shipment.create"
	resp, creds, err := c.call(ctx, callSpec{op: op, method: http.MethodPost, path: "/orders", payload: order, mutation: true})
	if err != nil {
		return nil, err
	}
	var d createData
	if err := decode(op, resp.Body, true, &d); err != nil {
		return nil, err
	}
	id := d.ConsignmentID.String()
	if !idPattern.MatchString(id) {
		return nil, asIndeterminate(protocol(op, "response lacks a valid consignment_id"))
	}
	moid := courier.MerchantOrderID(d.MerchantOrderID.String())
	if moid == "" {
		moid = req.MerchantOrderID
	}
	sh := &courier.Shipment{
		Ref:             courier.ShipmentRef{Provider: ProviderID, Account: creds.AccountID, ID: courier.ShipmentID(id)},
		MerchantOrderID: moid,
		TrackingCode:    courier.TrackingCode(id),
		ProviderStatus:  courier.ProviderStatus{Code: d.OrderStatus},
		Status:          mapStatusValue(d.OrderStatus),
		Raw:             resp.Body,
	}
	if fee, ok, err := courier.ParseAmount(d.DeliveryFee); err == nil && ok {
		m := courier.NewBDT(fee)
		sh.DeliveryCharge = &m
	}
	return sh, nil
}

// TrackShipment reads the order info. TrackingRequest.Code is the consignment
// ID; LookupProviderShipmentID is accepted too. Merchant-order lookup has no
// verified endpoint and is ErrUnsupported. The endpoint exposes the current
// status only, so the result is CoverageCurrentOnly with no events.
func (c *Client) TrackShipment(ctx context.Context, req courier.TrackingRequest) (*courier.TrackingResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(req.Code))
	if id == "" {
		v, ok := req.Find(courier.LookupProviderShipmentID)
		if !ok {
			return nil, courier.Unsupported(ProviderID, "tracking.get", "supported lookups: tracking code or provider shipment ID (consignment ID); merchant order lookup is unverified")
		}
		id = v
	}
	p, err := consignmentPath(id)
	if err != nil {
		return nil, err
	}
	const op = "tracking.get"
	resp, creds, err := c.call(ctx, callSpec{op: op, method: http.MethodGet, path: "/orders/" + p + "/info"})
	if err != nil {
		return nil, err
	}
	if req.Account != "" && creds.AccountID != "" && req.Account != creds.AccountID {
		return nil, courier.Validation("account", "tracking account does not match the client credentials")
	}
	var d infoData
	if err := decode(op, resp.Body, false, &d); err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(d.OrderStatus)
	if raw == "" {
		raw = strings.TrimSpace(d.OrderStatusSlug)
	}
	if raw == "" {
		return nil, protocol(op, "response lacks order_status")
	}
	if got := d.ConsignmentID.String(); got != "" && got != id {
		return nil, protocol(op, "response consignment does not match the request")
	}
	st := mapStatusValue(raw)
	res := &courier.TrackingResult{
		Ref:            courier.ShipmentRef{Provider: ProviderID, Account: creds.AccountID, ID: courier.ShipmentID(id)},
		TrackingCode:   courier.TrackingCode(id),
		ProviderStatus: courier.ProviderStatus{Code: raw},
		Status:         st,
		Coverage:       courier.CoverageCurrentOnly,
		COD:            courier.CODUnknown,
		ObservedAt:     time.Now().UTC(),
		Raw:            resp.Body,
	}
	if slug := strings.TrimSpace(d.OrderStatusSlug); slug != "" && slug != raw {
		res.ProviderStatus.Description = slug
	}
	if st == courier.StatusDelivered {
		res.Delivery = &courier.DeliveryConfirmation{} // no delivery timestamp is provided
	}
	return res, nil
}

// Quote prices a home delivery through the merchant price plan. The call is a
// POST read: not a mutation, but not replay-safe either (never auto-retried).
// Fee is the provider's final_price in BDT; Pathao gives no expiry, so
// ExpiresAt is nil. COD, if given, is validated but not part of the request.
func (c *Client) Quote(ctx context.Context, req courier.QuoteRequest) (*courier.Quote, error) {
	creds, err := c.creds.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	if err := creds.validate(); err != nil {
		return nil, err
	}
	pr, err := buildPrice(req, creds)
	if err != nil {
		return nil, err
	}
	const op = "quote.get"
	resp, creds, err := c.call(ctx, callSpec{op: op, method: http.MethodPost, path: "/merchant/price-plan", payload: pr})
	if err != nil {
		return nil, err
	}
	var d priceData
	if err := decode(op, resp.Body, false, &d); err != nil {
		return nil, err
	}
	fee, ok, err := courier.ParseAmount(d.FinalPrice)
	if err != nil || !ok {
		return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: op, Message: "missing or invalid final_price", Err: err}
	}
	if fee.IsNegative() {
		return nil, protocol(op, "negative final_price")
	}
	return &courier.Quote{Ref: courier.ShipmentRef{Provider: ProviderID, Account: creds.AccountID}, Fee: courier.NewBDT(fee), Raw: resp.Body}, nil
}
