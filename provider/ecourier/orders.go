package ecourier

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// OrderOptions carries the eCourier-specific order inputs that have no
// canonical equivalent. Set them per call with ContextWithOrderOptions.
type OrderOptions struct {
	// PackageCode is required (from ListPackages).
	PackageCode string
	// PaymentMethod is COD, POS, MPAY or CCRD. When the request has a COD amount
	// it must be empty or COD; without COD it is required (never assumed).
	PaymentMethod string
	ParcelType    string // BOX (default) or DOC
	// RequestedDeliveryDate is YYYY-MM-DD; default is the next day (provider).
	RequestedDeliveryDate string
	PickHub               *int64
	IsFragile             bool
	Exchange              bool // sending_type 2
	IsIPay                bool // only valid with COD
	HubDeliveryID         *int64
	PGWID, PGWTxnID       string
}

type optKey struct{}

// ContextWithOrderOptions attaches OrderOptions to ctx.
func ContextWithOrderOptions(ctx context.Context, o OrderOptions) context.Context {
	return context.WithValue(ctx, optKey{}, o)
}

var paymentMethods = map[string]bool{"COD": true, "POS": true, "MPAY": true, "CCRD": true}

func buildOrder(r courier.CreateShipmentRequest, o OrderOptions) (orderBody, error) {
	var b orderBody
	var err error
	if b.RecipientName, err = limited("recipient.name", r.Recipient.Contact.Name, 250); err != nil {
		return b, err
	}
	if b.RecipientMobile, err = localMobile("recipient.phone", r.Recipient.Contact.Phone); err != nil {
		return b, err
	}
	a := r.Recipient.Address
	if b.RecipientCity, err = limited("recipient.address.city", a.City, 40); err != nil {
		return b, err
	}
	if b.RecipientArea, err = limited("recipient.address.area", a.Area, 40); err != nil {
		return b, err
	}
	thana := thanaRef(a)
	if thana == "" {
		return b, &courier.Error{Kind: courier.KindAmbiguous, Field: "recipient.address.refs", Message: `eCourier needs an explicit thana: add LocationRef{Provider: "ecourier", Kind: "thana", ID: <thana name>} (see ListThanas)`}
	}
	if b.RecipientThana, err = limited("recipient.address.thana", thana, 40); err != nil {
		return b, err
	}
	if b.RecipientZip, err = limited("recipient.address.postal_code", a.PostalCode, 20); err != nil {
		return b, err
	}
	addr := strings.TrimSpace(a.Text)
	if addr == "" {
		addr = strings.TrimSpace(strings.Join(nonEmpty(a.Line1, a.Line2), ", "))
	}
	if b.RecipientAddress, err = limited("recipient.address.text", addr, 1000); err != nil {
		return b, err
	}
	if strings.TrimSpace(o.PackageCode) == "" {
		return b, courier.Validation("package_code", "required: set OrderOptions.PackageCode (see ListPackages)")
	}
	b.PackageCode = strings.TrimSpace(o.PackageCode)

	pm := strings.ToUpper(strings.TrimSpace(o.PaymentMethod))
	switch {
	case r.COD != nil:
		if pm != "" && pm != "COD" {
			return b, courier.Validation("payment_method", "a COD amount requires payment method COD")
		}
		pm = "COD"
		if b.ProductPrice, err = wholeTaka("cod", *r.COD, false); err != nil {
			return b, err
		}
	case pm == "":
		return b, courier.Validation("payment_method", "no COD amount given: set OrderOptions.PaymentMethod (POS, MPAY or CCRD) explicitly")
	case !paymentMethods[pm] || pm == "COD":
		return b, courier.Validation("payment_method", "must be POS, MPAY or CCRD when there is no COD amount")
	default:
		if r.Package.DeclaredValue == nil {
			return b, courier.Validation("package.declared_value", "required as product_price for non-COD orders")
		}
		if b.ProductPrice, err = wholeTaka("package.declared_value", *r.Package.DeclaredValue, false); err != nil {
			return b, err
		}
	}
	b.PaymentMethod = pm
	if o.IsIPay && pm != "COD" {
		return b, courier.Validation("is_ipay", "Instant Pay works only with COD")
	}
	if o.IsIPay {
		b.IsIPay = 1
	}
	if r.COD != nil && r.Package.DeclaredValue != nil {
		v, err := wholeTaka("package.declared_value", *r.Package.DeclaredValue, true)
		if err != nil {
			return b, err
		}
		b.ActualProductPrice = &v
	}
	switch strings.ToUpper(o.ParcelType) {
	case "":
	case "BOX", "DOC":
		b.ParcelType = strings.ToUpper(o.ParcelType)
	default:
		return b, courier.Validation("parcel_type", "must be BOX or DOC")
	}
	if d := strings.TrimSpace(o.RequestedDeliveryDate); d != "" {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			return b, courier.Validation("requested_delivery_time", "must be YYYY-MM-DD")
		}
		b.RequestedDelivery = d
	}
	if id := strings.TrimSpace(string(r.MerchantOrderID)); id != "" {
		if utf8Len(id) > 20 {
			return b, courier.Validation("merchant_order_id", "at most 20 characters (sent as product_id)")
		}
		b.ProductID = id
	}
	if p := strings.TrimSpace(r.Pickup.Address.Text); p != "" {
		if utf8Len(p) > 255 {
			return b, courier.Validation("pickup.address", "at most 255 characters")
		}
		b.PickAddress = p
	}
	b.PickHub = o.PickHub
	if n := strings.TrimSpace(r.Note); n != "" {
		if utf8Len(n) > 255 {
			return b, courier.Validation("note", "at most 255 characters")
		}
		b.Comments = n
	}
	if q := r.Package.Quantity; q < 0 {
		return b, courier.Validation("package.quantity", "must not be negative")
	} else if q > 0 {
		b.NumberOfItem = q
	}
	b.SpecialInstruction = strings.TrimSpace(r.Package.Description)
	b.PGWID, b.PGWTxnID = o.PGWID, o.PGWTxnID
	if o.IsFragile {
		b.IsFragile = 1
	}
	if o.Exchange {
		b.SendingType = 2
	}
	if o.HubDeliveryID != nil {
		b.HubDelivery, b.HubDeliveryID = "yes", o.HubDeliveryID
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

func utf8Len(s string) int { return len([]rune(s)) }

// CreateShipment places an order (POST /order-place). Required eCourier inputs
// without a canonical field come from OrderOptions (ContextWithOrderOptions).
// A timeout, 5xx or unusable 200 is indeterminate; eCourier refuses a duplicate
// product_id + mobile, so reconcile with TrackShipment before re-submitting.
func (c *Client) CreateShipment(ctx context.Context, req courier.CreateShipmentRequest) (*courier.Shipment, error) {
	const op = "shipment.create"
	opts, _ := ctx.Value(optKey{}).(OrderOptions)
	body, err := buildOrder(req, opts)
	if err != nil {
		return nil, err
	}
	resp, creds, err := c.call(ctx, op, "/order-place", body, true)
	if err != nil {
		return nil, err
	}
	env, err := decode(op, resp.Body)
	if err != nil {
		if courier.IsKind(err, courier.KindProtocol) {
			return nil, asIndeterminate(err)
		}
		return nil, err
	}
	id := strings.TrimSpace(env.ID)
	if !ecrPattern.MatchString(id) {
		return nil, asIndeterminate(protocol(op, "response lacks a valid order ID"))
	}
	sh := &courier.Shipment{
		Ref:             courier.ShipmentRef{Provider: ProviderID, Account: creds.account(), ID: courier.ShipmentID(id)},
		MerchantOrderID: courier.MerchantOrderID(body.ProductID),
		TrackingCode:    courier.TrackingCode(id),
		Status:          courier.StatusCreated, // "Order Submitted"; the provider status is "Initiated"
		ProviderStatus:  courier.ProviderStatus{Code: "Order Submitted"},
		Raw:             resp.Body,
	}
	if body.PaymentMethod == "COD" {
		m := courier.NewBDT(mustDecimal(string(body.ProductPrice)))
		sh.COD = &m
	}
	return sh, nil
}

// TrackShipment tracks by eCourier ID (Code) and/or merchant order ID
// (LookupMerchantOrderID, sent as product_id). Matching several parcels by
// product_id is ErrAmbiguous: the SDK never picks one (TRK-04).
func (c *Client) TrackShipment(ctx context.Context, req courier.TrackingRequest) (*courier.TrackingResult, error) {
	return c.track(ctx, "tracking.get", "/track", req)
}

// TrackChild tracks a child parcel by its eCourier ID (POST /track-child).
func (c *Client) TrackChild(ctx context.Context, ecr string) (*courier.TrackingResult, error) {
	return c.track(ctx, "tracking.get_child", "/track-child", courier.TrackingRequest{Code: courier.TrackingCode(ecr)})
}

func (c *Client) track(ctx context.Context, op, path string, req courier.TrackingRequest) (*courier.TrackingResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var b trackBody
	if code := strings.TrimSpace(string(req.Code)); code != "" {
		if !ecrPattern.MatchString(code) {
			return nil, courier.Validation("code", "invalid eCourier ID")
		}
		b.ECR = code
	}
	if v, ok := req.Find(courier.LookupMerchantOrderID); ok {
		b.ProductID = v
	}
	if v, ok := req.Find(courier.LookupTrackingCode); ok && b.ECR == "" {
		b.ECR = v
	}
	if b.ECR == "" && b.ProductID == "" {
		return nil, courier.Unsupported(ProviderID, op, "supported lookups: eCourier ID (code) or merchant order ID")
	}
	// A POST lookup: no automatic replay (TRK-13).
	resp, creds, err := c.call(ctx, op, path, b, false)
	if err != nil {
		return nil, err
	}
	env, err := decode(op, resp.Body)
	if err != nil {
		return nil, err
	}
	var no string
	if json.Unmarshal(env.QueryData, &no) == nil {
		return nil, &courier.Error{Kind: courier.KindNotFound, Provider: ProviderID, Op: op, Outcome: courier.OutcomeRejected, Message: truncate(no, 120)}
	}
	var parcels []json.RawMessage
	if err := json.Unmarshal(env.QueryData, &parcels); err != nil || len(parcels) == 0 {
		return nil, protocol(op, "query_data is neither a message nor a parcel list")
	}
	if len(parcels) > 1 {
		return nil, &courier.Error{Kind: courier.KindAmbiguous, Provider: ProviderID, Op: op, Message: "the lookup matched several parcels; track by eCourier ID"}
	}
	return c.mapParcel(creds.account(), op, parcels[0], time.Now().UTC())
}

func (c *Client) mapParcel(acct courier.AccountID, op string, raw json.RawMessage, now time.Time) (*courier.TrackingResult, error) {
	var p parcel
	if err := jsonDecode(op, raw, &p); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(p.REFID)
	if !ecrPattern.MatchString(id) {
		return nil, protocol(op, "parcel lacks a valid REFID")
	}
	res := &courier.TrackingResult{
		Ref:          courier.ShipmentRef{Provider: ProviderID, Account: acct, ID: courier.ShipmentID(id)},
		TrackingCode: courier.TrackingCode(id), Status: courier.StatusUnknown, Coverage: courier.CoverageFull,
		COD: courier.CODUnknown, ObservedAt: now, Raw: raw,
	}
	if len(p.Status) == 0 {
		res.Coverage = courier.CoverageUnknown
		return res, nil
	}
	curIdx, curTS := 0, ""
	allTS := true
	for i, entry := range p.Status {
		if len(entry) < 1 || strings.TrimSpace(rawString(entry[0])) == "" {
			return nil, protocol(op, "malformed status entry")
		}
		name := rawString(entry[0])
		var msg, ts string
		if len(entry) > 1 {
			msg = rawString(entry[1])
		}
		if len(entry) > 2 {
			ts = strings.TrimSpace(rawString(entry[2]))
		}
		entryRaw, _ := json.Marshal(entry)
		res.Events = append(res.Events, courier.TrackingEvent{OccurredAt: parseEventTime(ts, c.loc), Status: mapStatus(name),
			ProviderStatus: courier.ProviderStatus{Code: name, Description: msg}, Description: msg, Raw: entryRaw})
		if ts == "" {
			allTS = false
		} else if ts > curTS {
			curTS, curIdx = ts, i
		}
	}
	if !allTS { // cannot rank by time: fall back to the provider's first entry
		curIdx = 0
	}
	cur := res.Events[curIdx]
	res.ProviderStatus, res.Status = cur.ProviderStatus, cur.Status
	if res.Status == courier.StatusDelivered {
		res.Delivery = &courier.DeliveryConfirmation{DeliveredAt: cur.OccurredAt}
	}
	return res, nil
}

// CancelShipment cancels an order whose status is "Initiated" or "Initiated API"
// using the client's default cancel comment (WithCancelComment).
func (c *Client) CancelShipment(ctx context.Context, ref courier.ShipmentRef) error {
	if ref.Provider != "" && ref.Provider != ProviderID {
		return courier.Validation("ref.provider", "not an ecourier shipment")
	}
	return c.CancelOrder(ctx, string(ref.ID), c.cancelMsg)
}

// CancelOrder cancels with an explicit reason (mandatory, at most 255 chars).
// success=false with "Order not canceled" is a confirmed rejection (the status
// is no longer editable); "Invalid Tracking" is not-found.
func (c *Client) CancelOrder(ctx context.Context, ecr, comment string) error {
	const op = "shipment.cancel"
	ecr = strings.TrimSpace(ecr)
	if !ecrPattern.MatchString(ecr) {
		return courier.Validation("ecr", "invalid eCourier ID")
	}
	comment = strings.TrimSpace(comment)
	if comment == "" || utf8Len(comment) > 255 {
		return courier.Validation("comment", "required, at most 255 characters")
	}
	resp, _, err := c.call(ctx, op, "/cancel-order", map[string]string{"tracking": ecr, "comment": comment}, true)
	if err != nil {
		return err
	}
	return interpretCancel(op, resp.Body)
}

func interpretCancel(op string, body []byte) error {
	var e envelope
	if err := jsonDecode(op, body, &e); err != nil {
		return asIndeterminate(err)
	}
	switch {
	case isTrue(e.Success):
		return nil
	case isFalse(e.Success):
		msg := messageText(e.Message)
		kind := courier.KindConflict
		if strings.Contains(strings.ToLower(msg), "invalid tracking") {
			kind = courier.KindNotFound
		}
		return &courier.Error{Kind: kind, Provider: ProviderID, Op: op, Message: msg, Outcome: courier.OutcomeRejected}
	}
	if err := embedded(op, e); err != nil {
		return err
	}
	return asIndeterminate(protocol(op, "response does not state whether the order was cancelled"))
}

// GetShipmentLabel returns the label as an expiring-reference URL (POST
// /label-print). The SDK never fetches external URLs (DOM-10); the lifetime of
// the URL is not documented, so ExpiresAt is nil.
func (c *Client) GetShipmentLabel(ctx context.Context, ref courier.ShipmentRef) (*courier.Document, error) {
	const op = "label.get"
	if ref.Provider != "" && ref.Provider != ProviderID {
		return nil, courier.Validation("ref.provider", "not an ecourier shipment")
	}
	ecr := strings.TrimSpace(string(ref.ID))
	if !ecrPattern.MatchString(ecr) {
		return nil, courier.Validation("ref.id", "invalid eCourier ID")
	}
	resp, _, err := c.call(ctx, op, "/label-print", map[string]string{"tracking": ecr}, false)
	if err != nil {
		return nil, err
	}
	env, err := decode(op, resp.Body)
	if err != nil {
		return nil, err
	}
	var link string
	if json.Unmarshal(env.QueryData, &link) != nil || strings.TrimSpace(link) == "" {
		return nil, protocol(op, "response lacks a label URL")
	}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, protocol(op, "label URL is not an absolute https URL")
	}
	return &courier.Document{ContentType: "application/pdf", URL: u.String()}, nil
}

