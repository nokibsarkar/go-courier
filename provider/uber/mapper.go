package uber

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// minorUnitExponent is fixed at 2: every money field is an amount in cents
// (1/100 of the currency unit). Change it here if Uber ever states otherwise
// for zero- or three-decimal currencies (REF-02).
const minorUnitExponent int32 = 2

func protocol(op, msg string) *courier.Error {
	return &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: op, Message: msg}
}

// providerCurrency takes the currency from the response only; never defaulted.
func providerCurrency(op, currencyType, currency string) (courier.Currency, error) {
	a, b := strings.TrimSpace(currencyType), strings.TrimSpace(currency)
	if a == "" && b == "" {
		return "", protocol(op, "response lacks a currency")
	}
	if a != "" && b != "" && !strings.EqualFold(a, b) {
		return "", protocol(op, "conflicting currency fields")
	}
	if a == "" {
		a = b
	}
	cur, err := courier.ParseCurrency(a)
	if err != nil {
		return "", protocol(op, "invalid currency in response")
	}
	return cur, nil
}

// minorMoney converts an integer minor-unit amount into exact Money. present
// is false when the field is absent (nil, never zero: MON-05).
func minorMoney(op string, raw json.RawMessage, cur courier.Currency) (m *courier.Money, present bool, err error) {
	d, ok, perr := courier.ParseAmount(raw)
	if perr != nil {
		return nil, false, protocol(op, "invalid fee")
	}
	if !ok {
		return nil, false, nil
	}
	if !d.IsInteger() || d.IsNegative() {
		return nil, false, protocol(op, "fee must be a non-negative integer in minor units")
	}
	mm := courier.Money{Amount: d.Shift(-minorUnitExponent), Currency: cur}
	return &mm, true, nil
}

func hasValue(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && string(t) != "null"
}

func parseTime(s string) *time.Time {
	s = strings.TrimSpace(s)
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

// embeddedFailure turns an HTTP 2xx body that embeds an error into an error.
func embeddedFailure(op, kind, code, id string) error {
	if kind != "error" && (code == "" || id != "") {
		return nil
	}
	e := &courier.Error{Provider: ProviderID, Op: op, ProviderCode: truncate(code, 64), Outcome: courier.OutcomeRejected,
		Message: "provider reported a failure in a successful HTTP response"}
	switch code {
	case "unauthorized", "invalid_token", "token_expired":
		e.Kind = courier.KindCredentials
	case "forbidden", "permission_denied":
		e.Kind = courier.KindPermission
	case "not_found", "delivery_not_found", "quote_not_found":
		e.Kind = courier.KindNotFound
	case "invalid_params", "invalid_request", "validation_error":
		e.Kind = courier.KindValidation
	case "too_many_requests", "rate_limited":
		e.Kind, e.Retryable = courier.KindRateLimit, true
	default:
		e.Kind = courier.KindRejected
	}
	return e
}

// mapStatus maps Uber delivery statuses; ambiguous or unseen values stay
// StatusUnknown with the raw value preserved (DOM-07, ERR-05).
func mapStatus(s string) courier.Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pending":
		return courier.StatusCreated
	case "pickup":
		return courier.StatusPickupPending // courier heading to pickup
	case "pickup_complete":
		return courier.StatusPickedUp
	case "dropoff":
		return courier.StatusOutForDelivery // courier heading to dropoff
	case "delivered":
		return courier.StatusDelivered
	case "canceled", "cancelled":
		return courier.StatusCancelled
	case "returned":
		return courier.StatusReturned
	}
	return courier.StatusUnknown
}

func mapDelivery(acct courier.AccountID, d *deliveryResponse, raw []byte) (*courier.Shipment, error) {
	const op = "shipment.map"
	if !idPattern.MatchString(d.ID) {
		return nil, protocol(op, "invalid delivery id")
	}
	sh := &courier.Shipment{
		Ref:             courier.ShipmentRef{Provider: ProviderID, Account: acct, ID: courier.ShipmentID(d.ID)},
		MerchantOrderID: courier.MerchantOrderID(d.ExternalID),
		TrackingCode:    courier.TrackingCode(d.ID), // accepted by TrackShipment
		ProviderStatus:  courier.ProviderStatus{Code: d.Status},
		Status:          mapStatus(d.Status),
		CreatedAt:       parseTime(d.Created),
		UpdatedAt:       parseTime(d.Updated),
		Raw:             raw,
	}
	if hasValue(d.Fee) {
		cur, err := providerCurrency(op, d.CurrencyType, d.Currency)
		if err != nil {
			return nil, err
		}
		m, _, err := minorMoney(op, d.Fee, cur)
		if err != nil {
			return nil, err
		}
		sh.DeliveryCharge = m
	}
	return sh, nil
}

