package ecourier

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/shopspring/decimal"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/httpx"
)

type decimalT = decimal.Decimal

func decimalFromString(s string) (decimal.Decimal, error) { return decimal.NewFromString(s) }

func jsonDecode(op string, raw []byte, out any) error {
	return httpx.DecodeJSON(ProviderID, op, bytes.TrimSpace(raw), out)
}

// Package is an assigned eCourier package (section 2.3).
type Package struct {
	Name           string
	Code           string
	ShippingCharge *courier.Money // nil when absent
	Weight         string
	DeliveryTime   string
	CoverageID     string
	Coverage       string
}

// Option is a name/value pair used by city, thana lists.
type NameValue struct{ Name, Value string }

// Area is an eCourier area (section 2.19).
type Area struct {
	ID       string
	HubID    string
	PostCode string
	Name     string
}

// Hub is hub information for an area (section 2.20). Latitude/Longitude are
// strings exactly as returned.
type Hub struct {
	Name, ID, Mobile string
	Address          *string
	Latitude         string
	Longitude        string
}

// PaymentStatus is the settlement of a parcel (section 2.18); amounts are exact
// and nil when absent.
type PaymentStatus struct {
	PaymentID       string
	PaymentMode     string
	CollectedAmount *courier.Money
	PaymentDate     string // as returned (time zone not documented)
	ShippingCharge  *courier.Money
	BookingPrice    *courier.Money
	FragilePrice    *courier.Money
	CashOnDelivery  *courier.Money
	Raw             json.RawMessage
}

func (c *Client) list(ctx context.Context, op, path string, payload any) (envelope, json.RawMessage, error) {
	resp, _, err := c.call(ctx, op, path, payload, false)
	if err != nil {
		return envelope{}, nil, err
	}
	trimmed := bytes.TrimSpace(resp.Body)
	if len(trimmed) > 0 && trimmed[0] == '[' { // bare array responses
		return envelope{}, json.RawMessage(trimmed), nil
	}
	env, err := decode(op, resp.Body)
	return env, resp.Body, err
}

func numString(v any) string {
	switch t := v.(type) {
	case json.Number:
		return t.String()
	case string:
		return strings.TrimSpace(t)
	}
	return ""
}

// ListPackages returns the packages assigned to the account (POST /packages).
func (c *Client) ListPackages(ctx context.Context) ([]Package, error) {
	const op = "packages.list"
	_, raw, err := c.list(ctx, op, "/packages", nil)
	if err != nil {
		return nil, err
	}
	var items []struct {
		Name     string          `json:"package_name"`
		Code     string          `json:"package_code"`
		Charge   json.RawMessage `json:"shipping_charge"`
		Weight   string          `json:"weight"`
		Delivery string          `json:"delivery_time"`
		CovID    any             `json:"coverage_id"`
		Coverage string          `json:"coverage"`
	}
	if err := jsonDecode(op, raw, &items); err != nil {
		return nil, err
	}
	out := make([]Package, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it.Code) == "" {
			return out, protocol(op, "package lacks a package_code")
		}
		out = append(out, Package{Name: it.Name, Code: it.Code, ShippingCharge: moneyOf(it.Charge), Weight: it.Weight,
			DeliveryTime: it.Delivery, CoverageID: numString(it.CovID), Coverage: it.Coverage})
	}
	return out, nil
}

func nameValues(op string, raw json.RawMessage) ([]NameValue, error) {
	var items []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := jsonDecode(op, raw, &items); err != nil {
		return nil, err
	}
	out := make([]NameValue, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it.Value) == "" {
			return out, protocol(op, "record lacks a value")
		}
		out = append(out, NameValue{Name: it.Name, Value: it.Value})
	}
	return out, nil
}

// ListCities returns the delivery cities (POST /city-list).
func (c *Client) ListCities(ctx context.Context) ([]NameValue, error) {
	const op = "locations.cities"
	_, raw, err := c.list(ctx, op, "/city-list", nil)
	if err != nil {
		return nil, err
	}
	return nameValues(op, raw)
}

