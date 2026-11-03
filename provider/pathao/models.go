package pathao

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// flex decodes a JSON string or number into its string form (Pathao IDs are
// sometimes numbers, sometimes strings).
type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	s := bytes.TrimSpace(b)
	switch {
	case len(s) == 0 || string(s) == "null":
		*f = ""
	case s[0] == '"':
		var v string
		if err := json.Unmarshal(s, &v); err != nil {
			return err
		}
		*f = flex(strings.TrimSpace(v))
	case s[0] == '{' || s[0] == '[' || s[0] == 't' || s[0] == 'f':
		return fmt.Errorf("unexpected JSON value for scalar")
	default:
		*f = flex(string(s))
	}
	return nil
}

func (f flex) String() string { return string(f) }

// parseBool reads true/false/1/0 (number or string); nil when absent/unknown.
func parseBool(raw json.RawMessage) *bool {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	var v bool
	switch strings.ToLower(s) {
	case "true", "1":
		v = true
	case "false", "0":
		v = false
	default:
		return nil
	}
	return &v
}

// envelope is the common Pathao response wrapper.
type envelope struct {
	Type    string          `json:"type"`
	Code    json.Number     `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type tokenRequest struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	GrantType    string `json:"grant_type"`
}

type tokenResponse struct {
	TokenType    string      `json:"token_type"`
	ExpiresIn    json.Number `json:"expires_in"`
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
}

type orderRequest struct {
	StoreID                 json.Number `json:"store_id"`
	MerchantOrderID         string      `json:"merchant_order_id,omitempty"`
	RecipientName           string      `json:"recipient_name"`
	RecipientPhone          string      `json:"recipient_phone"`
	RecipientSecondaryPhone string      `json:"recipient_secondary_phone,omitempty"`
	RecipientAddress        string      `json:"recipient_address"`
	RecipientCity           json.Number `json:"recipient_city"`
	RecipientZone           json.Number `json:"recipient_zone"`
	RecipientArea           json.Number `json:"recipient_area"`
	DeliveryType            int         `json:"delivery_type"`
	ItemType                int         `json:"item_type"`
	SpecialInstruction      string      `json:"special_instruction,omitempty"`
	ItemQuantity            int         `json:"item_quantity"`
	ItemWeight              json.Number `json:"item_weight"`
	ItemDescription         string      `json:"item_description,omitempty"`
	AmountToCollect         json.Number `json:"amount_to_collect"` // exact decimal literal, never float
}

type priceRequest struct {
	StoreID       json.Number `json:"store_id"`
	ItemType      int         `json:"item_type"`
	DeliveryType  int         `json:"delivery_type"`
	ItemWeight    json.Number `json:"item_weight"`
	RecipientCity json.Number `json:"recipient_city"`
	RecipientZone json.Number `json:"recipient_zone"`
}

type priceData struct {
	Price      json.RawMessage `json:"price"`
	Discount   json.RawMessage `json:"discount"`
	FinalPrice json.RawMessage `json:"final_price"`
}

type createData struct {
	ConsignmentID   flex            `json:"consignment_id"`
	MerchantOrderID flex            `json:"merchant_order_id"`
	OrderStatus     string          `json:"order_status"`
	DeliveryFee     json.RawMessage `json:"delivery_fee"`
}

type infoData struct {
	ConsignmentID   flex   `json:"consignment_id"`
	MerchantOrderID flex   `json:"merchant_order_id"`
	OrderStatus     string `json:"order_status"`
	OrderStatusSlug string `json:"order_status_slug"`
	InvoiceID       flex   `json:"invoice_id"`
}

type storePage struct {
	Data     []rawStore  `json:"data"`
	LastPage json.Number `json:"last_page"`
}

type rawStore struct {
	StoreID      flex            `json:"store_id"`
	StoreName    string          `json:"store_name"`
	StoreAddress string          `json:"store_address"`
	IsActive     json.RawMessage `json:"is_active"`
}

type listData[T any] struct {
	Data []T `json:"data"`
}

type rawCity struct {
	ID   flex   `json:"city_id"`
	Name string `json:"city_name"`
}

type rawZone struct {
	ID   flex   `json:"zone_id"`
	Name string `json:"zone_name"`
}

type rawArea struct {
	ID                    flex            `json:"area_id"`
	Name                  string          `json:"area_name"`
	HomeDeliveryAvailable json.RawMessage `json:"home_delivery_available"`
	PickupAvailable       json.RawMessage `json:"pickup_available"`
}
