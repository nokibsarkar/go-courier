package steadfast

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/httpx"
)

var pickupPhonePattern = regexp.MustCompile(`^01[3-9][0-9]{8}$`)

// PickupRequest asks for a rider to collect from one of your saved addresses.
type PickupRequest struct {
	// AddressID is one of your saved pickup addresses (your Pickup Addresses page).
	AddressID int64
	// PoliceStationID is the thana the address is in (see ListPoliceStations).
	PoliceStationID int64
	// Address is the address in words, up to 255 characters.
	Address string
	// ContactNumber is who the rider should call: eleven digits, 013 to 019.
	ContactNumber string
	// Note is optional, up to 500 characters.
	Note string
	// EstimatedQty is roughly how many parcels are waiting; nil omits it.
	EstimatedQty *int
}

// PickupInfo is a created pickup request.
type PickupInfo struct {
	ID              string
	AddressID       string
	PoliceStationID string
	Location        string
	ContactNumber   string
	Note            *string
	EstimatedQty    *int64
	Status          string // req_status, raw (0 when newly created)
	Raw             json.RawMessage
}

type pickupBody struct {
	AddressID       int64  `json:"address_id"`
	PoliceStationID int64  `json:"police_station_id"`
	Address         string `json:"address"`
	ContactNumber   string `json:"contact_number"`
	Note            string `json:"note,omitempty"`
	EstimQty        *int   `json:"estim_qty,omitempty"`
}

// CreatePickupRequest asks for a rider (POST /create_pickup_request). The
// provider answers 201. Asking again for an address that already has a pending
// request is refused with 409 PICKUP_REQUEST_EXISTS (a courier.ErrConflict
// error): retrying cannot send two riders. The SDK still does not replay a
// timed-out request for you; it is indeterminate.
func (c *Client) CreatePickupRequest(ctx context.Context, r PickupRequest) (*PickupInfo, error) {
	const op = "pickup.create"
	if r.AddressID <= 0 {
		return nil, courier.Validation("address_id", "required")
	}
	if r.PoliceStationID <= 0 {
		return nil, courier.Validation("police_station_id", "required")
	}
	addr := strings.TrimSpace(r.Address)
	if addr == "" || utf8.RuneCountInString(addr) > 255 {
		return nil, courier.Validation("address", "required, at most 255 characters")
	}
	phone, err := localPhone("contact_number", r.ContactNumber)
	if err != nil {
		return nil, err
	}
	if !pickupPhonePattern.MatchString(phone) {
		return nil, courier.Validation("contact_number", "eleven digits beginning 013 to 019")
	}
	note := strings.TrimSpace(r.Note)
	if utf8.RuneCountInString(note) > 500 {
		return nil, courier.Validation("note", "at most 500 characters")
	}
	if r.EstimatedQty != nil && *r.EstimatedQty < 0 {
		return nil, courier.Validation("estimated_qty", "must not be negative")
	}
	body := pickupBody{AddressID: r.AddressID, PoliceStationID: r.PoliceStationID, Address: addr, ContactNumber: phone, Note: note, EstimQty: r.EstimatedQty}
	resp, _, err := c.call(ctx, op, http.MethodPost, "/create_pickup_request", body, true)
	if err != nil {
		return nil, err
	}
	var out struct {
		envelope
		Data struct {
			ID        json.RawMessage `json:"id"`
			AddressID json.RawMessage `json:"user_address_id"`
			Station   json.RawMessage `json:"police_station_id"`
			Location  string          `json:"pickup_location"`
			Contact   string          `json:"contact_number"`
			Note      *string         `json:"note"`
			Qty       json.RawMessage `json:"estim_qty"`
			ReqStatus json.RawMessage `json:"req_status"`
		} `json:"data"`
	}
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &out); err != nil {
		return nil, asIndeterminate(err)
	}
	if err := checkEmbedded(op, out.envelope); err != nil {
		return nil, err
	}
	id := scalarText(out.Data.ID)
	if id == "" {
		return nil, indeterminateProtocol(op, "response lacks data.id")
	}
	return &PickupInfo{ID: id, AddressID: scalarText(out.Data.AddressID), PoliceStationID: scalarText(out.Data.Station),
		Location: strings.TrimSpace(out.Data.Location), ContactNumber: strings.TrimSpace(out.Data.Contact), Note: out.Data.Note,
		EstimatedQty: wholeCount(out.Data.Qty), Status: scalarText(out.Data.ReqStatus), Raw: append(json.RawMessage(nil), resp.Body...)}, nil
}
