package steadfast

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/httpx"
)

// The return-request creation fields and statuses are verified; the other
// response bodies (return reads, payments, police stations, return status) use
// assumed field names and are UNVERIFIED (see Manifest, CAP-01). Decoding is
// permissive about extras, strict about the identifiers it depends on, and
// every result keeps the raw payload.

// ReturnTarget selects the parcel for a return request; set exactly one field.
type ReturnTarget struct {
	ConsignmentID string
	Invoice       string
	TrackingCode  string
}

func (t ReturnTarget) body() (map[string]string, error) {
	var set []string
	m := map[string]string{}
	if v := strings.TrimSpace(t.ConsignmentID); v != "" {
		if !digitsPattern.MatchString(v) {
			return nil, courier.Validation("consignment_id", "must be numeric")
		}
		m["consignment_id"] = v
		set = append(set, "consignment_id")
	}
	if v := strings.TrimSpace(t.Invoice); v != "" {
		if !invoicePattern.MatchString(v) {
			return nil, courier.Validation("invoice", "invalid merchant order ID")
		}
		m["invoice"] = v
		set = append(set, "invoice")
	}
	if v := strings.TrimSpace(t.TrackingCode); v != "" {
		m["tracking_code"] = v
		set = append(set, "tracking_code")
	}
	if len(set) != 1 {
		return nil, courier.Validation("target", "exactly one of consignment ID, invoice or tracking code is required")
	}
	return m, nil
}

// ReturnInfo is a Steadfast return request. Status keeps the raw value
// (pending, approved, processing, completed or cancelled).
type ReturnInfo struct {
	ID            string
	UserID        string
	ConsignmentID string
	Reason        string
	Status        string // raw provider status
	CreatedAt     *time.Time
	UpdatedAt     *time.Time
	Raw           json.RawMessage
}

type returnJSON struct {
	ID            json.Number `json:"id"`
	UserID        json.Number `json:"user_id"`
	ConsignmentID json.Number `json:"consignment_id"`
	Reason        string      `json:"reason"`
	Status        string      `json:"status"`
	CreatedAt     string      `json:"created_at"`
	UpdatedAt     string      `json:"updated_at"`
}

func mapReturn(raw json.RawMessage) (*ReturnInfo, error) {
	var r returnJSON
	if err := httpx.DecodeJSON(ProviderID, "return.decode", raw, &r); err != nil {
		return nil, err
	}
	if r.ID.String() == "" {
		return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "return.decode", Message: "return request lacks an id"}
	}
	return &ReturnInfo{ID: r.ID.String(), UserID: r.UserID.String(), ConsignmentID: r.ConsignmentID.String(), Reason: r.Reason, Status: r.Status,
		CreatedAt: parseTime(r.CreatedAt), UpdatedAt: parseTime(r.UpdatedAt), Raw: raw}, nil
}

// unwrap returns the object under key (or "data"), or the body itself when it
// is already the object. Embedded failures are returned as errors.
func unwrap(op string, body []byte, keys ...string) (json.RawMessage, error) {
	var env map[string]json.RawMessage
	if err := httpx.DecodeJSON(ProviderID, op, body, &env); err != nil {
		return nil, err
	}
	var e envelope
	_ = json.Unmarshal(body, &e)
	if err := checkEmbedded(op, e); err != nil {
		return nil, err
	}
	for _, k := range append(keys, "data") {
		if v, ok := env[k]; ok && len(v) > 0 && string(v) != "null" {
			return v, nil
		}
	}
	return json.RawMessage(body), nil
}

// unwrapList returns the items of a bare array body or of {"data":[...]}.
func unwrapList(op string, body []byte) ([]json.RawMessage, error) {
	t := strings.TrimSpace(string(body))
	var items []json.RawMessage
	if strings.HasPrefix(t, "[") {
		err := httpx.DecodeJSON(ProviderID, op, body, &items)
		return items, err
	}
	raw, err := unwrap(op, body, "payments", "items")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: op, Message: "expected a list"}
	}
	err = httpx.DecodeJSON(ProviderID, op, raw, &items)
	return items, err
}

func pagePath(base string, page int) (string, error) {
	if page < 0 {
		return "", courier.Validation("page", "must not be negative")
	}
	if page == 0 {
		return base, nil
	}
	return base + "?" + url.Values{"page": {strconv.Itoa(page)}}.Encode(), nil
}

