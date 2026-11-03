package uber

import "encoding/json"

// Wire models for the Direct API.

type tokenResponse struct {
	AccessToken string      `json:"access_token"`
	ExpiresIn   json.Number `json:"expires_in"`
	TokenType   string      `json:"token_type"`
	Error       string      `json:"error"`
}

type quoteResponse struct {
	Kind         string          `json:"kind"`
	ID           string          `json:"id"`
	Fee          json.RawMessage `json:"fee"`
	Currency     string          `json:"currency"`
	CurrencyType string          `json:"currency_type"`
	Expires      string          `json:"expires"`
	Code         string          `json:"code"`
}

type deliveryResponse struct {
	Kind         string            `json:"kind"`
	ID           string            `json:"id"`
	Status       string            `json:"status"`
	Fee          json.RawMessage   `json:"fee"`
	Currency     string            `json:"currency"`
	CurrencyType string            `json:"currency_type"`
	ExternalID   string            `json:"external_id"`
	Created      string            `json:"created"`
	Updated      string            `json:"updated"`
	Code         string            `json:"code"`
	Pickup       *waypointResponse `json:"pickup"`
	Dropoff      *waypointResponse `json:"dropoff"`
}

// waypointResponse is the status part of the spec's WaypointInfo.
type waypointResponse struct {
	Status          string `json:"status"` // completed | failed
	StatusTimestamp string `json:"status_timestamp"`
}

type listResponse struct {
	Data     []json.RawMessage `json:"data"`
	NextHref string            `json:"next_href"`
}

type podRequest struct {
	Waypoint string `json:"waypoint"`
	Type     string `json:"type"`
}

type podResponse struct {
	Document string `json:"document"`
}

type manifestItem struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
}

type quoteRequest struct {
	PickupAddress  string `json:"pickup_address"`
	DropoffAddress string `json:"dropoff_address"`
	PickupReadyDT  string `json:"pickup_ready_dt,omitempty"`
}

type createRequest struct {
	QuoteID        string         `json:"quote_id,omitempty"`
	PickupName     string         `json:"pickup_name"`
	PickupAddress  string         `json:"pickup_address"`
	PickupPhone    string         `json:"pickup_phone_number"`
	PickupNotes    string         `json:"pickup_notes,omitempty"`
	DropoffName    string         `json:"dropoff_name"`
	DropoffAddress string         `json:"dropoff_address"`
	DropoffPhone   string         `json:"dropoff_phone_number"`
	DropoffNotes   string         `json:"dropoff_notes,omitempty"`
	ManifestItems  []manifestItem `json:"manifest_items"`
	ExternalID     string         `json:"external_id,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
}

type structuredAddress struct {
	StreetAddress []string `json:"street_address"`
	City          string   `json:"city"`
	State         string   `json:"state,omitempty"`
	ZipCode       string   `json:"zip_code,omitempty"`
	Country       string   `json:"country"`
}

// hookEvent is a delivery-status style webhook envelope.
type hookEvent struct {
	Kind       string          `json:"kind"`
	EventType  string          `json:"event_type"`
	Type       string          `json:"type"` // direct.organization uses type, not kind
	Timestamp  string          `json:"timestamp"`
	ID         string          `json:"id"`
	DeliveryID string          `json:"delivery_id"`
	Status     string          `json:"status"`
	Created    json.RawMessage `json:"created"`
	Data       json.RawMessage `json:"data"`
}

type hookData struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Updated string `json:"updated"`
}
