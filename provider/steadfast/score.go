package steadfast

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nokibsarkar/go-courier/httpx"
)

// CustomerScore is a customer's delivery record from GET
// /fraud_check/score/{phone}, built from every merchant's history with that
// number. It is a signal, not a verdict.
//
// A nil value means "not known" or "not shown", never zero: a customer nothing
// has finished for has nil ratios, which is not the same as a clean record, and
// the exact counts are shown only while the account is booking regularly. Both
// ratios are whole percents of finished parcels, each rounded down on its own,
// so they can add up to 99.
//
// The provider has switched its numeric score off: its score, level and reasons
// are always null and are deliberately not exposed.
type CustomerScore struct {
	Phone             string
	DeliveryRatio     *int64 // percent of finished parcels delivered (partly delivered counts as delivered)
	CancellationRatio *int64 // percent of finished parcels cancelled
	// VolumeBand is none, low (1-5), medium (6-20), high (21-200) or very_high (200+).
	VolumeBand string
	// VolumeRange is the finished-parcel count as shown to merchants: exact below
	// ten, then "10+", "25+", "50+" or "100+"; nil when nothing has finished.
	VolumeRange    *string
	DeliveredCount *int64
	CancelledCount *int64
	TotalReports   *int64
	// FraudCategories maps the reported category code to times reported. It can
	// add up to less than TotalReports (older reports carry no category).
	FraudCategories map[string]int64
	DoubtfulReports *bool
	Raw             json.RawMessage
}

// GetCustomerScore looks up the delivery record of a Bangladeshi mobile number.
// The phone is validated and normalised locally before any request. The
// provider rate-limits this per merchant against your own parcel volume (429
// when exceeded); the limit is shared with the older counts endpoint.
func (c *Client) GetCustomerScore(ctx context.Context, phone string) (*CustomerScore, error) {
	const op = "customer.score"
	p, err := localPhone("phone", phone)
	if err != nil {
		return nil, err
	}
	resp, _, err := c.call(ctx, op, http.MethodGet, "/fraud_check/score/"+url.PathEscape(p), nil, false)
	if err != nil {
		return nil, err
	}
	var env struct {
		envelope
		Phone           json.RawMessage `json:"phone"`
		DeliveryRatio   json.RawMessage `json:"delivery_ratio"`
		CancelRatio     json.RawMessage `json:"cancellation_ratio"`
		VolumeBand      string          `json:"volume_band"`
		VolumeRange     json.RawMessage `json:"volume_range"`
		DeliveredCount  json.RawMessage `json:"delivered_count"`
		CancelledCount  json.RawMessage `json:"cancelled_count"`
		TotalReports    json.RawMessage `json:"total_reports"`
		FraudCategories json.RawMessage `json:"fraud_categories"`
		Doubtful        *bool           `json:"doubtful_reports"`
	}
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &env); err != nil {
		return nil, err
	}
	if err := checkEmbedded(op, env.envelope); err != nil {
		return nil, err
	}
	s := &CustomerScore{
		Phone:             scalarText(env.Phone),
		DeliveryRatio:     wholeCount(env.DeliveryRatio),
		CancellationRatio: wholeCount(env.CancelRatio),
		VolumeBand:        strings.TrimSpace(env.VolumeBand),
		DeliveredCount:    wholeCount(env.DeliveredCount),
		CancelledCount:    wholeCount(env.CancelledCount),
		TotalReports:      wholeCount(env.TotalReports),
		DoubtfulReports:   env.Doubtful,
		Raw:               append(json.RawMessage(nil), resp.Body...),
	}
	if v := scalarText(env.VolumeRange); v != "" {
		s.VolumeRange = &v
	}
	// An empty set arrives as [] (not {}), so only an object is read.
	var cats map[string]json.RawMessage
	if json.Unmarshal(env.FraudCategories, &cats) == nil && len(cats) > 0 {
		s.FraudCategories = make(map[string]int64, len(cats))
		for k, v := range cats {
			if n := wholeCount(v); n != nil {
				s.FraudCategories[k] = *n
			}
		}
	}
	return s, nil
}

// wholeCount returns a non-negative whole number, or nil when the value is
// null, absent or not a plain integer.
func wholeCount(raw json.RawMessage) *int64 {
	v := scalarText(raw)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	return &n
}
