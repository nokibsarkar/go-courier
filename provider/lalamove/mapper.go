package lalamove

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/httpx"
)

// e164 accepts only an explicit international form ("+<cc>..." or "00<cc>...").
// Unlike courier.NormalizePhone it never assumes a country: Lalamove serves many
// markets and a national number must not be guessed.
func e164(field, in string) (string, error) {
	s := strings.TrimSpace(in)
	plus := strings.HasPrefix(s, "+")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' || r == '+':
		default:
			return "", courier.Validation(field, "contains invalid characters")
		}
	}
	d := b.String()
	switch {
	case plus:
	case strings.HasPrefix(d, "00"):
		d = d[2:]
	default:
		return "", courier.Validation(field, "use the international form +<country code><number>")
	}
	if len(d) < 8 || len(d) > 15 || strings.Contains(s[1:], "+") {
		return "", courier.Validation(field, "invalid phone number")
	}
	return "+" + d, nil
}

func joinAddress(a courier.Address) string {
	var parts []string
	for _, p := range []string{a.Line1, a.Line2, a.Area, a.City, a.District, a.State, a.PostalCode, a.Country} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

func fmtCoord(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func buildStop(field string, p courier.Party) (stopReq, error) {
	if p.Address.Coordinates == nil {
		return stopReq{}, courier.Validation(field+".coordinates", "coordinates are required; the adapter never geocodes or guesses")
	}
	c := p.Address.Coordinates
	if math.IsNaN(c.Lat) || math.IsNaN(c.Lng) || c.Lat < -90 || c.Lat > 90 || c.Lng < -180 || c.Lng > 180 {
		return stopReq{}, courier.Validation(field+".coordinates", "out of range")
	}
	addr := strings.TrimSpace(p.Address.Text)
	if addr == "" {
		addr = joinAddress(p.Address)
	}
	if addr == "" || utf8.RuneCountInString(addr) > 500 {
		return stopReq{}, courier.Validation(field+".address", "required, at most 500 characters")
	}
	return stopReq{Coordinates: coordReq{Lat: fmtCoord(c.Lat), Lng: fmtCoord(c.Lng)}, Address: addr}, nil
}

func buildQuote(req courier.QuoteRequest, o QuoteOptions) (quoteBody, error) {
	if !idPattern.MatchString(o.ServiceType) || strings.ToUpper(o.ServiceType) != o.ServiceType {
		return quoteBody{}, courier.Validation("service_type", "required (for example MOTORCYCLE); never defaulted")
	}
	if l := o.Language; l == "" || len(l) > 16 || strings.ContainsAny(l, " \r\n\"") {
		return quoteBody{}, courier.Validation("language", "required (for example en_SG); never defaulted")
	}
	if req.COD != nil {
		return quoteBody{}, courier.Unsupported(ProviderID, "quote.create", "COD is not supported by this adapter")
	}
	pick, err := buildStop("pickup", req.Pickup)
	if err != nil {
		return quoteBody{}, err
	}
	drop, err := buildStop("recipient", req.Recipient)
	if err != nil {
		return quoteBody{}, err
	}
	q := quoteReq{ServiceType: o.ServiceType, Language: o.Language, Stops: []stopReq{pick, drop},
		SpecialRequests: append([]string(nil), o.SpecialRequests...)}
	if req.ScheduleAt != nil {
		q.ScheduleAt = req.ScheduleAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return quoteBody{Data: q}, nil
}

func parseTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, l := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(l, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, asiaDhaka); err == nil {
		return &t
	}
	return nil // absent, never substituted
}

var asiaDhaka = time.FixedZone("Asia/Dhaka", 6*60*60)

// money builds exact Money from provider total and currency; nil when either is
// absent. The currency is never defaulted.
func money(op string, p *priceData) (*courier.Money, error) {
	if p == nil {
		return nil, nil
	}
	d, ok, err := courier.ParseAmount(p.Total)
	if err != nil {
		return nil, protocol(op, "invalid price total")
	}
	if !ok || strings.TrimSpace(p.Currency) == "" {
		return nil, nil
	}
	cur, err := courier.ParseCurrency(p.Currency)
	if err != nil {
		return nil, protocol(op, "invalid price currency")
	}
	if d.IsNegative() {
		return nil, protocol(op, "negative price")
	}
	return &courier.Money{Amount: d, Currency: cur}, nil
}

func mapQuote(op string, acct courier.AccountID, body []byte) (*courier.Quote, *quotationData, error) {
	var env quoteEnvelope
	if err := httpx.DecodeJSON(ProviderID, op, body, &env); err != nil {
		return nil, nil, err
	}
	d := env.Data
	if d == nil || !idPattern.MatchString(string(d.QuotationID)) {
		return nil, nil, protocol(op, "response lacks a valid quotationId")
	}
	fee, err := money(op, d.PriceBreakdown)
	if err != nil {
		return nil, nil, err
	}
	if fee == nil {
		return nil, nil, protocol(op, "response lacks priceBreakdown total or currency")
	}
	q := &courier.Quote{
		Ref: courier.ShipmentRef{Provider: ProviderID, Account: acct},
		ID:  courier.QuoteID(d.QuotationID), Fee: *fee, Raw: body,
	}
	if strings.TrimSpace(d.ExpiresAt) != "" {
		if q.ExpiresAt = parseTime(d.ExpiresAt); q.ExpiresAt == nil {
			return nil, nil, protocol(op, "invalid expiresAt")
		}
	}
	return q, d, nil
}

// QuoteStop is a stop of a quotation, in provider order (index 0 is the pickup).
type QuoteStop struct {
	ID      string
	Lat     string
	Lng     string
	Address string
}

// QuoteStops is the native typed accessor for the stop identities of a quote
// returned by this adapter (parsed from Quote.Raw). Order is preserved.
func QuoteStops(q *courier.Quote) ([]QuoteStop, error) {
	if q == nil {
		return nil, courier.Validation("quote", "nil quote")
	}
	var env quoteEnvelope
	if err := httpx.DecodeJSON(ProviderID, "quote.stops", q.Raw, &env); err != nil {
		return nil, err
	}
	if env.Data == nil {
		return nil, protocol("quote.stops", "quote carries no data")
	}
	out := make([]QuoteStop, len(env.Data.Stops))
	for i, s := range env.Data.Stops {
		out[i] = QuoteStop{ID: string(s.StopID), Lat: string(s.Coordinates.Lat), Lng: string(s.Coordinates.Lng), Address: s.Address}
	}
	return out, nil
}

type createInput struct {
	senderName, senderPhone, recipName, recipPhone, remarks string
}

func validateCreate(req courier.CreateShipmentRequest) (createInput, error) {
	var p createInput
	if strings.TrimSpace(string(req.QuoteID)) == "" {
		return p, courier.Validation("quote_id", "required: Lalamove orders are quotation-first and the adapter never quotes and buys implicitly")
	}
	if err := checkID("quote_id", string(req.QuoteID)); err != nil {
		return p, err
	}
	if req.IdempotencyKey != "" {
		return p, courier.Unsupported(ProviderID, shipmentCreateOp, "Lalamove documents no idempotency key; reconcile via GetOrder and metadata merchantOrderId")
	}
	if req.COD != nil || req.DeliveryCharge != nil {
		return p, courier.Unsupported(ProviderID, shipmentCreateOp, "COD and delivery charge are not supported by this adapter")
	}
	if req.Delivery != "" && req.Delivery != courier.DeliveryHome {
		return p, courier.Unsupported(ProviderID, shipmentCreateOp, "point delivery is not supported")
	}
	if req.Pickup.PickupPoint != "" || req.Pickup.Store != "" || req.Recipient.PickupPoint != "" || req.Recipient.Store != "" {
		return p, courier.Unsupported(ProviderID, shipmentCreateOp, "pickup points and stores are not supported")
	}
	var err error
	if p.senderName = strings.TrimSpace(req.Pickup.Contact.Name); p.senderName == "" || utf8.RuneCountInString(p.senderName) > 100 {
		return p, courier.Validation("pickup.name", "required, at most 100 characters")
	}
	if p.recipName = strings.TrimSpace(req.Recipient.Contact.Name); p.recipName == "" || utf8.RuneCountInString(p.recipName) > 100 {
		return p, courier.Validation("recipient.name", "required, at most 100 characters")
	}
	if p.senderPhone, err = e164("pickup.phone", req.Pickup.Contact.Phone); err != nil {
		return p, err
	}
	if p.recipPhone, err = e164("recipient.phone", req.Recipient.Contact.Phone); err != nil {
		return p, err
	}
	p.remarks = strings.TrimSpace(req.Recipient.Instructions)
	if n := strings.TrimSpace(req.Note); n != "" {
		if p.remarks != "" {
			p.remarks += "; "
		}
		p.remarks += n
	}
	return p, nil
}

func coordMatch(got flexString, want float64) bool {
	g, err := strconv.ParseFloat(string(got), 64)
	return err == nil && math.Abs(g-want) <= 1e-5
}

// checkStops verifies the quotation can be ordered as requested and returns the
// pickup and recipient stop IDs in provider order. It never reorders or edits.
func checkStops(req courier.CreateShipmentRequest, q *quotationData) ([]string, error) {
	switch {
	case len(q.Stops) > 2:
		return nil, courier.Unsupported(ProviderID, shipmentCreateOp, "multi-stop quotations are not supported; exactly one pickup and one recipient")
	case len(q.Stops) < 2:
		return nil, protocol(shipmentCreateOp, "quotation has fewer than two stops")
	}
	ids := make([]string, 2)
	for i, s := range q.Stops {
		ids[i] = string(s.StopID)
		if !idPattern.MatchString(ids[i]) {
			return nil, protocol(shipmentCreateOp, "quotation stop lacks a valid stopId")
		}
	}
	if ids[0] == ids[1] {
		return nil, protocol(shipmentCreateOp, "quotation stop IDs are not distinct")
	}
	for i, p := range []struct {
		name  string
		party courier.Party
	}{{"pickup", req.Pickup}, {"recipient", req.Recipient}} {
		if c := p.party.Address.Coordinates; c != nil {
			s := q.Stops[i]
			if !coordMatch(s.Coordinates.Lat, c.Lat) || !coordMatch(s.Coordinates.Lng, c.Lng) {
				return nil, courier.Validation(p.name+".coordinates", "does not match the quotation stop at this position")
			}
		}
	}
	return ids, nil
}

func decodeOrder(op string, body []byte) (*orderData, error) {
	var env orderEnvelope
	if err := httpx.DecodeJSON(ProviderID, op, body, &env); err != nil {
		return nil, err
	}
	if env.Data == nil || !idPattern.MatchString(string(env.Data.OrderID)) {
		return nil, protocol(op, "response lacks a valid orderId")
	}
	return env.Data, nil
}

// mapOrderStatus maps clear Lalamove statuses only. ON_GOING, REJECTED and
// EXPIRED are ambiguous without further provider evidence and stay unknown, as
// does COMPLETED when a stop reports a failed proof of delivery.
func mapOrderStatus(code string, stops []orderStopData) courier.Status {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "ASSIGNING_DRIVER":
		return courier.StatusCreated
	case "PICKED_UP":
		return courier.StatusPickedUp
	case "CANCELED":
		return courier.StatusCancelled
	case "COMPLETED":
		for _, s := range stops {
			if s.POD != nil && strings.EqualFold(s.POD.Status, "FAILED") {
				return courier.StatusUnknown
			}
		}
		return courier.StatusDelivered
	default: // ON_GOING, REJECTED, EXPIRED, anything new
		return courier.StatusUnknown
	}
}

func mapOrder(acct courier.AccountID, o *orderData, raw []byte) (*courier.Shipment, error) {
	fee, err := money("order", o.PriceBreakdown)
	if err != nil {
		return nil, err
	}
	return &courier.Shipment{
		Ref:            courier.ShipmentRef{Provider: ProviderID, Account: acct, ID: courier.ShipmentID(o.OrderID)},
		ProviderStatus: courier.ProviderStatus{Code: o.Status},
		Status:         mapOrderStatus(o.Status, o.Stops),
		DeliveryCharge: fee,
		Raw:            raw,
	}, nil
}

func mapTracking(acct courier.AccountID, req courier.TrackingRequest, o *orderData, raw []byte, now time.Time) *courier.TrackingResult {
	st := mapOrderStatus(o.Status, o.Stops)
	res := &courier.TrackingResult{
		Ref:            courier.ShipmentRef{Provider: ProviderID, Account: acct, ID: courier.ShipmentID(o.OrderID)},
		TrackingCode:   req.Code,
		ProviderStatus: courier.ProviderStatus{Code: o.Status},
		Status:         st,
		Coverage:       courier.CoverageCurrentOnly,
		COD:            courier.CODUnknown,
		ObservedAt:     now,
		Raw:            raw,
	}
	if st == courier.StatusDelivered {
		res.Delivery = &courier.DeliveryConfirmation{}
		if n := len(o.Stops); n > 0 && o.Stops[n-1].POD != nil {
			res.Delivery.DeliveredAt = parseTime(o.Stops[n-1].POD.DeliveredAt)
		}
	}
	return res
}

// POD is proof-of-pickup/delivery data for one stop, as supplied by Lalamove.
type POD struct {
	Status      string // PENDING, DELIVERED, SIGNED, FAILED (raw)
	Image       string // provider-issued URL, never fetched by the SDK
	DeliveredAt *time.Time
}

// OrderStop is one stop of an order, in provider order.
type OrderStop struct {
	ID  string
	POD *POD
}

// Order is the typed Lalamove order view.
type Order struct {
	Shipment    *courier.Shipment
	QuotationID string
	DriverID    string // empty until a driver is matched
	ShareLink   string
	Stops       []OrderStop
}

func toOrder(acct courier.AccountID, o *orderData, raw []byte) (*Order, error) {
	sh, err := mapOrder(acct, o, raw)
	if err != nil {
		return nil, err
	}
	out := &Order{Shipment: sh, QuotationID: string(o.QuotationID), DriverID: string(o.DriverID), ShareLink: o.ShareLink}
	for _, s := range o.Stops {
		os := OrderStop{ID: string(s.StopID)}
		if s.POD != nil {
			os.POD = &POD{Status: s.POD.Status, Image: s.POD.Image, DeliveredAt: parseTime(s.POD.DeliveredAt)}
		}
		out.Stops = append(out.Stops, os)
	}
	return out, nil
}

// Driver is the driver assigned to an order (personal data).
type Driver struct {
	ID          string
	Name        string
	Phone       string
	PlateNumber string // partially masked by Lalamove
	Photo       string
	Lat, Lng    string
	LocationAt  *time.Time
	Raw         []byte
}

func toDriver(d *driverData, raw []byte) *Driver {
	out := &Driver{ID: string(d.DriverID), Name: d.Name, Phone: d.Phone, PlateNumber: d.PlateNumber, Photo: d.Photo, Raw: raw}
	if c := d.Coordinates; c != nil {
		out.Lat, out.Lng, out.LocationAt = string(c.Lat), string(c.Lng), parseTime(c.UpdatedAt)
	}
	return out
}
