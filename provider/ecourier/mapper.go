package ecourier

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
	"github.com/shopspring/decimal"
)

var ecrPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

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

// errorText extracts the documented {"errors":[...]} text from non-2xx bodies.
func errorText(_ int, body []byte) (string, string) {
	var e struct {
		Errors  []json.RawMessage `json:"errors"`
		Message string            `json:"message"`
	}
	if json.Unmarshal(body, &e) != nil {
		return "", ""
	}
	if m := joinErrors(e.Errors); m != "" {
		return "", m
	}
	return "", e.Message
}

func joinErrors(errs []json.RawMessage) string {
	var parts []string
	for _, v := range errs {
		var s string
		if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
			parts = append(parts, strings.TrimSpace(s))
		}
	}
	return truncate(strings.Join(parts, "; "), 300)
}

func isFalse(raw json.RawMessage) bool {
	t := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return strings.EqualFold(t, "false")
}

func isTrue(raw json.RawMessage) bool {
	t := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return strings.EqualFold(t, "true")
}

func messageText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return truncate(s, 300)
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return truncate(strings.Join(list, "; "), 300)
	}
	return ""
}

// embedded turns a 2xx body that reports failure (success=false or a
// response_code other than 200) into an error (ERR-02). Bodies with neither
// field are not failures by themselves.
func embedded(op string, e envelope) error {
	rc := e.ResponseCode.String()
	if !isFalse(e.Success) && (rc == "" || rc == "200") && len(e.Errors) == 0 {
		return nil
	}
	msg := joinErrors(e.Errors)
	if msg == "" {
		msg = messageText(e.Message)
	}
	if msg == "" {
		var s string
		if json.Unmarshal(e.QueryData, &s) == nil {
			msg = truncate(s, 300)
		}
	}
	err := &courier.Error{Provider: ProviderID, Op: op, Message: msg, Outcome: courier.OutcomeRejected, ProviderCode: rc}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "duplicate"):
		err.Kind = courier.KindConflict
	case rc == "401":
		err.Kind = courier.KindCredentials
	case rc == "403" || strings.Contains(low, "contact with ecourier support") || strings.Contains(low, "contact ecourier support"):
		err.Kind = courier.KindPermission
	case rc == "404" || strings.Contains(low, "not found") || strings.Contains(low, "no data found"):
		err.Kind = courier.KindNotFound
	case rc == "400" || rc == "422" || strings.Contains(low, "invalid number") || strings.Contains(low, "invalid request"):
		err.Kind = courier.KindValidation
	default:
		err.Kind = courier.KindRejected
	}
	return err
}

// decode reads a bounded JSON body and applies the embedded-failure check.
func decode(op string, body []byte) (envelope, error) {
	var e envelope
	if err := httpx.DecodeJSON(ProviderID, op, body, &e); err != nil {
		return e, err
	}
	return e, embedded(op, e)
}

func wholeTaka(field string, m courier.Money, allowZero bool) (json.Number, error) {
	if err := m.Validate(field, false); err != nil {
		return "", err
	}
	if m.Code() != courier.BDT {
		return "", courier.Validation(field+".currency", "ecourier operates in BDT only")
	}
	if !m.Amount.IsInteger() {
		return "", courier.Validation(field, "ecourier accepts whole taka only; the SDK never rounds money")
	}
	if !allowZero && m.Amount.IsZero() {
		return "", courier.Validation(field, "must be greater than zero for this payment method")
	}
	return json.Number(m.Amount.String()), nil
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
		return "", courier.Validation(field, "ecourier requires an 11-digit Bangladeshi mobile number")
	}
	return "0" + n[4:], nil
}

func thanaRef(a courier.Address) string {
	for _, r := range a.Refs {
		if r.Provider == ProviderID && r.Kind == "thana" && strings.TrimSpace(r.ID) != "" {
			return strings.TrimSpace(r.ID)
		}
	}
	return ""
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

// parseEventTime parses "YYYY-MM-DD HH:MM:SS" in the configured location.
func parseEventTime(s string, loc *time.Location) *time.Time {
	if loc == nil {
		return nil
	}

	t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(s), loc)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

var asiaDhaka = time.FixedZone("Asia/Dhaka", 6*60*60)

// mapStatus maps documented tracking statuses (section 2.2.3); anything else,
// including new values, stays unknown with the raw value preserved.
func mapStatus(s string) courier.Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "initiated", "initiated api":
		return courier.StatusCreated
	case "picked up":
		return courier.StatusPickedUp
	case "in source branch", "in transit", "in destination branch":
		return courier.StatusInTransit
	case "on the way to delivery":
		return courier.StatusOutForDelivery
	case "delivered":
		return courier.StatusDelivered
	}
	return courier.StatusUnknown
}

func rawString(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return ""
}

func moneyOf(raw json.RawMessage) *courier.Money {
	d, ok, err := courier.ParseAmount(json.RawMessage(bytes.TrimSpace(raw)))
	if err != nil || !ok {
		return nil
	}
	m := courier.NewBDT(d)
	return &m
}

var _ = decimal.Zero
