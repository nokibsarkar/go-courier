package redx

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	courier "github.com/nokibsarkar/go-courier"
)

// Area is a RedX delivery area.
type Area struct {
	ID           int64
	Name         string
	PostCode     int64
	DivisionName string
	ZoneID       int64
}

// AreaFilter narrows ListAreas. Set at most one of PostCode and DistrictName.
type AreaFilter struct {
	PostCode     int64
	DistrictName string
}

// ListAreas lists delivery areas (GET /areas, optionally ?post_code= or
// ?district_name=). Use the IDs as LocationRef{Provider:"redx", Kind:"area"}.
func (c *Client) ListAreas(ctx context.Context, f AreaFilter) ([]Area, error) {
	const op = "areas.list"
	q := url.Values{}
	switch {
	case f.PostCode != 0 && strings.TrimSpace(f.DistrictName) != "":
		return nil, courier.Validation("filter", "set either PostCode or DistrictName, not both")
	case f.PostCode < 0:
		return nil, courier.Validation("post_code", "must be positive")
	case f.PostCode > 0:
		q.Set("post_code", strconv.FormatInt(f.PostCode, 10))
	case strings.TrimSpace(f.DistrictName) != "":
		q.Set("district_name", strings.TrimSpace(f.DistrictName))
	}
	resp, _, err := c.call(ctx, callSpec{op: op, method: http.MethodGet, path: "/areas", query: q})
	if err != nil {
		return nil, err
	}
	var out struct {
		Areas []struct {
			ID           int64  `json:"id"`
			Name         string `json:"name"`
			PostCode     int64  `json:"post_code"`
			DivisionName string `json:"division_name"`
			ZoneID       int64  `json:"zone_id"`
		} `json:"areas"`
	}
	if err := jsonDecode(op, resp.Body, &out); err != nil {
		return nil, err
	}
	areas := make([]Area, 0, len(out.Areas))
	for _, a := range out.Areas {
		if a.ID <= 0 || strings.TrimSpace(a.Name) == "" {
			return nil, protocol(op, "area entry lacks id or name")
		}
		areas = append(areas, Area{ID: a.ID, Name: a.Name, PostCode: a.PostCode, DivisionName: a.DivisionName, ZoneID: a.ZoneID})
	}
	return areas, nil
}

// PickupStore is a RedX pickup store. CreatedAt is nil when absent.
type PickupStore struct {
	ID       int64
	Name     string
	Address  string
	AreaName string
	AreaID   int64
	Phone    string
}

// PickupStoreInput creates a pickup store.
type PickupStoreInput struct {
	Name, Phone, Address string
	AreaID               int64
}

type storeWire struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	AreaName string `json:"area_name"`
	AreaID   int64  `json:"area_id"`
	Phone    string `json:"phone"`
}

func (w storeWire) store(op string) (*PickupStore, error) {
	if w.ID <= 0 {
		return nil, protocol(op, "pickup store lacks an id")
	}
	return &PickupStore{ID: w.ID, Name: w.Name, Address: w.Address, AreaName: w.AreaName, AreaID: w.AreaID, Phone: w.Phone}, nil
}

// CreatePickupStore creates a pickup store (POST /pickup/store). RedX documents
// no duplicate protection, so an indeterminate result must be reconciled with
// ListPickupStores before retrying.
func (c *Client) CreatePickupStore(ctx context.Context, in PickupStoreInput) (*PickupStore, error) {
	const op = "pickup_store.create"
	name, err := limited("name", in.Name, 250)
	if err != nil {
		return nil, err
	}
	addr, err := limited("address", in.Address, 500)
	if err != nil {
		return nil, err
	}
	phone, err := localMobile("phone", in.Phone)
	if err != nil {
		return nil, err
	}
	if in.AreaID <= 0 {
		return nil, courier.Validation("area_id", "required")
	}
	body := map[string]any{"name": name, "phone": phone, "address": addr, "area_id": in.AreaID}
	resp, _, err := c.call(ctx, callSpec{op: op, method: http.MethodPost, path: "/pickup/store", payload: body, mutation: true})
	if err != nil {
		return nil, err
	}
	var w storeWire
	if err := jsonDecode(op, resp.Body, &w); err != nil {
		return nil, asIndeterminate(err)
	}
	s, err := w.store(op)
	if err != nil {
		return nil, asIndeterminate(err)
	}
	return s, nil
}

