package pathao

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	courier "github.com/nokibsarkar/go-courier"
)

// maxStorePages bounds ListStores pagination.
const maxStorePages = 100

// Store is a Pathao pickup store (provider-native).
type Store struct {
	ID       string
	Name     string
	Address  string
	IsActive *bool // nil when not reported
}

// City is a Pathao city.
type City struct{ ID, Name string }

// Zone is a Pathao zone within a city.
type Zone struct{ ID, Name string }

// Area is a Pathao area within a zone.
type Area struct {
	ID, Name              string
	HomeDeliveryAvailable *bool
	PickupAvailable       *bool
}

// Ref returns the explicit LocationRef to place in courier.Address.Refs.
func (c City) Ref() courier.LocationRef {
	return courier.LocationRef{Provider: ProviderID, Kind: "city", ID: c.ID}
}

// Ref returns the explicit LocationRef to place in courier.Address.Refs.
func (z Zone) Ref() courier.LocationRef {
	return courier.LocationRef{Provider: ProviderID, Kind: "zone", ID: z.ID}
}

// Ref returns the explicit LocationRef to place in courier.Address.Refs.
func (a Area) Ref() courier.LocationRef {
	return courier.LocationRef{Provider: ProviderID, Kind: "area", ID: a.ID}
}

func (c *Client) read(ctx context.Context, op, path string, q url.Values, out any) error {
	resp, _, err := c.call(ctx, callSpec{op: op, method: http.MethodGet, path: path, query: q})
	if err != nil {
		return err
	}
	return decode(op, resp.Body, false, out)
}

// ListStores returns every store, following pagination (bounded).
func (c *Client) ListStores(ctx context.Context) ([]Store, error) {
	var out []Store
	for page := 1; page <= maxStorePages; page++ {
		var q url.Values
		if page > 1 {
			q = url.Values{"page": {strconv.Itoa(page)}}
		}
		var p storePage
		if err := c.read(ctx, "stores.list", "/stores", q, &p); err != nil {
			return nil, err
		}
		for _, s := range p.Data {
			out = append(out, Store{ID: s.StoreID.String(), Name: s.StoreName, Address: s.StoreAddress, IsActive: parseBool(s.IsActive)})
		}
		last, err := p.LastPage.Int64()
		if err != nil || int64(page) >= last {
			return out, nil
		}
	}
	return out, &courier.Error{Kind: courier.KindResourceLimit, Provider: ProviderID, Op: "stores.list", Message: "too many store pages"}
}

// ListCities returns all cities.
func (c *Client) ListCities(ctx context.Context) ([]City, error) {
	var d listData[rawCity]
	if err := c.read(ctx, "cities.list", "/city-list", nil, &d); err != nil {
		return nil, err
	}
	out := make([]City, 0, len(d.Data))
	for _, r := range d.Data {
		out = append(out, City{ID: r.ID.String(), Name: r.Name})
	}
	return out, nil
}

func numericID(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if !digitsPattern.MatchString(v) {
		return "", courier.Validation(field, "must be a numeric pathao ID")
	}
	return v, nil
}

// ListZones returns the zones of a city.
func (c *Client) ListZones(ctx context.Context, cityID string) ([]Zone, error) {
	id, err := numericID("city_id", cityID)
	if err != nil {
		return nil, err
	}
	var d listData[rawZone]
	if err := c.read(ctx, "zones.list", "/cities/"+id+"/zone-list", nil, &d); err != nil {
		return nil, err
	}
	out := make([]Zone, 0, len(d.Data))
	for _, r := range d.Data {
		out = append(out, Zone{ID: r.ID.String(), Name: r.Name})
	}
	return out, nil
}

// ListAreas returns the areas of a zone.
func (c *Client) ListAreas(ctx context.Context, zoneID string) ([]Area, error) {
	id, err := numericID("zone_id", zoneID)
	if err != nil {
		return nil, err
	}
	var d listData[rawArea]
	if err := c.read(ctx, "areas.list", "/zones/"+id+"/area-list", nil, &d); err != nil {
		return nil, err
	}
	out := make([]Area, 0, len(d.Data))
	for _, r := range d.Data {
		out = append(out, Area{ID: r.ID.String(), Name: r.Name, HomeDeliveryAvailable: parseBool(r.HomeDeliveryAvailable), PickupAvailable: parseBool(r.PickupAvailable)})
	}
	return out, nil
}