// CreateReturnRequest asks Steadfast to bring a parcel back to the merchant
// before delivery. It is a state-changing request (never retried; a timeout is
// indeterminate: list your return requests to reconcile).
func (c *Client) CreateReturnRequest(ctx context.Context, target ReturnTarget, reason string) (*ReturnInfo, error) {
	body, err := target.body()
	if err != nil {
		return nil, err
	}
	if r := strings.TrimSpace(reason); r != "" {
		body["reason"] = r
	}
	resp, _, err := c.call(ctx, "return.create", http.MethodPost, "/create_return_request", body, true)
	if err != nil {
		return nil, err
	}
	raw, err := unwrap("return.create", resp.Body, "return_request")
	if err != nil {
		return nil, err
	}
	info, err := mapReturn(raw)
	if err != nil {
		return nil, asIndeterminate(err)
	}
	return info, nil
}

// RequestReturn implements courier.ReturnRequester using the consignment ID.
func (c *Client) RequestReturn(ctx context.Context, r courier.ReturnRequest) (*courier.ReturnReceipt, error) {
	if r.Shipment.Provider != "" && r.Shipment.Provider != ProviderID {
		return nil, courier.Validation("shipment.provider", "not a steadfast shipment")
	}
	info, err := c.CreateReturnRequest(ctx, ReturnTarget{ConsignmentID: string(r.Shipment.ID)}, r.Reason)
	if err != nil {
		return nil, err
	}
	return &courier.ReturnReceipt{ID: info.ID, Status: courier.ProviderStatus{Code: info.Status}, Raw: info.Raw}, nil
}

// GetReturnRequest fetches one return request.
func (c *Client) GetReturnRequest(ctx context.Context, id string) (*ReturnInfo, error) {
	if !digitsPattern.MatchString(strings.TrimSpace(id)) {
		return nil, courier.Validation("id", "must be numeric")
	}
	resp, _, err := c.call(ctx, "return.get", http.MethodGet, "/get_return_request/"+url.PathEscape(strings.TrimSpace(id)), nil, false)
	if err != nil {
		return nil, err
	}
	raw, err := unwrap("return.get", resp.Body, "return_request")
	if err != nil {
		return nil, err
	}
	return mapReturn(raw)
}

// ListReturnRequests lists return requests, newest first, ten per page. Page 0
// means the provider default (first page).
func (c *Client) ListReturnRequests(ctx context.Context, page int) ([]ReturnInfo, error) {
	path, err := pagePath("/get_return_requests", page)
	if err != nil {
		return nil, err
	}
	resp, _, err := c.call(ctx, "return.list", http.MethodGet, path, nil, false)
	if err != nil {
		return nil, err
	}
	items, err := unwrapList("return.list", resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]ReturnInfo, 0, len(items))
	for _, it := range items {
		r, err := mapReturn(it)
		if err != nil {
			return out, err // never silently skip a malformed record (ERR-05)
		}
		out = append(out, *r)
	}
	return out, nil
}

// Payment is a payout made to the merchant. Amounts are nil when absent (never
// zero). The provider's timestamps (for example "2026-09-18 11:04:22") carry no
// timezone, so they are kept as the original text and never converted.
// Consignments are available in Raw for GetPayment.
type Payment struct {
	ID          string // for example "SFC-88213"
	Amount      *courier.Money
	Method      string
	StatusLabel string
	DueBills    *courier.Money
	PaidBills   *courier.Money
	Charges     *courier.Money
	Total       *courier.Money
	CreatedAt   string
	ReadyAt     string
	PaidAt      string
	Raw         json.RawMessage
}

type paymentJSON struct {
	PaymentID   json.RawMessage `json:"payment_id"`
	ID          json.RawMessage `json:"id"`
	Amount      any             `json:"amount"`
	Method      string          `json:"method"`
	StatusLabel string          `json:"status_label"`
	DueBills    any             `json:"due_bills"`
	PaidBills   any             `json:"paid_bills"`
	Charges     any             `json:"charges"`
	Total       any             `json:"total"`
	CreatedAt   string          `json:"created_at"`
	ReadyAt     string          `json:"ready_at"`
	PaidAt      string          `json:"paid_at"`
}

