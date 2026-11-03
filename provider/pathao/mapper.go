package pathao

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	courier "github.com/nokibsarkar/go-courier"
)

var (
	digitsPattern = regexp.MustCompile(`^[0-9]{1,18}$`)
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Pathao enumerations.
const (
	deliveryNormal = 48
	itemParcel     = 2
	minWeightGrams = 500
	maxWeightGrams = 10000
)

func localPhone(field, in string) (string, error) {
	n, err := courier.NormalizePhone(in)
	if err != nil {
		var ce *courier.Error
		if errors.As(err, &ce) {
			ce.Field = field
		}
		return "", err
	}
	if !strings.HasPrefix(n, "+880") || len(n) != 14 {
		return "", courier.Validation(field, "pathao supports Bangladeshi mobile numbers only")
	}
	return "0" + n[4:], nil
}

// locationRef returns the explicit numeric Pathao reference of kind on addr.
// There is never a text-based or first-match fallback (DOM-05).
func locationRef(field string, a courier.Address, kind string) (json.Number, error) {
	var found string
	for _, r := range a.Refs {
		if r.Provider != ProviderID || !strings.EqualFold(r.Kind, kind) {
			continue
		}
		id := strings.TrimSpace(r.ID)
		if !digitsPattern.MatchString(id) {
			return "", courier.Validation(field, "pathao location IDs must be numeric")
		}
		if found != "" && found != id {
			return "", &courier.Error{Kind: courier.KindAmbiguous, Provider: ProviderID, Field: field,
				Message: "multiple conflicting pathao " + kind + " references"}
		}
		found = id
	}
	if found == "" {
		return "", &courier.Error{Kind: courier.KindAmbiguous, Provider: ProviderID, Field: field,
			Message: "location mapping must be explicit: add a LocationRef{Provider: \"pathao\", Kind: \"" + kind +
				"\"} (see ListCities, ListZones, ListAreas); free-text addresses are never matched"}
	}
	return json.Number(found), nil
}

func storeID(party courier.Party, creds Credentials) (json.Number, error) {
	s := strings.TrimSpace(string(party.Store))
	if s == "" {
		s = strings.TrimSpace(string(creds.DefaultStoreID))
	}
	if s == "" {
		return "", courier.Validation("pickup.store", "a store is required (request or default store)")
	}
	if !digitsPattern.MatchString(s) {
		return "", courier.Validation("pickup.store", "pathao store IDs must be numeric")
	}
	return json.Number(s), nil
}

func weightKg(g *uint32) (json.Number, error) {
	if g == nil {
		return "", courier.Validation("package.weight", "weight is required (0.5 to 10 kg)")
	}
	if *g < minWeightGrams || *g > maxWeightGrams {
		return "", courier.Validation("package.weight", "weight must be between 500 and 10000 grams")
	}
	return json.Number(decimal.NewFromInt(int64(*g)).Shift(-3).String()), nil
}

func bdtAmount(field string, m *courier.Money) (json.Number, error) {
	if m == nil {
		return "0", nil
	}
	if err := m.Validate(field, false); err != nil {
		return "", err
	}
	if m.Code() != courier.BDT {
		return "", courier.Validation(field+".currency", "pathao operates in BDT only")
	}
	return json.Number(m.Amount.String()), nil
}

func joinAddress(a courier.Address) string {
	var parts []string
	for _, p := range []string{a.Line1, a.Line2, a.Area, a.City, a.District, a.PostalCode} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

func buildOrder(r courier.CreateShipmentRequest, creds Credentials) (orderRequest, error) {
	var o orderRequest
	var err error
	if r.QuoteID != "" {
		return o, courier.Validation("quote_id", "pathao orders cannot be bound to a quote")
	}
	if r.DeliveryCharge != nil || r.Package.Insurance != nil {
		return o, courier.Validation("delivery_charge", "delivery charge and insurance are not settable on pathao orders")
	}
	if o.StoreID, err = storeID(r.Pickup, creds); err != nil {
		return o, err
	}
	name := strings.TrimSpace(r.Recipient.Contact.Name)
	if n := utf8.RuneCountInString(name); n < 3 || n > 100 {
		return o, courier.Validation("recipient.name", "required, 3 to 100 characters")
	}
	if o.RecipientPhone, err = localPhone("recipient.phone", r.Recipient.Contact.Phone); err != nil {
		return o, err
	}
	if p := r.Recipient.Contact.AlternativePhone; p != "" {
		if o.RecipientSecondaryPhone, err = localPhone("recipient.alternative_phone", p); err != nil {
			return o, err
		}
	}
	addr := strings.TrimSpace(r.Recipient.Address.Text)
	if addr == "" {
		addr = joinAddress(r.Recipient.Address)
	}
	if n := utf8.RuneCountInString(addr); n < 10 || n > 220 {
		return o, courier.Validation("recipient.address", "required, 10 to 220 characters")
	}
	if o.RecipientCity, err = locationRef("recipient.city", r.Recipient.Address, "city"); err != nil {
		return o, err
	}
	if o.RecipientZone, err = locationRef("recipient.zone", r.Recipient.Address, "zone"); err != nil {
		return o, err
	}
	if o.RecipientArea, err = locationRef("recipient.area", r.Recipient.Address, "area"); err != nil {
		return o, err
	}
	switch r.Delivery {
	case "", courier.DeliveryHome:
		o.DeliveryType = deliveryNormal
	default:
		return o, courier.Validation("delivery", "pathao supports home (normal) delivery only")
	}
	o.ItemType = itemParcel
	if o.ItemWeight, err = weightKg(r.Package.WeightGrams); err != nil {
		return o, err
	}
	switch {
	case r.Package.Quantity < 0:
		return o, courier.Validation("package.quantity", "must not be negative")
	case r.Package.Quantity == 0:
		o.ItemQuantity = 1
	default:
		o.ItemQuantity = r.Package.Quantity
	}
	if o.AmountToCollect, err = bdtAmount("cod", r.COD); err != nil {
		return o, err
	}
	o.MerchantOrderID = strings.TrimSpace(string(r.MerchantOrderID))
	o.SpecialInstruction = strings.TrimSpace(r.Note)
	if o.SpecialInstruction == "" {
		o.SpecialInstruction = strings.TrimSpace(r.Recipient.Instructions)
	}
	o.ItemDescription = strings.TrimSpace(r.Package.Description)
	return o, nil
}

func buildPrice(r courier.QuoteRequest, creds Credentials) (priceRequest, error) {
	var p priceRequest
	var err error
	if r.ScheduleAt != nil {
		return p, courier.Validation("schedule_at", "scheduled quotes are not supported")
	}
	if r.COD != nil { // validated, but price-plan takes no COD field
		if _, err = bdtAmount("cod", r.COD); err != nil {
			return p, err
		}
	}
	if p.StoreID, err = storeID(r.Pickup, creds); err != nil {
		return p, err
	}
	if p.RecipientCity, err = locationRef("recipient.city", r.Recipient.Address, "city"); err != nil {
		return p, err
	}
	if p.RecipientZone, err = locationRef("recipient.zone", r.Recipient.Address, "zone"); err != nil {
		return p, err
	}
	if p.ItemWeight, err = weightKg(r.Package.WeightGrams); err != nil {
		return p, err
	}
	p.ItemType, p.DeliveryType = itemParcel, deliveryNormal
	return p, nil
}

// consignmentPath validates an ID that is placed into a URL path.
func consignmentPath(id string) (string, error) {
	id = strings.TrimSpace(id)
	if !idPattern.MatchString(id) {
		return "", courier.Validation("code", "invalid consignment ID")
	}
	return url.PathEscape(id), nil
}

func normStatus(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer(" ", "_", "-", "_").Replace(s)
}

// mapStatusValue maps only statuses that are unambiguous.
// Ambiguous or unseen values (Return, Partial_Delivery, Pickup_Failed,
// Paid_Return, Exchange, new statuses) stay StatusUnknown.
func mapStatusValue(s string) courier.Status {
	switch normStatus(s) {
	case "pending":
		return courier.StatusCreated
	case "pickup_requested", "assigned_for_pickup":
		return courier.StatusPickupPending
	case "pickup", "picked", "picked_up":
		return courier.StatusPickedUp
	case "in_transit", "at_the_sorting_hub", "received_at_last_mile_hub":
		return courier.StatusInTransit
	case "assigned_for_delivery":
		return courier.StatusOutForDelivery
	case "delivered":
		return courier.StatusDelivered
	case "delivery_failed":
		return courier.StatusDeliveryFailed
	case "on_hold":
		return courier.StatusOnHold
	case "cancel", "cancelled", "canceled":
		return courier.StatusCancelled
	case "returned":
		return courier.StatusReturned
	default:
		return courier.StatusUnknown
	}
}
