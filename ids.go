package courier

// Opaque identifier types (DOM-01). They are never parsed as numbers by the
// canonical model; adapters convert at their boundary.
type (
	ProviderID      string
	AccountID       string
	StoreID         string
	PickupPointID   string
	ShipmentID      string
	MerchantOrderID string
	TrackingCode    string
	QuoteID         string
	IncidentID      string
	ConsumerID      string
)

// ShipmentRef identifies a shipment globally (DOM-02): provider, the
// provider account/environment scope when applicable, and the provider's own
// shipment ID. Identical-looking IDs under different accounts are different
// shipments.
type ShipmentRef struct {
	Provider ProviderID
	Account  AccountID
	ID       ShipmentID
}

// IsZero reports whether the reference carries no identity at all.
func (r ShipmentRef) IsZero() bool { return r == ShipmentRef{} }
