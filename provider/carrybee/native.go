package carrybee

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// Location is a city, zone, area or store entry. Only id and name are read;
// Raw always holds the full entry.
type Location struct {
	ID   string
	Name string
	Raw  json.RawMessage
}

func (c *Client) list(ctx context.Context, op, path string, q url.Values) ([]Location, error) {
	resp, _, err := c.call(ctx, callSpec{op: op, method: http.MethodGet, path: path, query: q})
	if err != nil {
		return nil, err
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	raw := resp.Body
	if jsonDecode(op, resp.Body, &env) == nil && len(env.Data) > 0 && string(env.Data) != "null" {
		raw = env.Data
	}
	var items []json.RawMessage
	if t := strings.TrimSpace(string(raw)); strings.HasPrefix(t, "[") {
		if err := jsonDecode(op, raw, &items); err != nil {
			return nil, err
		}
	} else {
		// {"data":{"cities":[...]}} style: take the first list-valued key.
		var obj map[string]json.RawMessage
		if err := jsonDecode(op, raw, &obj); err != nil {
			return nil, err
		}
		for _, v := range obj {
			if strings.HasPrefix(strings.TrimSpace(string(v)), "[") {
				if err := jsonDecode(op, v, &items); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	out := make([]Location, 0, len(items))
	for _, it := range items {
		var l struct {
			ID   json.RawMessage `json:"id"`
			Name string          `json:"name"`
		}
		_ = json.Unmarshal(it, &l)
		out = append(out, Location{ID: scalarString(l.ID), Name: strings.TrimSpace(l.Name), Raw: it})
	}
	return out, nil
}

// ListCities returns the cities (GET /api/v2/cities).
func (c *Client) ListCities(ctx context.Context) ([]Location, error) {
	return c.list(ctx, "locations.cities", "/cities", nil)
}

// ListZones returns the zones of a city.
func (c *Client) ListZones(ctx context.Context, cityID string) ([]Location, error) {
	if !numPattern.MatchString(strings.TrimSpace(cityID)) {
		return nil, validationID("city_id")
	}
	return c.list(ctx, "locations.zones", "/cities/"+strings.TrimSpace(cityID)+"/zones", nil)
}

// ListAreas returns the areas of a zone.
func (c *Client) ListAreas(ctx context.Context, cityID, zoneID string) ([]Location, error) {
	if !numPattern.MatchString(strings.TrimSpace(cityID)) {
		return nil, validationID("city_id")
	}
	if !numPattern.MatchString(strings.TrimSpace(zoneID)) {
		return nil, validationID("zone_id")
	}
	return c.list(ctx, "locations.areas", "/cities/"+strings.TrimSpace(cityID)+"/zones/"+strings.TrimSpace(zoneID)+"/areas", nil)
}

// AreaSuggestion searches areas by free text (GET /api/v2/area-suggestion).
// Use it to find IDs; the SDK never picks one for you.
func (c *Client) AreaSuggestion(ctx context.Context, search string) ([]Location, error) {
	s := strings.TrimSpace(search)
	if s == "" {
		return nil, validationID("search")
	}
	return c.list(ctx, "locations.suggest", "/area-suggestion", url.Values{"search": {s}})
}

// ListStores returns the merchant's stores (GET /api/v2/stores).
func (c *Client) ListStores(ctx context.Context) ([]Location, error) {
	return c.list(ctx, "stores.list", "/stores", nil)
}
