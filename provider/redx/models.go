package redx

import "encoding/json"

type trackResponse struct {
	Tracking []trackEntry `json:"tracking"`
}

type trackEntry struct {
	MessageEN string `json:"message_en"`
	MessageBN string `json:"message_bn"`
	Time      string `json:"time"`
}

type infoResponse struct {
	Parcel *infoParcel `json:"parcel"`
}

type infoParcel struct {
	TrackingID     string          `json:"tracking_id"`
	Status         string          `json:"status"`
	Charge         json.RawMessage `json:"charge"`
	CashCollection json.RawMessage `json:"cash_collection_amount"`
	InvoiceID      string          `json:"merchant_invoice_id"`
	CreatedAt      string          `json:"created_at"`
	DeliveryType   string          `json:"delivery_type"`
}

type createBody struct {
	CustomerName         string         `json:"customer_name"`
	CustomerPhone        string         `json:"customer_phone"`
	DeliveryArea         string         `json:"delivery_area"`
	DeliveryAreaID       int64          `json:"delivery_area_id"`
	CustomerAddress      string         `json:"customer_address"`
	MerchantInvoiceID    string         `json:"merchant_invoice_id,omitempty"`
	CashCollectionAmount string         `json:"cash_collection_amount"`
	PaidReturnCharge     json.Number    `json:"paid_return_charge,omitempty"`
	ParcelWeight         uint32         `json:"parcel_weight"`
	Instruction          string         `json:"instruction,omitempty"`
	Type                 string         `json:"type,omitempty"`
	Value                json.Number    `json:"value"`
	IsClosedBox          *bool          `json:"is_closed_box,omitempty"`
	PickupStoreID        *int64         `json:"pickup_store_id,omitempty"`
	ParcelDetails        []parcelDetail `json:"parcel_details_json,omitempty"`
}

type parcelDetail struct {
	Name     string      `json:"name"`
	Category string      `json:"category"`
	Value    json.Number `json:"value"`
}

type updateBody struct {
	EntityType    string        `json:"entity_type"`
	EntityID      string        `json:"entity_id"`
	UpdateDetails updateDetails `json:"update_details"`
}

type updateDetails struct {
	PropertyName string `json:"property_name"`
	NewValue     string `json:"new_value"`
	Reason       string `json:"reason,omitempty"`
}
