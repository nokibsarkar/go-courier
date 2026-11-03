package redx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/httpx"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func protocol(op, msg string) *courier.Error {
	return &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: op, Message: msg}
}

func asIndeterminate(err error) error {
	return &courier.Error{Kind: courier.KindIndeterminate, Provider: ProviderID, Outcome: courier.OutcomeIndeterminate,
		Message: "unusable response to a state-changing request; the operation may have been applied", Err: err}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// errorText extracts a message from non-2xx bodies. The error body shape is
// not documented; common keys are tried and nothing is invented.
func errorText(_ int, body []byte) (string, string) {
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return "", ""
	}
	if e.Message != "" {
		return "", truncate(e.Message, 300)
	}
	return "", truncate(e.Error, 300)
}

func parseTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	for _, l := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(l, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, asiaDhaka); err == nil {
		return &t
	}
	return nil // absent, never substituted
}

var asiaDhaka = time.FixedZone("Asia/Dhaka", 6*60*60)

// mapStatus maps known parcel statuses. Unmapped values (paid, agent-area-change, new ones)
// stay unknown with the raw value preserved.
func mapStatus(s string) courier.Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pickup-pending":
		return courier.StatusPickupPending
	case "ready-for-delivery":
		return courier.StatusPickedUp // "Parcel received from merchants"
	case "delivery-in-progress":
		return courier.StatusOutForDelivery
	case "delivered":
		return courier.StatusDelivered
	case "agent-hold":
		return courier.StatusOnHold
	case "agent-returning":
		return courier.StatusReturning
	case "returned":
		return courier.StatusReturned
	case "cancelled", "canceled":
		return courier.StatusCancelled
	}
	return courier.StatusUnknown
}

func jsonDecode(op string, raw []byte, out any) error {
	return httpx.DecodeJSON(ProviderID, op, bytes.TrimSpace(raw), out)
}

func wholeTaka(field string, m courier.Money) (string, error) {
	if err := m.Validate(field, false); err != nil {
		return "", err
	}
	if m.Code() != courier.BDT {
		return "", courier.Validation(field+".currency", "redx operates in BDT only")
	}
	if !m.Amount.IsInteger() {
		return "", courier.Validation(field, "redx examples show whole taka only; the SDK never rounds money")
	}
	return m.Amount.String(), nil
}

func localMobile(field, in string) (string, error) {
	n, err := courier.NormalizePhone(in)
	if err != nil {
		if ce, ok := err.(*courier.Error); ok {
			ce.Field = field
		}
		return "", err
	}
	if !strings.HasPrefix(n, "+880") || len(n) != 14 {
		return "", courier.Validation(field, "redx requires an 11-digit Bangladeshi mobile number")
	}
	return "0" + n[4:], nil
}

func limited(field, v string, n int) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", courier.Validation(field, "required")
	}
	if utf8.RuneCountInString(v) > n {
		return "", courier.Validation(field, fmt.Sprintf("at most %d characters", n))
	}
	return v, nil
}

func refID(refs []courier.LocationRef, kind string) string {
	for _, r := range refs {
		if r.Provider == ProviderID && r.Kind == kind && strings.TrimSpace(r.ID) != "" {
			return strings.TrimSpace(r.ID)
		}
	}
	return ""
}
