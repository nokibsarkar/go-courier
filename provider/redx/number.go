package redx

import (
	"encoding/json"

	courier "github.com/nokibsarkar/go-courier"
)

// rawNumber keeps a JSON number (or numeric string) exact until converted.
type rawNumber json.RawMessage

func (n *rawNumber) UnmarshalJSON(b []byte) error { *n = append((*n)[:0], b...); return nil }

// money converts to exact BDT; nil when the field is absent.
func (n rawNumber) money(op, field string) (*courier.Money, error) {
	d, ok, err := courier.ParseAmount(json.RawMessage(n))
	if err != nil {
		return nil, protocol(op, "invalid "+field)
	}
	if !ok {
		return nil, nil
	}
	if d.IsNegative() {
		return nil, protocol(op, field+" must not be negative")
	}
	m := courier.NewBDT(d)
	return &m, nil
}
