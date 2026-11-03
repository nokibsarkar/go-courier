package uber

import (
	"encoding/json"
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// RefundRequest is the decoded body of an event.refund_request webhook
// webhook. Amounts are exact Money converted
// from integer cents; an absent amount is nil, never zero.
type RefundRequest struct {
	EventID    string
	DeliveryID string
	ExternalID string
	StoreID    string
	RefundID   string
	CreatedAt  *time.Time
	Currency   courier.Currency

	TotalPartnerRefund *courier.Money
	TotalUberRefund    *courier.Money
	Fees               []RefundFee
	Items              []RefundOrderItem
}

// RefundFee is one refund_fees entry. FeeCode is UBER_DELIVERY_FEE,
// PARTNER_FEE or PARTNER_TAX; Category is DELIVERY or TAX (raw values kept).
type RefundFee struct {
	FeeCode  string
	Category string
	Value    *courier.Money
}

// RefundOrderItem is one refund_order_items entry. Reason and PartyAtFault are
// the provider's raw strings (the reason list is open-ended: "could include").
type RefundOrderItem struct {
	PartyAtFault        string
	Reason              string
	PartnerRefundAmount *courier.Money
	UberRefundAmount    *courier.Money
	Items               []RefundItem
}

// RefundItem names a refunded manifest item.
type RefundItem struct {
	Name     string
	Quantity int
}

type refundHook struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	DeliveryID string `json:"delivery_id"`
	ExternalID string `json:"external_id"`
	StoreID    string `json:"external_store_id"`
	Data       struct {
		ID        string          `json:"id"`
		CreatedAt string          `json:"created_at"`
		Currency  string          `json:"currency_code"`
		TotalP    json.RawMessage `json:"total_partner_refund"`
		TotalU    json.RawMessage `json:"total_uber_refund"`
		Fees      []struct {
			Category string          `json:"category"`
			FeeCode  string          `json:"fee_code"`
			Value    json.RawMessage `json:"value"`
		} `json:"refund_fees"`
		Items []struct {
			PartnerAmt json.RawMessage `json:"partner_refund_amount"`
			UberAmt    json.RawMessage `json:"uber_refund_amount"`
			Fault      string          `json:"party_at_fault"`
			Reason     string          `json:"reason"`
			Items      []struct {
				Name     string `json:"name"`
				Quantity int    `json:"quantity"`
			} `json:"refund_items"`
		} `json:"refund_order_items"`
	} `json:"data"`
}

// ParseRefundRequest decodes a verified event.refund_request event. It does
// not verify anything itself: obtain the Event from DecodeWebhook or the
// webhook handler and check Trust before acting on it.
func ParseRefundRequest(e courier.Event) (*RefundRequest, error) {
	if e.Provider != ProviderID || e.Type != evRefundRequest {
		return nil, courier.Validation("event", "not an Uber event.refund_request event")
	}
	var h refundHook
	if err := json.Unmarshal(e.Payload, &h); err != nil {
		return nil, protocol("webhook.refund", "malformed refund event")
	}
	cur, err := providerCurrency("webhook.refund", "", h.Data.Currency)
	if err != nil {
		return nil, err
	}
	money := func(raw json.RawMessage) (*courier.Money, error) {
		m, _, err := minorMoney("webhook.refund", raw, cur)
		return m, err
	}
	r := &RefundRequest{EventID: h.ID, DeliveryID: h.DeliveryID, ExternalID: h.ExternalID, StoreID: h.StoreID,
		RefundID: h.Data.ID, CreatedAt: parseTime(h.Data.CreatedAt), Currency: cur}
	if r.TotalPartnerRefund, err = money(h.Data.TotalP); err != nil {
		return nil, err
	}
	if r.TotalUberRefund, err = money(h.Data.TotalU); err != nil {
		return nil, err
	}
	for _, f := range h.Data.Fees {
		v, err := money(f.Value)
		if err != nil {
			return nil, err
		}
		r.Fees = append(r.Fees, RefundFee{FeeCode: f.FeeCode, Category: f.Category, Value: v})
	}
	for _, it := range h.Data.Items {
		o := RefundOrderItem{PartyAtFault: it.Fault, Reason: it.Reason}
		if o.PartnerRefundAmount, err = money(it.PartnerAmt); err != nil {
			return nil, err
		}
		if o.UberRefundAmount, err = money(it.UberAmt); err != nil {
			return nil, err
		}
		for _, x := range it.Items {
			o.Items = append(o.Items, RefundItem{Name: x.Name, Quantity: x.Quantity})
		}
		r.Items = append(r.Items, o)
	}
	return r, nil
}