// GetCustomerRisk calls POST /fraud-status-check. eCourier answers with a
// textual customer status ("Warning!", "New Customer for eCourier", "Verified by
// eCourier") and optional reasons; it provides no counts or numeric score, so
// none are invented. Each call may consume COD balance: it is never retried.
func (c *Client) GetCustomerRisk(ctx context.Context, req courier.CustomerRiskRequest) (*courier.CustomerRisk, error) {
	const op = "risk.get"
	num, err := localMobile("phone", req.Phone)
	if err != nil {
		return nil, err
	}
	resp, _, err := c.call(ctx, op, "/fraud-status-check", map[string]string{"number": num}, false)
	if err != nil {
		return nil, err
	}
	var e struct {
		envelope
		CustomerStatus string `json:"customer_status"`
	}
	if err := jsonDecode(op, resp.Body, &e); err != nil {
		return nil, err
	}
	if err := embedded(op, e.envelope); err != nil {
		return nil, err
	}
	if strings.TrimSpace(e.CustomerStatus) == "" {
		return nil, protocol(op, "response lacks customer_status")
	}
	now := time.Now().UTC()
	return &courier.CustomerRisk{
		Source: ProviderID, Assessment: strings.TrimSpace(e.CustomerStatus),
		Provenance: courier.RiskProvenance{Origin: "ecourier merchant network", Intermediary: "", RetrievedAt: now,
			Coverage: "eCourier customers only", Completeness: courier.CompletenessUnknown},
		Raw: resp.Body,
	}, nil
}

func mustDecimal(s string) decimalT {
	d, _ := decimalFromString(s) // s comes from a validated whole-taka json.Number
	return d
}
