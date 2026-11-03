package courier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// Currency is an upper-case ISO-4217-style code.
type Currency string

// BDT is the canonical default currency (MON-02).
const BDT Currency = "BDT"

// ParseCurrency normalises and validates a currency code.
func ParseCurrency(s string) (Currency, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 3 {
		return "", Validation("currency", "must be a 3-letter code")
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return "", Validation("currency", "must be a 3-letter code")
		}
	}
	return Currency(s), nil
}

// Money is an exact amount in a currency (MON-01). float types are never used.
// An empty Currency on an application-created Money means BDT; see Code.
type Money struct {
	Amount   decimal.Decimal
	Currency Currency
}

// NewMoney builds a Money in the given currency. An empty currency means BDT.
func NewMoney(amount decimal.Decimal, currency Currency) (Money, error) {
	if currency == "" {
		currency = BDT
	}
	c, err := ParseCurrency(string(currency))
	if err != nil {
		return Money{}, err
	}
	return Money{Amount: amount, Currency: c}, nil
}

// NewBDT builds a BDT Money.
func NewBDT(amount decimal.Decimal) Money { return Money{Amount: amount, Currency: BDT} }

// ParseMoney parses a decimal string. An empty currency means BDT.
func ParseMoney(amount string, currency Currency) (Money, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(amount))
	if err != nil {
		return Money{}, Validation("amount", "not a valid decimal")
	}
	return NewMoney(d, currency)
}

// Code returns the effective currency (BDT when unset).
func (m Money) Code() Currency {
	if m.Currency == "" {
		return BDT
	}
	return m.Currency
}

func (m Money) same(o Money, op string) error {
	if m.Code() != o.Code() {
		return &Error{Kind: KindValidation, Op: op, Message: fmt.Sprintf("%s vs %s", m.Code(), o.Code()), Err: ErrCurrencyMismatch}
	}
	return nil
}

// Add returns m+o, failing on currency mismatch. No conversion or rounding.
func (m Money) Add(o Money) (Money, error) {
	if err := m.same(o, "money.add"); err != nil {
		return Money{}, err
	}
	return Money{Amount: m.Amount.Add(o.Amount), Currency: m.Code()}, nil
}

// Sub returns m-o, failing on currency mismatch.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.same(o, "money.sub"); err != nil {
		return Money{}, err
	}
	return Money{Amount: m.Amount.Sub(o.Amount), Currency: m.Code()}, nil
}

// Cmp compares m and o, failing on currency mismatch.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.same(o, "money.cmp"); err != nil {
		return 0, err
	}
	return m.Amount.Cmp(o.Amount), nil
}

// Validate checks the currency code and, unless allowNegative, that the amount
// is not negative (MON-06). Credits and adjustments should pass true.
func (m Money) Validate(field string, allowNegative bool) error {
	if _, err := ParseCurrency(string(m.Code())); err != nil {
		return Validation(field, "invalid currency")
	}
	if !allowNegative && m.Amount.IsNegative() {
		return Validation(field, "negative amount not allowed")
	}
	return nil
}

// String formats as "1250.75 BDT".
func (m Money) String() string { return m.Amount.String() + " " + string(m.Code()) }

type moneyJSON struct {
	Amount   json.RawMessage `json:"amount"`
	Currency string          `json:"currency"`
}

// MarshalJSON encodes {"amount":"1250.75","currency":"BDT"} (MON-03); it does
// not depend on decimal's package-global marshalling settings.
func (m Money) MarshalJSON() ([]byte, error) {
	amt, err := json.Marshal(m.Amount.String())
	if err != nil {
		return nil, err
	}
	return json.Marshal(moneyJSON{Amount: amt, Currency: string(m.Code())})
}

// UnmarshalJSON accepts a string or number amount without binary floating
// point and a missing currency meaning BDT.
func (m *Money) UnmarshalJSON(b []byte) error {
	var in moneyJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return Validation("money", "malformed money JSON")
	}
	d, ok, err := ParseAmount(in.Amount)
	if err != nil || !ok {
		return Validation("amount", "missing or invalid amount")
	}
	cur := Currency(in.Currency)
	if in.Currency == "" {
		cur = BDT
	}
	mm, err := NewMoney(d, cur)
	if err != nil {
		return err
	}
	*m = mm
	return nil
}

// ParseAmount parses a provider JSON amount (string, number, null/absent)
// into an exact decimal. present is false for absent/null/empty, so callers
// never treat a missing value as zero (MON-05). Accepted inputs: json.RawMessage,
// json.Number, string, nil.
func ParseAmount(v any) (d decimal.Decimal, present bool, err error) {
	var s string
	switch t := v.(type) {
	case nil:
		return d, false, nil
	case json.RawMessage:
		t = bytes.TrimSpace(t)
		if len(t) == 0 || string(t) == "null" {
			return d, false, nil
		}
		if t[0] == '"' {
			if err := json.Unmarshal(t, &s); err != nil {
				return d, false, Validation("amount", "malformed amount")
			}
		} else {
			s = string(t)
		}
	case json.Number:
		s = t.String()
	case string:
		s = t
	default:
		return d, false, Validation("amount", fmt.Sprintf("unsupported amount type %T", v))
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return d, false, nil
	}
	d, err = decimal.NewFromString(s)
	if err != nil {
		return decimal.Decimal{}, false, Validation("amount", "not a valid decimal")
	}
	return d, true, nil
}
