package courier

import (
	"io"
	"time"
)

// Coordinates is a WGS84 point.
type Coordinates struct{ Lat, Lng float64 }

// LocationRef is a provider-scoped location reference (city/zone/area/store).
type LocationRef struct {
	Provider ProviderID
	Kind     string
	ID       string
}

// Address permits free text, postal/geographic components, coordinates and
// provider-scoped references (DOM-05). Adapters must fail with ErrAmbiguous
// rather than guess when mapping uncertain addresses.
type Address struct {
	Text        string
	Line1       string
	Line2       string
	Area        string
	City        string
	District    string
	State       string
	PostalCode  string
	Country     string
	Coordinates *Coordinates
	Refs        []LocationRef
}

// Contact is a person or business contact. Phone numbers are personal data
// (PRIV-01) and are never logged by the SDK.
type Contact struct {
	Name             string
	Phone            string
	AlternativePhone string
	Email            string
}

// Party is one end of a delivery.
type Party struct {
	Contact      Contact
	Address      Address
	PickupPoint  PickupPointID // provider-scoped pickup/store point, optional
	Store        StoreID
	Instructions string
}

// Dimensions are in millimetres.
type Dimensions struct{ LengthMM, WidthMM, HeightMM uint32 }

// Package carries physical and financial attributes. COD, declared value,
// insurance and delivery charges are independent monetary concepts (DOM-04).
type Package struct {
	Description   string
	Quantity      int
	WeightGrams   *uint32
	Dimensions    *Dimensions
	DeclaredValue *Money
	Insurance     *Money
	Restrictions  []string
}

// DeliveryType selects home or point delivery.
type DeliveryType string

const (
	DeliveryHome  DeliveryType = "home"
	DeliveryPoint DeliveryType = "point"
)

// CreateShipmentRequest is the canonical shipment-creation input.
type CreateShipmentRequest struct {
	// IdempotencyKey is forwarded only where the provider documents support.
	IdempotencyKey  string
	MerchantOrderID MerchantOrderID
	Pickup          Party
	Recipient       Party
	Package         Package
	// COD is the cash to collect; nil means no COD (never zero-by-omission).
	COD            *Money
	DeliveryCharge *Money
	Delivery       DeliveryType
	// QuoteID, when set, binds creation to a previously obtained quote.
	QuoteID QuoteID
	Note    string
}

// Shipment is a created or retrieved shipment. Timestamps are pointers: a
// missing provider timestamp is absent, not the observation time (DOM-06).
type Shipment struct {
	Ref             ShipmentRef
	MerchantOrderID MerchantOrderID
	TrackingCode    TrackingCode
	ProviderStatus  ProviderStatus
	Status          Status
	COD             *Money
	DeliveryCharge  *Money
	CreatedAt       *time.Time
	UpdatedAt       *time.Time
	// Raw is the provider payload this value was derived from.
	Raw []byte
}

// BatchStatus is the per-item outcome of a batch operation (ERR-04).
type BatchStatus string

const (
	BatchSucceeded     BatchStatus = "succeeded"
	BatchFailed        BatchStatus = "failed"
	BatchIndeterminate BatchStatus = "indeterminate"
	BatchNotAttempted  BatchStatus = "not_attempted"
)

// BatchItem is one item's receipt. Index refers to the request slice.
type BatchItem struct {
	Index    int
	Status   BatchStatus
	Shipment *Shipment
	Err      error
}

// BatchResult retains per-item receipts, failures, indeterminate and
// not-attempted items. A non-nil result may accompany a non-nil error.
type BatchResult struct {
	Items []BatchItem
	Raw   []byte
}

// Quote is a priced offer. Expiry and currency come from the provider.
type Quote struct {
	Ref       ShipmentRef // Provider/Account scope; ID is unused
	ID        QuoteID
	Fee       Money
	ExpiresAt *time.Time
	Raw       []byte
}

// QuoteRequest asks for a price.
type QuoteRequest struct {
	Pickup     Party
	Recipient  Party
	Package    Package
	COD        *Money
	ScheduleAt *time.Time
}

// Availability is a tri-state used where "unknown" must stay distinct.
type Availability string

const (
	Available   Availability = "available"
	Unavailable Availability = "unavailable"
	Unknown     Availability = "unknown"
)

// ServiceabilityRequest asks whether a route is serviceable.
type ServiceabilityRequest struct {
	Pickup    Party
	Recipient Party
	Package   Package
}

// Serviceability is the provider's answer; Reason is provider text.
type Serviceability struct {
	Availability Availability
	Reason       string
	Raw          []byte
}

// PickupRequest explicitly requests a pickup (a state-changing action, CAP-02).
type PickupRequest struct {
	Pickup    Party
	ReadyAt   *time.Time
	Shipments []ShipmentRef
}

// Pickup is the receipt for a pickup request.
type Pickup struct {
	ID     string
	Status ProviderStatus
	Raw    []byte
}

// Balance is an account balance in the account's real currency.
type Balance struct {
	Provider ProviderID
	Account  AccountID
	Amount   Money
	Raw      []byte
}

// Document is a downloadable file (label, proof of delivery). Large files are
// streamed (DOM-10): the caller must Close Body. URL is set instead when the
// provider issues an expiring reference; the SDK never fetches external URLs.
type Document struct {
	ContentType string
	Body        io.ReadCloser
	URL         string
	ExpiresAt   *time.Time
}

// ReturnRequest asks for a reverse-logistics return (distinct from cancel).
type ReturnRequest struct {
	Shipment ShipmentRef
	Reason   string
}

// ReturnReceipt is a created return.
type ReturnReceipt struct {
	ID     string
	Status ProviderStatus
	Raw    []byte
}
