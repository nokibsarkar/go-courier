package lalamove

import (
	"encoding/json"
	"strings"
)

// flexString accepts a JSON string or number (identifiers are opaque strings).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*f = ""
		return nil
	}
	if s[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexString(v)
		return nil
	}
	*f = flexString(s)
	return nil
}

// ---- requests ----

type quoteBody struct {
	Data quoteReq `json:"data"`
}

type quoteReq struct {
	ServiceType     string    `json:"serviceType"`
	Language        string    `json:"language"`
	Stops           []stopReq `json:"stops"`
	ScheduleAt      string    `json:"scheduleAt,omitempty"`
	SpecialRequests []string  `json:"specialRequests,omitempty"`
}

type stopReq struct {
	Coordinates coordReq `json:"coordinates"`
	Address     string   `json:"address"`
}

type coordReq struct {
	Lat string `json:"lat"`
	Lng string `json:"lng"`
}

type orderBody struct {
	Data orderReq `json:"data"`
}

type orderReq struct {
	QuotationID  string            `json:"quotationId"`
	Sender       personReq         `json:"sender"`
	Recipients   []personReq       `json:"recipients"`
	IsPODEnabled *bool             `json:"isPODEnabled,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type personReq struct {
	StopID  string `json:"stopId"`
	Name    string `json:"name"`
	Phone   string `json:"phone"`
	Remarks string `json:"remarks,omitempty"`
}

// ---- responses ----

type quoteEnvelope struct {
	Data *quotationData `json:"data"`
}

type quotationData struct {
	QuotationID    flexString      `json:"quotationId"`
	ScheduleAt     string          `json:"scheduleAt"`
	ExpiresAt      string          `json:"expiresAt"`
	ServiceType    string          `json:"serviceType"`
	Stops          []quoteStopData `json:"stops"`
	PriceBreakdown *priceData      `json:"priceBreakdown"`
}

type quoteStopData struct {
	StopID      flexString `json:"stopId"`
	Coordinates struct {
		Lat flexString `json:"lat"`
		Lng flexString `json:"lng"`
	} `json:"coordinates"`
	Address string `json:"address"`
}

type priceData struct {
	Total    json.RawMessage `json:"total"`
	Currency string          `json:"currency"`
}

type orderEnvelope struct {
	Data *orderData `json:"data"`
}

type orderData struct {
	OrderID        flexString      `json:"orderId"`
	QuotationID    flexString      `json:"quotationId"`
	PriceBreakdown *priceData      `json:"priceBreakdown"`
	DriverID       flexString      `json:"driverId"`
	ShareLink      string          `json:"shareLink"`
	Status         string          `json:"status"`
	Stops          []orderStopData `json:"stops"`
}

type orderStopData struct {
	StopID flexString `json:"stopId"`
	POD    *podData   `json:"POD"`
}

type podData struct {
	Status      string `json:"status"`
	Image       string `json:"image"`
	DeliveredAt string `json:"deliveredAt"`
}

type driverEnvelope struct {
	Data *driverData `json:"data"`
}

type driverData struct {
	DriverID    flexString `json:"driverId"`
	Name        string     `json:"name"`
	Phone       string     `json:"phone"`
	PlateNumber string     `json:"plateNumber"`
	Photo       string     `json:"photo"`
	Coordinates *struct {
		Lat       flexString `json:"lat"`
		Lng       flexString `json:"lng"`
		UpdatedAt string     `json:"updatedAt"`
	} `json:"coordinates"`
}

type errorEnvelope struct {
	Errors []struct {
		ID flexString `json:"id"`
	} `json:"errors"`
}
