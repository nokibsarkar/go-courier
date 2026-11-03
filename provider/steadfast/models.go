package steadfast

import (
	"encoding/json"
	"strings"

	"github.com/nokibsarkar/go-courier/httpx"
)

type orderRequest struct {
	Invoice          string      `json:"invoice"`
	RecipientName    string      `json:"recipient_name"`
	RecipientPhone   string      `json:"recipient_phone"`
	AlternativePhone string      `json:"alternative_phone,omitempty"`
	RecipientEmail   string      `json:"recipient_email,omitempty"`
	RecipientAddress string      `json:"recipient_address"`
	CODAmount        json.Number `json:"cod_amount"` // exact decimal literal, never float
	Note             string      `json:"note,omitempty"`
	ItemDescription  string      `json:"item_description,omitempty"`
	TotalLot         int         `json:"total_lot,omitempty"`
	DeliveryType     int         `json:"delivery_type"`
}

type bulkRequest struct {
	Data []orderRequest `json:"data"`
}

// envelope captures the embedded status common to responses.
type envelope struct {
	Status  json.Number     `json:"status"`
	Message string          `json:"message"`
	Errors  json.RawMessage `json:"errors"`
}

type createResponse struct {
	envelope
	Consignment *consignment `json:"consignment"`
}

type consignment struct {
	ConsignmentID json.Number `json:"consignment_id"`
	Invoice       string      `json:"invoice"`
	TrackingCode  string      `json:"tracking_code"`
	Status        string      `json:"status"`
	CODAmount     any         `json:"cod_amount"`
	CreatedAt     string      `json:"created_at"`
	UpdatedAt     string      `json:"updated_at"`
}

type bulkItem struct {
	Invoice       string          `json:"invoice"`
	ConsignmentID json.Number     `json:"consignment_id"`
	TrackingCode  string          `json:"tracking_code"`
	Status        string          `json:"status"`
	Error         json.RawMessage `json:"error"`
	CODAmount     any             `json:"cod_amount"`
}

// errText flattens the item error: a JSON-encoded array of codes inside a
// string (standard bulk), an array of sentences (extended bulk) or plain text.
func (b bulkItem) errText() string {
	raw := strings.TrimSpace(string(b.Error))
	if raw == "" || raw == "null" {
		return ""
	}
	var list []string
	var s string
	if json.Unmarshal(b.Error, &list) == nil {
		return strings.Join(list, ", ")
	}
	if json.Unmarshal(b.Error, &s) == nil {
		if json.Unmarshal([]byte(s), &list) == nil {
			return strings.Join(list, ", ")
		}
		return strings.TrimSpace(s)
	}
	return ""
}

type statusResponse struct {
	envelope
	DeliveryStatus *string `json:"delivery_status"`
}

type balanceResponse struct {
	envelope
	CurrentBalance any `json:"current_balance"`
}

// normalise trims padding in bulk items (for example " error " and " null ") and drops the literal string "null" so it is never
// mistaken for a real tracking code.
func (b *bulkItem) normalise() {
	b.Invoice = strings.TrimSpace(b.Invoice)
	b.Status = strings.TrimSpace(b.Status)
	b.TrackingCode = strings.TrimSpace(b.TrackingCode)
	if strings.EqualFold(b.TrackingCode, "null") {
		b.TrackingCode = ""
	}
}

// decodeBulkItems accepts a bare array or a {"data":[...]} wrapper.
func decodeBulkItems(body []byte) ([]bulkItem, error) {
	raws, err := unwrapList(shipmentBatchOp, body)
	if err != nil {
		return nil, err
	}
	items := make([]bulkItem, 0, len(raws))
	for _, r := range raws {
		var it bulkItem
		if err := httpx.DecodeJSON(ProviderID, shipmentBatchOp, r, &it); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}