func trackingResult(acct courier.AccountID, id, code, status string, raw []byte, now time.Time) *courier.TrackingResult {
	st := mapStatus(status)
	res := &courier.TrackingResult{
		Ref:            courier.ShipmentRef{Provider: ProviderID, Account: acct, ID: courier.ShipmentID(id)},
		TrackingCode:   courier.TrackingCode(code),
		ProviderStatus: courier.ProviderStatus{Code: status},
		Status:         st,
		Coverage:       courier.CoverageCurrentOnly,
		COD:            courier.CODUnknown,
		ObservedAt:     now,
		Raw:            raw,
	}
	if st == courier.StatusDelivered {
		res.Delivery = &courier.DeliveryConfirmation{} // no delivery timestamp is exposed
	}
	return res
}

// ---- request building ----

func rejectUnsupported(r courier.CreateShipmentRequest) error {
	switch {
	case r.COD != nil:
		return courier.Validation("cod", "Uber Direct does not support cash on delivery")
	case r.DeliveryCharge != nil:
		return courier.Validation("delivery_charge", "the delivery fee is set by Uber")
	case r.Package.DeclaredValue != nil:
		return courier.Validation("package.declared_value", "not supported by this adapter")
	case r.Package.Insurance != nil:
		return courier.Validation("package.insurance", "not supported by this adapter")
	case r.Package.WeightGrams != nil, r.Package.Dimensions != nil:
		return courier.Validation("package", "weight and dimensions are not supported by this adapter")
	case r.Delivery == courier.DeliveryPoint:
		return courier.Validation("delivery", "point delivery is not supported")
	case r.Pickup.PickupPoint != "" || r.Recipient.PickupPoint != "" || r.Pickup.Store != "" || r.Recipient.Store != "":
		return courier.Validation("party", "pickup points and stores are not supported")
	case r.Delivery != "" && r.Delivery != courier.DeliveryHome:
		return courier.Validation("delivery", "must be home")
	}
	return nil
}

// buildAddress returns Uber's address JSON string. pickup_address and dropoff_address are a "JSON string containing address
// details" (street_address array, city, state, zip_code, country), so free text
// is NOT sent: it would be a guess
// (DOM-05). Provide Line1, City and Country.
func buildAddress(field string, a courier.Address) (string, error) {
	l1, city, country := strings.TrimSpace(a.Line1), strings.TrimSpace(a.City), strings.TrimSpace(a.Country)
	if l1 == "" && city == "" && country == "" {
		if strings.TrimSpace(a.Text) != "" {
			return "", &courier.Error{Kind: courier.KindAmbiguous, Field: field,
				Message: "Uber Direct requires a structured address (Line1, City, Country); free text is not parsed here"}
		}
		return "", courier.Validation(field, "address is required")
	}
	if l1 == "" || city == "" || country == "" {
		return "", courier.Validation(field, "structured address needs Line1, City and Country")
	}
	street := []string{l1}
	if l2 := strings.TrimSpace(a.Line2); l2 != "" {
		street = append(street, l2)
	}
	b, err := json.Marshal(structuredAddress{StreetAddress: street, City: city, State: strings.TrimSpace(a.State),
		ZipCode: strings.TrimSpace(a.PostalCode), Country: country})
	if err != nil {
		return "", courier.Validation(field, "cannot encode address")
	}
	return string(b), nil
}

func buildQuote(r courier.QuoteRequest) (quoteRequest, error) {
	if r.COD != nil {
		return quoteRequest{}, courier.Validation("cod", "Uber Direct does not support cash on delivery")
	}
	p, err := buildAddress("pickup.address", r.Pickup.Address)
	if err != nil {
		return quoteRequest{}, err
	}
	d, err := buildAddress("recipient.address", r.Recipient.Address)
	if err != nil {
		return quoteRequest{}, err
	}
	q := quoteRequest{PickupAddress: p, DropoffAddress: d}
	if r.ScheduleAt != nil {
		q.PickupReadyDT = r.ScheduleAt.UTC().Format(time.RFC3339)
	}
	return q, nil
}

