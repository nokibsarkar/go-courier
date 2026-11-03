package courier

import (
	"time"

	"github.com/shopspring/decimal"
)

// CustomerRiskRequest identifies the subject by phone number. Phones are
// personal data (PRIV-01) and one number does not uniquely identify one person
// (PRIV-03).
type CustomerRiskRequest struct {
	Phone string
}

// Completeness describes how complete a risk observation is.
type Completeness string

const (
	CompletenessUnknown  Completeness = "unknown"
	CompletenessComplete Completeness = "complete"
	CompletenessPartial  Completeness = "partial"
	CompletenessSparse   Completeness = "sparse"
)

// Rating is the raw provider rating/score; it is not comparable across
// sources and is never averaged by the SDK (RISK-04).
type Rating struct {
	Value decimal.Decimal
	Scale string
	Label string
}

// DeliveryCounts are observed counts. A nil field means "not provided" and is
// never zero (RISK-02).
type DeliveryCounts struct {
	Total     *int
	Delivered *int
	Cancelled *int
	Returned  *int
}

// ReturnRatio derives returned/total when both are known and total > 0. It is
// an observed delivery ratio, not a probability of fraud (RISK-04).
func (c DeliveryCounts) ReturnRatio() (decimal.Decimal, bool) {
	if c.Total == nil || c.Returned == nil || *c.Total <= 0 {
		return decimal.Decimal{}, false
	}
	return decimal.NewFromInt(int64(*c.Returned)).Div(decimal.NewFromInt(int64(*c.Total))), true
}

// RiskProvenance records where a risk observation came from.
type RiskProvenance struct {
	Origin       string // underlying origin of the data
	Intermediary string // service that relayed it
	ObservedAt   *time.Time
	RetrievedAt  time.Time
	Coverage     string
	Freshness    string
	Completeness Completeness
}

// CustomerRisk is one source's risk observation.
type CustomerRisk struct {
	Source     ProviderID
	Rating     *Rating
	Counts     *DeliveryCounts
	Assessment string // source's own wording; never an invented verdict
	Provenance RiskProvenance
	Raw        []byte
}

// CustomerIncidentReport describes factual events (RISK-05). Ordinary refusal
// or cancellation must not be turned into an accusation by the SDK.
type CustomerIncidentReport struct {
	Phone       string
	Summary     string
	Source      string // reporter's own identification
	Evidence    string // evidentiary basis
	OccurredAt  *time.Time
	Shipment    *ShipmentRef
	MerchantRef MerchantOrderID
}

// IncidentReceipt is one consumer's acknowledgement.
type IncidentReceipt struct {
	ID       IncidentID
	Provider ProviderID
	Status   string
	Raw      []byte
}
