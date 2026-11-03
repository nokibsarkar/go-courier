package uber

import (
	"math"

	"encoding/json"
	"github.com/shopspring/decimal"

	courier "github.com/nokibsarkar/go-courier"
)

// ShoppingProgress is a decoded event.shopping_progress webhook (Courier Pick
// & Pack). Item prices are
// integer minor units: the event carries no currency, so no Money is built.
type ShoppingProgress struct {
	EventID    string
	DeliveryID string
	RouteID    string
	// ItemStatus is the raw value: fulfilled, removed, replaced or suggested.
	ItemStatus string
	Item       ShoppingItem
	// Replacement is nil when the event carries no replacement item.
	Replacement *ShoppingItem
}

// ShoppingItem is a manifest or replacement item. Absent numbers are nil.
type ShoppingItem struct {
	Name              string
	SKU               string
	Quantity          *int64
	FulfilledQuantity *int64
	PriceMinor        *int64
	WeightGrams       *int64
	FulfilledWeight   *int64
}

type shoppingItemWire struct {
	Name              string            `json:"name"`
	SKU               string            `json:"sku"`
	Quantity          json.RawMessage   `json:"quantity"`
	FulfilledQuantity json.RawMessage   `json:"fulfilled_quantity"`
	Price             json.RawMessage   `json:"price"`
	Weight            json.RawMessage   `json:"weight"`
	FulfilledWeight   json.RawMessage   `json:"fulfilled_weight"`
	Replacement       *shoppingItemWire `json:"replacement_item"`
}

func wholeNumber(op string, raw json.RawMessage) (*int64, error) {
	d, ok, err := courier.ParseAmount(raw)
	if err != nil {
		return nil, protocol(op, "invalid number")
	}
	if !ok {
		return nil, nil
	}
	if !d.IsInteger() || d.IsNegative() || d.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return nil, protocol(op, "expected a non-negative whole number")
	}
	v := d.IntPart()
	return &v, nil
}

func (w shoppingItemWire) convert(op string) (ShoppingItem, error) {
	it := ShoppingItem{Name: w.Name, SKU: w.SKU}
	var err error
	for _, f := range []struct {
		dst *(*int64)
		raw json.RawMessage
	}{{&it.Quantity, w.Quantity}, {&it.FulfilledQuantity, w.FulfilledQuantity}, {&it.PriceMinor, w.Price},
		{&it.WeightGrams, w.Weight}, {&it.FulfilledWeight, w.FulfilledWeight}} {
		if *f.dst, err = wholeNumber(op, f.raw); err != nil {
			return ShoppingItem{}, err
		}
	}
	return it, nil
}

// ParseShoppingProgress decodes a shopping-progress Event from DecodeWebhook.
// Check Trust before acting on it.
func ParseShoppingProgress(e courier.Event) (*ShoppingProgress, error) {
	if e.Provider != ProviderID || e.Type != evShopping {
		return nil, courier.Validation("event", "not an Uber event.shopping_progress event")
	}
	var h struct {
		ID         string `json:"id"`
		DeliveryID string `json:"delivery_id"`
		RouteID    string `json:"route_id"`
		ItemStatus string `json:"item_status"`
		Data       struct {
			Manifest    *shoppingItemWire `json:"manifest_item"`
			Replacement *shoppingItemWire `json:"replacement_item"`
		} `json:"data"`
	}
	const op = "webhook.shopping_progress"
	if err := json.Unmarshal(e.Payload, &h); err != nil || h.Data.Manifest == nil {
		return nil, protocol(op, "malformed shopping progress event")
	}
	item, err := h.Data.Manifest.convert(op)
	if err != nil {
		return nil, err
	}
	out := &ShoppingProgress{EventID: h.ID, DeliveryID: h.DeliveryID, RouteID: h.RouteID, ItemStatus: h.ItemStatus, Item: item}
	// replacement_item appears under data or under manifest_item. Accept either.
	rep := h.Data.Replacement
	if rep == nil {
		rep = h.Data.Manifest.Replacement
	}
	if rep != nil {
		r, err := rep.convert(op)
		if err != nil {
			return nil, err
		}
		out.Replacement = &r
	}
	return out, nil
}

// Attribute names carried in direct.organization changes.
const (
	AttrBillingPolicy   = "b2b_billing_info.policies_metadata"
	AttrTaxFormComplete = "organization.direct_organization.base_info.is_tax_form_completed.value"
)

// BillingPolicyUpdate is a decoded direct.organization webhook (billing
// policy). It is organization-scoped: there is no
// delivery. BillingStatus is the raw "active"/"inactive"; TaxFormCompleted is
// nil when absent. Changes keep the provider's raw attribute names and values.
type BillingPolicyUpdate struct {
	EventID          string
	SchemaVersion    string
	OrganizationID   string
	OrganizationName string
	RootOrgID        string
	Tenancy          string
	BillingStatus    string
	TaxFormCompleted *bool
	Changes          []OrganizationChange
}

// OrganizationChange is one data.changes entry.
type OrganizationChange struct {
	Attribute, Operation, Previous, Current string
}

// ParseBillingPolicyUpdate decodes a direct.organization Event. Check Trust
// before acting on it.
func ParseBillingPolicyUpdate(e courier.Event) (*BillingPolicyUpdate, error) {
	if e.Provider != ProviderID || e.Type != evOrganization {
		return nil, courier.Validation("event", "not an Uber direct.organization event")
	}
	var h struct {
		ID            string `json:"id"`
		SchemaVersion string `json:"schema_version"`
		Data          struct {
			OrgUUID string `json:"organization_uuid"`
			OrgName string `json:"organization_name"`
			RootOrg string `json:"root_org_uuid"`
			Tenancy string `json:"tenancy"`
			Billing string `json:"billing_status"`
			TaxForm *bool  `json:"is_tax_form_completed"`
			Changes []struct {
				Attr string `json:"attribute_name"`
				Op   string `json:"operation"`
				Prev string `json:"previous_value"`
				Cur  string `json:"current_value"`
			} `json:"changes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(e.Payload, &h); err != nil {
		return nil, protocol("webhook.organization", "malformed organization event")
	}
	d := h.Data
	out := &BillingPolicyUpdate{EventID: h.ID, SchemaVersion: h.SchemaVersion, OrganizationID: d.OrgUUID, OrganizationName: d.OrgName,
		RootOrgID: d.RootOrg, Tenancy: d.Tenancy, BillingStatus: d.Billing, TaxFormCompleted: d.TaxForm}
	for _, c := range d.Changes {
		out.Changes = append(out.Changes, OrganizationChange{Attribute: c.Attr, Operation: c.Op, Previous: c.Prev, Current: c.Cur})
	}
	return out, nil
}