func payMoney(v any) (*courier.Money, error) {
	d, ok, err := courier.ParseAmount(toAmountInput(v))
	if err != nil {
		return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "payment.decode", Message: "invalid payment amount", Err: err}
	}
	if !ok {
		return nil, nil
	}
	m := courier.NewBDT(d)
	return &m, nil
}

func mapPayment(raw json.RawMessage) (*Payment, error) {
	var p paymentJSON
	if err := httpx.DecodeJSON(ProviderID, "payment.decode", raw, &p); err != nil {
		return nil, err
	}
	id := scalarText(p.PaymentID)
	if id == "" {
		id = scalarText(p.ID)
	}
	if id == "" {
		return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "payment.decode", Message: "payment lacks an id"}
	}
	out := &Payment{ID: id, Method: strings.TrimSpace(p.Method), StatusLabel: strings.TrimSpace(p.StatusLabel),
		CreatedAt: strings.TrimSpace(p.CreatedAt), ReadyAt: strings.TrimSpace(p.ReadyAt), PaidAt: strings.TrimSpace(p.PaidAt), Raw: raw}
	var err error
	for _, f := range []struct {
		dst *(*courier.Money)
		src any
	}{{&out.Amount, p.Amount}, {&out.DueBills, p.DueBills}, {&out.PaidBills, p.PaidBills}, {&out.Charges, p.Charges}, {&out.Total, p.Total}} {
		if *f.dst, err = payMoney(f.src); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scalarText returns a JSON string or number as text ("" when null or absent).
func scalarText(raw json.RawMessage) string {
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

// ListPayments lists payouts, ten per page (page 0 = default first page).
func (c *Client) ListPayments(ctx context.Context, page int) ([]Payment, error) {
	path, err := pagePath("/payments", page)
	if err != nil {
		return nil, err
	}
	resp, _, err := c.call(ctx, "payment.list", http.MethodGet, path, nil, false)
	if err != nil {
		return nil, err
	}
	items, err := unwrapList("payment.list", resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]Payment, 0, len(items))
	for _, it := range items {
		p, err := mapPayment(it)
		if err != nil {
			return out, err
		}
		out = append(out, *p)
	}
	return out, nil
}

// GetPayment fetches one payout with every parcel it settled (in Raw). The id
// may be "SFC-88213" or 88213: the provider ignores non-digits, so only the
// digits are sent.
func (c *Client) GetPayment(ctx context.Context, id string) (*Payment, error) {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, id)
	if digits == "" {
		return nil, courier.Validation("id", "must contain digits (for example SFC-88213 or 88213)")
	}
	resp, _, err := c.call(ctx, "payment.get", http.MethodGet, "/payments/"+digits, nil, false)
	if err != nil {
		return nil, err
	}
	raw, err := unwrap("payment.get", resp.Body, "payment")
	if err != nil {
		return nil, err
	}
	return mapPayment(raw)
}

// PoliceStation is a thana Steadfast delivers to.
type PoliceStation struct {
	ID       string
	Name     string
	District string
	Raw      json.RawMessage
}

// ListPoliceStations returns every thana with its district (reference data).
func (c *Client) ListPoliceStations(ctx context.Context) ([]PoliceStation, error) {
	resp, _, err := c.call(ctx, "locations.police_stations", http.MethodGet, "/police_stations", nil, false)
	if err != nil {
		return nil, err
	}
	items, err := unwrapList("locations.police_stations", resp.Body)
	if err != nil {
		return nil, err
	}
	out := make([]PoliceStation, 0, len(items))
	for i, it := range items {
		var p struct {
			ID       json.Number `json:"id"`
			Name     string      `json:"name"`
			District string      `json:"district"`
		}
		if err := httpx.DecodeJSON(ProviderID, "locations.police_stations", it, &p); err != nil {
			return out, err
		}
		if strings.TrimSpace(p.Name) == "" {
			return out, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "locations.police_stations", Message: fmt.Sprintf("record %d lacks a name", i)}
		}
		out = append(out, PoliceStation{ID: p.ID.String(), Name: p.Name, District: p.District, Raw: it})
	}
	return out, nil
}

