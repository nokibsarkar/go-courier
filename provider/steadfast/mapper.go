package steadfast

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	courier "github.com/nokibsarkar/go-courier"
)

var (
	invoicePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	digitsPattern  = regexp.MustCompile(`^[0-9]+$`)
)

func errorsJoin(errs []error) error { return errors.Join(errs...) }

// localPhone converts any accepted phone form to Steadfast's 11-digit local
// Bangladeshi form. Non-Bangladeshi numbers are rejected (provider limit).
func localPhone(field, in string) (string, error) {
	n, err := courier.NormalizePhone(in)
	if err != nil {
		var ce *courier.Error
		if errors.As(err, &ce) {
			ce.Field = field
		}
		return "", err
	}
	if !strings.HasPrefix(n, "+880") || len(n) != 14 {
		return "", courier.Validation(field, "steadfast supports Bangladeshi mobile numbers only")
	}
	return "0" + n[4:], nil
}

func buildOrder(r courier.CreateShipmentRequest) (orderRequest, error) {
	inv := strings.TrimSpace(string(r.MerchantOrderID))
	switch {
	case inv == "":
		return orderRequest{}, courier.Validation("merchant_order_id", "required")
	case utf8.RuneCountInString(inv) > MaxInvoiceLen:
		return orderRequest{}, courier.Validation("merchant_order_id", "at most 100 characters")
	case !invoicePattern.MatchString(inv):
		return orderRequest{}, courier.Validation("merchant_order_id", "only letters, digits, hyphen and underscore are allowed")
	}
	name := strings.TrimSpace(r.Recipient.Contact.Name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return orderRequest{}, courier.Validation("recipient.name", "required, at most 100 characters")
	}
	phone, err := localPhone("recipient.phone", r.Recipient.Contact.Phone)
	if err != nil {
		return orderRequest{}, err
	}
	addr := strings.TrimSpace(r.Recipient.Address.Text)
	if addr == "" {
		addr = joinAddress(r.Recipient.Address)
	}
	if addr == "" || utf8.RuneCountInString(addr) > MaxAddressLen {
		return orderRequest{}, courier.Validation("recipient.address", "required, at most 490 characters")
	}
	// COD: nil means no COD; steadfast requires an explicit number, so send 0
	// only when the caller explicitly provided none (documented COD-free order).
	cod := json.Number("0")
	if r.COD != nil {
		if err := r.COD.Validate("cod", false); err != nil {
			return orderRequest{}, err
		}
		if r.COD.Code() != courier.BDT {
			return orderRequest{}, courier.Validation("cod.currency", "steadfast operates in BDT only")
		}
		if r.COD.Amount.GreaterThan(decimal.NewFromInt(MaxCOD)) {
			return orderRequest{}, courier.Validation("cod", "at most 1,000,000 BDT")
		}
		cod = json.Number(r.COD.Amount.String())
	}
	dt := 0
	switch r.Delivery {
	case "", courier.DeliveryHome:
	case courier.DeliveryPoint:
		dt = 1
	default:
		return orderRequest{}, courier.Validation("delivery", "must be home or point")
	}
	if utf8.RuneCountInString(strings.TrimSpace(r.Note)) > MaxNoteLen {
		return orderRequest{}, courier.Validation("note", "at most 480 characters")
	}
	o := orderRequest{Invoice: inv, RecipientName: name, RecipientPhone: phone, RecipientAddress: addr, CODAmount: cod,
		DeliveryType: dt, Note: strings.TrimSpace(r.Note), ItemDescription: strings.TrimSpace(r.Package.Description)}
	if r.Package.Quantity < 0 {
		return orderRequest{}, courier.Validation("package.quantity", "must not be negative")
	}
	o.TotalLot = r.Package.Quantity
	if p := r.Recipient.Contact.AlternativePhone; p != "" {
		if o.AlternativePhone, err = localPhone("recipient.alternative_phone", p); err != nil {
			return orderRequest{}, err
		}
	}
	if e := strings.TrimSpace(r.Recipient.Contact.Email); e != "" {
		if !strings.Contains(e, "@") {
			return orderRequest{}, courier.Validation("recipient.email", "invalid email address")
		}
		o.RecipientEmail = e
	}
	return o, nil
}