// ListThanas returns the thanas of a city (POST /thana-list).
func (c *Client) ListThanas(ctx context.Context, city string) ([]NameValue, error) {
	const op = "locations.thanas"
	city = strings.TrimSpace(city)
	if city == "" {
		return nil, courier.Validation("city", "required")
	}
	env, _, err := c.list(ctx, op, "/thana-list", map[string]string{"city": city})
	if err != nil {
		return nil, err
	}
	return nameValues(op, env.Message)
}

// ListAreasByDistrict returns areas of a district (POST /area-by-district).
func (c *Client) ListAreasByDistrict(ctx context.Context, district string) ([]Area, error) {
	const op = "locations.areas"
	district = strings.TrimSpace(district)
	if district == "" {
		return nil, courier.Validation("district", "required")
	}
	env, _, err := c.list(ctx, op, "/area-by-district", map[string]string{"district": district})
	if err != nil {
		return nil, err
	}
	var items []struct {
		ID       any    `json:"id"`
		HubID    any    `json:"hub_id"`
		PostCode string `json:"post_code"`
		Name     string `json:"name"`
	}
	if err := jsonDecode(op, env.Data, &items); err != nil {
		return nil, err
	}
	out := make([]Area, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it.Name) == "" {
			return out, protocol(op, "area lacks a name")
		}
		out = append(out, Area{ID: numString(it.ID), HubID: numString(it.HubID), PostCode: it.PostCode, Name: it.Name})
	}
	return out, nil
}

// HubByArea returns hub details for an area name (POST /hub-info-by-area).
func (c *Client) HubByArea(ctx context.Context, area string) (*Hub, error) {
	const op = "locations.hub"
	area = strings.TrimSpace(area)
	if area == "" {
		return nil, courier.Validation("area", "required")
	}
	env, _, err := c.list(ctx, op, "/hub-info-by-area", map[string]string{"area": area})
	if err != nil {
		return nil, err
	}
	var d struct {
		Name      string  `json:"hub_name"`
		ID        any     `json:"hub_id"`
		Mobile    string  `json:"hub_mobile"`
		Address   *string `json:"address"`
		Latitude  string  `json:"latitude"`
		Longitude string  `json:"longitude"`
	}
	if err := jsonDecode(op, env.Data, &d); err != nil {
		return nil, err
	}
	if numString(d.ID) == "" {
		return nil, protocol(op, "hub lacks an id")
	}
	return &Hub{Name: d.Name, ID: numString(d.ID), Mobile: d.Mobile, Address: d.Address, Latitude: d.Latitude, Longitude: d.Longitude}, nil
}

// GetPaymentStatus returns the settlement of a parcel (POST /payment-status).
func (c *Client) GetPaymentStatus(ctx context.Context, ecr string) (*PaymentStatus, error) {
	const op = "payment.status"
	ecr = strings.TrimSpace(ecr)
	if !ecrPattern.MatchString(ecr) {
		return nil, courier.Validation("ecr", "invalid eCourier ID")
	}
	env, _, err := c.list(ctx, op, "/payment-status", map[string]string{"tracking": ecr})
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := jsonDecode(op, env.Message, &m); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, protocol(op, "empty payment record")
	}
	s := func(k string) string {
		var v any
		_ = json.Unmarshal(m[k], &v)
		switch t := v.(type) {
		case string:
			return t
		case float64:
			return decimal.NewFromFloat(t).String()
		}
		return ""
	}
	return &PaymentStatus{PaymentID: s("payment_id"), PaymentMode: s("payment_mode"), PaymentDate: s("payment_date"),
		CollectedAmount: moneyOf(m["collected_amount"]), ShippingCharge: moneyOf(m["shipping_charge"]),
		BookingPrice: moneyOf(m["booking_price"]), FragilePrice: moneyOf(m["fragile_price"]),
		CashOnDelivery: moneyOf(m["cash_on_delivery"]), Raw: env.Message}, nil
}