// TrackWithReturnStatus calls /status_with_return_status_by_cid. The answer's
// delivery_status carries the return progress for a parcel that is coming back
// (for example "cancelled_return_rider_assigned"); the code is then returned as
// ReturnStatus, and the tracking status is Returning or Returned where that is
// unambiguous (a partly delivered parcel stays unknown). For any other parcel
// the value is a normal delivery status and ReturnStatus is nil. Coverage is
// current-only, as in TrackShipment.
func (c *Client) TrackWithReturnStatus(ctx context.Context, consignmentID string) (*courier.TrackingResult, *string, error) {
	const op = "tracking.get_with_return"
	id := strings.TrimSpace(consignmentID)
	if !digitsPattern.MatchString(id) {
		return nil, nil, courier.Validation("consignment_id", "must be numeric")
	}
	resp, creds, err := c.call(ctx, op, http.MethodGet, "/status_with_return_status_by_cid/"+url.PathEscape(id), nil, false)
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		envelope
		DeliveryStatus *string `json:"delivery_status"`
	}
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &out); err != nil {
		return nil, nil, err
	}
	if err := checkEmbedded(op, out.envelope); err != nil {
		return nil, nil, err
	}
	if out.DeliveryStatus == nil || strings.TrimSpace(*out.DeliveryStatus) == "" {
		return nil, nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: op, Message: "response lacks delivery_status"}
	}
	raw := strings.TrimSpace(*out.DeliveryStatus)
	res := mapStatus(creds.AccountID, courier.TrackingRequest{}, raw, resp.Body, time.Now().UTC())
	res.Ref.ID = courier.ShipmentID(id)
	if st, isReturn := returnStatuses[strings.ToLower(raw)]; isReturn {
		res.Status = st
		return res, &raw, nil
	}
	return res, nil, nil
}

// TrackingHistory returns every step a parcel has been through, looked up by
// your own invoice (GET /trackings_by_invoice/{invoice}). The history carries
// no status, so the result's Status is unknown and each event's status is
// unknown with the provider's text as its description; combine it with
// TrackShipment for the current state. An empty history declares unknown
// coverage rather than an empty "full" one.
func (c *Client) TrackingHistory(ctx context.Context, invoice string) (*courier.TrackingResult, error) {
	const op = "tracking.history"
	inv := strings.TrimSpace(invoice)
	if !invoicePattern.MatchString(inv) || utf8.RuneCountInString(inv) > MaxInvoiceLen {
		return nil, courier.Validation("invoice", "only letters, digits, hyphen and underscore, at most 100 characters")
	}
	resp, creds, err := c.call(ctx, op, http.MethodGet, "/trackings_by_invoice/"+url.PathEscape(inv), nil, false)
	if err != nil {
		return nil, err
	}
	var out struct {
		envelope
		Tracking []json.RawMessage `json:"tracking"`
	}
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &out); err != nil {
		return nil, err
	}
	if err := checkEmbedded(op, out.envelope); err != nil {
		return nil, err
	}
	res := &courier.TrackingResult{
		Ref:    courier.ShipmentRef{Provider: ProviderID, Account: creds.AccountID},
		Status: courier.StatusUnknown, Coverage: courier.CoverageUnknown, COD: courier.CODUnknown,
		ObservedAt: time.Now().UTC(), Raw: resp.Body,
	}
	for _, it := range out.Tracking {
		var e struct {
			ConsignmentID json.Number `json:"consignment_id"`
			TrackingType  json.Number `json:"tracking_type"`
			Text          string      `json:"text"`
			CreatedAt     string      `json:"created_at"`
		}
		if err := httpx.DecodeJSON(ProviderID, op, it, &e); err != nil {
			return res, err // never silently skip a malformed record (ERR-05)
		}
		if res.Ref.ID == "" && e.ConsignmentID.String() != "" {
			res.Ref.ID = courier.ShipmentID(e.ConsignmentID.String())
		}
		res.Events = append(res.Events, courier.TrackingEvent{OccurredAt: parseTime(e.CreatedAt), Status: courier.StatusUnknown,
			ProviderStatus: courier.ProviderStatus{Code: e.TrackingType.String(), Description: strings.TrimSpace(e.Text)},
			Description:    strings.TrimSpace(e.Text), Raw: it})
	}
	if len(res.Events) > 0 {
		res.Coverage = courier.CoverageFull
	}
	return res, nil
}

var _ courier.ReturnRequester = (*Client)(nil)