func joinAddress(a courier.Address) string {
	var parts []string
	for _, p := range []string{a.Line1, a.Line2, a.Area, a.City, a.District, a.PostalCode} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
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

func mapConsignment(acct courier.AccountID, c *consignment, raw []byte) (*courier.Shipment, error) {
	id := c.ConsignmentID.String()
	if !digitsPattern.MatchString(id) {
		return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Message: "invalid consignment_id"}
	}
	sh := &courier.Shipment{
		Ref:             courier.ShipmentRef{Provider: ProviderID, Account: acct, ID: courier.ShipmentID(id)},
		MerchantOrderID: courier.MerchantOrderID(c.Invoice),
		TrackingCode:    courier.TrackingCode(c.TrackingCode),
		ProviderStatus:  courier.ProviderStatus{Code: c.Status},
		Status:          mapStatusValue(c.Status),
		CreatedAt:       parseTime(c.CreatedAt),
		UpdatedAt:       parseTime(c.UpdatedAt),
		Raw:             raw,
	}
	if d, ok, err := courier.ParseAmount(c.CODAmount); err == nil && ok {
		m := courier.NewBDT(d)
		sh.COD = &m
	}
	return sh, nil
}

func trackingPath(r courier.TrackingRequest) (string, error) {
	if code := strings.TrimSpace(string(r.Code)); code != "" {
		return "/status_by_trackingcode/" + url.PathEscape(code), nil
	}
	if v, ok := r.Find(courier.LookupMerchantOrderID); ok {
		if !invoicePattern.MatchString(v) {
			return "", courier.Validation("lookups", "invalid merchant order ID")
		}
		return "/status_by_invoice/" + url.PathEscape(v), nil
	}
	if v, ok := r.Find(courier.LookupProviderShipmentID); ok {
		if !digitsPattern.MatchString(v) {
			return "", courier.Validation("lookups", "consignment ID must be numeric")
		}
		return "/status_by_cid/" + url.PathEscape(v), nil
	}
	return "", courier.Unsupported(ProviderID, trackingGetOp, "supported lookups: tracking code, merchant order ID, provider shipment ID")
}

// mapStatusValue maps Steadfast delivery statuses. "*_approval_pending" is the
// rider's word, not confirmed: it stays StatusUnknown (with the raw code kept)
// until delivered, partial_delivered or cancelled is reported (DOM-07, TRK-07,
// PROV-02). partial_delivered has no canonical phase; exceptional and unknown
// mean "ask the provider".
func mapStatusValue(s string) courier.Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pending", "in_review":
		return courier.StatusCreated
	case "hold":
		return courier.StatusOnHold
	case "delivered":
		return courier.StatusDelivered
	case "cancelled":
		return courier.StatusCancelled
	default:
		return courier.StatusUnknown
	}
}

// returnStatuses are the progress codes of a parcel coming back (the provider
// spells "processing" with two c's; both spellings are accepted).
var returnStatuses = map[string]courier.Status{
	"cancelled_return_proccessing":            courier.StatusReturning,
	"cancelled_return_processing":             courier.StatusReturning,
	"cancelled_return_rider_assigned":         courier.StatusReturning,
	"cancelled_return_received":               courier.StatusReturned,
	"partial_delivered_return_proccessing":    courier.StatusUnknown, // partly delivered: no canonical phase
	"partial_delivered_return_processing":     courier.StatusUnknown,
	"partial_delivered_return_rider_assigned": courier.StatusUnknown,
	"partial_delivered_return_received":       courier.StatusUnknown,
}

func mapStatus(acct courier.AccountID, req courier.TrackingRequest, raw string, body []byte, now time.Time) *courier.TrackingResult {
	st := mapStatusValue(raw)
	res := &courier.TrackingResult{
		Ref:            courier.ShipmentRef{Provider: ProviderID, Account: acct},
		TrackingCode:   req.Code,
		ProviderStatus: courier.ProviderStatus{Code: raw},
		Status:         st,
		Coverage:       courier.CoverageCurrentOnly,
		COD:            courier.CODUnknown,
		ObservedAt:     now,
		Raw:            body,
	}
	if st == courier.StatusDelivered {
		// "delivered" is confirmed by Steadfast accounts and the COD is then
		// owed to the merchant; no timestamp is provided.
		res.Delivery = &courier.DeliveryConfirmation{}
		res.COD = courier.CODCollected
	}
	return res
}
