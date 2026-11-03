package ecourier

import "encoding/json"

// envelope is the common response wrapper. success may be a bool or the
// strings "true"/"false"; response_code is a number.
type envelope struct {
	Success      json.RawMessage `json:"success"`
	ResponseCode json.Number     `json:"response_code"`
	Message      json.RawMessage `json:"message"`
	Errors       []any           `json:"errors"`
	QueryData    json.RawMessage `json:"query_data"`
	Data         json.RawMessage `json:"data"`
	ID           string          `json:"ID"`
}

type orderBody struct {
	RecipientName      string       `json:"recipient_name"`
	RecipientMobile    string       `json:"recipient_mobile"`
	RecipientCity      string       `json:"recipient_city"`
	RecipientThana     string       `json:"recipient_thana"`
	RecipientArea      string       `json:"recipient_area"`
	RecipientAddress   string       `json:"recipient_address"`
	PackageCode        string       `json:"package_code"`
	ProductPrice       json.Number  `json:"product_price"`
	PaymentMethod      string       `json:"payment_method"`
	RecipientZip       string       `json:"recipient_zip"`
	ParcelType         string       `json:"parcel_type,omitempty"`
	RequestedDelivery  string       `json:"requested_delivery_time,omitempty"`
	ProductID          string       `json:"product_id,omitempty"`
	PickAddress        string       `json:"pick_address,omitempty"`
	PickHub            *int64       `json:"pick_hub,omitempty"`
	Comments           string       `json:"comments,omitempty"`
	NumberOfItem       int          `json:"number_of_item,omitempty"`
	ActualProductPrice *json.Number `json:"actual_product_price,omitempty"`
	SpecialInstruction string       `json:"special_instruction,omitempty"`
	PGWID              string       `json:"pgwid,omitempty"`
	PGWTxnID           string       `json:"pgwtxn_id,omitempty"`
	IsFragile          int          `json:"is_fragile"`
	SendingType        int          `json:"sending_type,omitempty"`
	IsIPay             int          `json:"is_ipay"`
	HubDelivery        string       `json:"hub_delivery,omitempty"`
	HubDeliveryID      *int64       `json:"hub_delivery_id,omitempty"`
}

type trackBody struct {
	ProductID string `json:"product_id,omitempty"`
	ECR       string `json:"ecr,omitempty"`
}

// parcel is one entry of query_data in tracking responses.
type parcel struct {
	Status        [][]json.RawMessage `json:"status"`
	REFID         string              `json:"REFID"`
	ProductID     string              `json:"product_id"`
	PaymentMethod string              `json:"paymentmethod"`
	PaymentClear  string              `json:"paymentclear"`
	ShippingPrice json.RawMessage     `json:"shipping_price"`
	COD           json.RawMessage     `json:"cod"`
	ProductPrice  json.RawMessage     `json:"product_price"`
}
