package courier

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func TestMoneyJSONRoundTripExact(t *testing.T) {
	m, err := ParseMoney("1250.75", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m)
	if string(b) != `{"amount":"1250.75","currency":"BDT"}` {
		t.Fatalf("got %s", b)
	}
	var back Money
	if err := json.Unmarshal(b, &back); err != nil || !back.Amount.Equal(m.Amount) || back.Currency != BDT {
		t.Fatalf("round trip: %v %v", back, err)
	}
	// number form, no float coercion
	if err := json.Unmarshal([]byte(`{"amount":0.1000000000000000055511151231257827,"currency":"usd"}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Amount.String() != "0.1000000000000000055511151231257827" || back.Currency != "USD" {
		t.Fatalf("precision lost: %v", back)
	}
	if err := json.Unmarshal([]byte(`{"currency":"BDT"}`), &back); err == nil {
		t.Fatal("missing amount must fail, not become zero")
	}
}

func TestMoneyArithmetic(t *testing.T) {
	a, _ := ParseMoney("10.10", BDT)
	b, _ := ParseMoney("0.20", BDT)
	s, err := a.Add(b)
	if err != nil || s.Amount.String() != "10.3" {
		t.Fatalf("%v %v", s, err)
	}
	u, _ := ParseMoney("1", "USD")
	if _, err := a.Add(u); !errors.Is(err, ErrCurrencyMismatch) || !errors.Is(err, ErrValidation) {
		t.Fatalf("want mismatch, got %v", err)
	}
	if (Money{Amount: decimal.NewFromInt(1)}).Code() != BDT {
		t.Fatal("default BDT")
	}
	neg, _ := ParseMoney("-1", BDT)
	if neg.Validate("x", false) == nil || neg.Validate("x", true) != nil {
		t.Fatal("negative handling")
	}
	if _, err := ParseCurrency("TAKA"); err == nil {
		t.Fatal("bad currency")
	}
}

func TestParseAmountAbsentIsNotZero(t *testing.T) {
	for _, in := range []any{nil, json.RawMessage(`null`), json.RawMessage(``), json.RawMessage(`""`), ""} {
		if _, ok, err := ParseAmount(in); ok || err != nil {
			t.Fatalf("%#v ok=%v err=%v", in, ok, err)
		}
	}
	d, ok, err := ParseAmount(json.Number("12.50"))
	if !ok || err != nil || d.String() != "12.5" {
		t.Fatal(d, ok, err)
	}
	if _, _, err := ParseAmount(json.RawMessage(`"abc"`)); err == nil {
		t.Fatal("garbage must error")
	}
}

func TestErrorsIsAs(t *testing.T) {
	err := error(&Error{Kind: KindIndeterminate, Provider: "x", Op: "create", Outcome: OutcomeIndeterminate})
	wrapped := errors.Join(errors.New("ctx"), err)
	if !errors.Is(wrapped, ErrIndeterminate) || errors.Is(wrapped, ErrValidation) || !IsIndeterminate(wrapped) {
		t.Fatal("kind matching")
	}
	var e *Error
	if !errors.As(wrapped, &e) || e.Op != "create" {
		t.Fatal("As")
	}
}

func TestNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"01711-111111": "+8801711111111", "+880 1711 111111": "+8801711111111",
		"008801711111111": "+8801711111111", "+14155550123": "+14155550123",
	} {
		if got, err := NormalizePhone(in); err != nil || got != want {
			t.Errorf("%q -> %q %v", in, got, err)
		}
	}
	if _, err := NormalizePhone("4155550123"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous: %v", err)
	}
}

func TestReturnRatioNeedsEvidence(t *testing.T) {
	var c DeliveryCounts
	if _, ok := c.ReturnRatio(); ok {
		t.Fatal("no counts -> no ratio")
	}
	tot, ret := 10, 3
	c = DeliveryCounts{Total: &tot, Returned: &ret}
	if r, ok := c.ReturnRatio(); !ok || r.String() != "0.3" {
		t.Fatal(r, ok)
	}
}