// ListPickupStores lists the merchant's pickup stores (GET /pickup/stores).
func (c *Client) ListPickupStores(ctx context.Context) ([]PickupStore, error) {
	const op = "pickup_store.list"
	resp, _, err := c.call(ctx, callSpec{op: op, method: http.MethodGet, path: "/pickup/stores"})
	if err != nil {
		return nil, err
	}
	var out struct {
		Stores []storeWire `json:"pickup_stores"`
	}
	if err := jsonDecode(op, resp.Body, &out); err != nil {
		return nil, err
	}
	stores := make([]PickupStore, 0, len(out.Stores))
	for _, w := range out.Stores {
		s, err := w.store(op)
		if err != nil {
			return nil, err
		}
		stores = append(stores, *s)
	}
	return stores, nil
}

// GetPickupStore returns one pickup store (GET /pickup/store/info/{id}).
func (c *Client) GetPickupStore(ctx context.Context, id int64) (*PickupStore, error) {
	const op = "pickup_store.get"
	if id <= 0 {
		return nil, courier.Validation("id", "must be positive")
	}
	resp, _, err := c.call(ctx, callSpec{op: op, method: http.MethodGet, path: "/pickup/store/info/" + strconv.FormatInt(id, 10)})
	if err != nil {
		return nil, err
	}
	var out struct {
		Store *storeWire `json:"pickup_store"`
	}
	if err := jsonDecode(op, resp.Body, &out); err != nil {
		return nil, err
	}
	if out.Store == nil {
		return nil, protocol(op, "response lacks pickup_store")
	}
	return out.Store.store(op)
}

// ChargeQuery asks for the charge of a prospective parcel.
type ChargeQuery struct {
	DeliveryAreaID int64
	PickupAreaID   int64
	// CashCollection is the COD amount; nil means 0.
	CashCollection *courier.Money
	WeightGrams    uint32
}

// Charge is the provider's charge breakdown. Both amounts are exact BDT and
// nil when absent. It is not a bookable quote: RedX issues no quote ID.
type Charge struct {
	Delivery *courier.Money
	COD      *courier.Money
	Raw      []byte
}

// CalculateCharge calls GET /charge/charge_calculator.
func (c *Client) CalculateCharge(ctx context.Context, q ChargeQuery) (*Charge, error) {
	const op = "charge.calculate"
	if q.DeliveryAreaID <= 0 || q.PickupAreaID <= 0 || q.WeightGrams == 0 {
		return nil, courier.Validation("query", "delivery area, pickup area and weight are required")
	}
	cash := "0"
	if q.CashCollection != nil {
		var err error
		if cash, err = wholeTaka("cash_collection", *q.CashCollection); err != nil {
			return nil, err
		}
	}
	v := url.Values{}
	v.Set("delivery_area_id", strconv.FormatInt(q.DeliveryAreaID, 10))
	v.Set("pickup_area_id", strconv.FormatInt(q.PickupAreaID, 10))
	v.Set("cash_collection_amount", cash)
	v.Set("weight", strconv.FormatUint(uint64(q.WeightGrams), 10))
	resp, _, err := c.call(ctx, callSpec{op: op, method: http.MethodGet, path: "/charge/charge_calculator", query: v})
	if err != nil {
		return nil, err
	}
	var out struct {
		Delivery rawNumber `json:"deliveryCharge"`
		COD      rawNumber `json:"codCharge"`
	}
	if err := jsonDecode(op, resp.Body, &out); err != nil {
		return nil, err
	}
	ch := &Charge{Raw: resp.Body}
	if ch.Delivery, err = out.Delivery.money(op, "deliveryCharge"); err != nil {
		return nil, err
	}
	if ch.COD, err = out.COD.money(op, "codCharge"); err != nil {
		return nil, err
	}
	if ch.Delivery == nil && ch.COD == nil {
		return nil, protocol(op, "response carries neither deliveryCharge nor codCharge")
	}
	return ch, nil
}