func party(field string, p courier.Party) (name, addr, phone string, err error) {
	name = strings.TrimSpace(p.Contact.Name)
	if name == "" {
		return "", "", "", courier.Validation(field+".name", "required")
	}
	if phone, err = courier.NormalizePhone(p.Contact.Phone); err != nil {
		if ce, ok := err.(*courier.Error); ok {
			ce.Field = field + ".phone"
		}
		return "", "", "", err
	}
	if addr, err = buildAddress(field+".address", p.Address); err != nil {
		return "", "", "", err
	}
	return name, addr, phone, nil
}

func buildCreate(r courier.CreateShipmentRequest) (createRequest, error) {
	if err := rejectUnsupported(r); err != nil {
		return createRequest{}, err
	}
	pn, pa, pp, err := party("pickup", r.Pickup)
	if err != nil {
		return createRequest{}, err
	}
	dn, da, dp, err := party("recipient", r.Recipient)
	if err != nil {
		return createRequest{}, err
	}
	desc := strings.TrimSpace(r.Package.Description)
	if desc == "" {
		return createRequest{}, courier.Validation("package.description", "required (sent as the manifest item name)")
	}
	qty := r.Package.Quantity
	switch {
	case qty < 0:
		return createRequest{}, courier.Validation("package.quantity", "must not be negative")
	case qty == 0:
		qty = 1
	}
	notes := strings.TrimSpace(r.Recipient.Instructions)
	if n := strings.TrimSpace(r.Note); n != "" {
		if notes != "" {
			notes += " | "
		}
		notes += n
	}
	ext := strings.TrimSpace(string(r.MerchantOrderID))
	if len(ext) > 128 {
		return createRequest{}, courier.Validation("merchant_order_id", fmt.Sprintf("at most %d characters", 128))
	}
	return createRequest{
		QuoteID:    strings.TrimSpace(string(r.QuoteID)),
		PickupName: pn, PickupAddress: pa, PickupPhone: pp, PickupNotes: strings.TrimSpace(r.Pickup.Instructions),
		DropoffName: dn, DropoffAddress: da, DropoffPhone: dp, DropoffNotes: notes,
		ManifestItems:  []manifestItem{{Name: desc, Quantity: qty}},
		ExternalID:     ext,
		IdempotencyKey: strings.TrimSpace(r.IdempotencyKey),
	}, nil
}

// trackingFromDelivery builds the tracking result from a Get Delivery
// response. Uber exposes only the current status plus, per waypoint, a
// completed/failed state with its timestamp, so the
// only events produced are those genuine waypoint outcomes (coverage partial);
// without them the result is current-only. Nothing is inferred or sorted.
func trackingFromDelivery(acct courier.AccountID, id string, d *deliveryResponse, raw []byte, now time.Time) *courier.TrackingResult {
	res := trackingResult(acct, id, id, d.Status, raw, now)
	add := func(w *waypointResponse, name string, done, failed courier.Status) {
		if w == nil || w.StatusTimestamp == "" {
			return
		}
		var st courier.Status
		switch w.Status {
		case "completed":
			st = done
		case "failed":
			st = failed
		default:
			return
		}
		res.Events = append(res.Events, courier.TrackingEvent{OccurredAt: parseTime(w.StatusTimestamp), Status: st,
			ProviderStatus: courier.ProviderStatus{Code: name + ":" + w.Status}})
	}
	add(d.Pickup, "pickup", courier.StatusPickedUp, courier.StatusUnknown)
	add(d.Dropoff, "dropoff", courier.StatusDelivered, courier.StatusDeliveryFailed)
	if len(res.Events) > 0 {
		res.Coverage = courier.CoveragePartial
	}
	if res.Delivery != nil && d.Dropoff != nil && d.Dropoff.Status == "completed" {
		res.Delivery.DeliveredAt = parseTime(d.Dropoff.StatusTimestamp)
	}
	return res
}
