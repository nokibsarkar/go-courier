package carrybee

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/httpx"
)

var (
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	numPattern = regexp.MustCompile(`^[0-9]{1,18}$`)
)

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

// errorMessage builds an error text from message (or error_description) plus "field: msg1, msg2" entries from "errors".
func errorMessage(body []byte) (msg string, isErr bool) {
	var e struct {
		Message          string          `json:"message"`
		ErrorDescription string          `json:"error_description"`
		Error            json.RawMessage `json:"error"`
		Status           json.RawMessage `json:"status"`
		Errors           json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &e) != nil {
		return "", false
	}
	var details []string
	var fields map[string][]string
	if len(e.Errors) > 0 && json.Unmarshal(e.Errors, &fields) == nil && len(fields) > 0 {
		isErr = true
		names := make([]string, 0, len(fields))
		for k := range fields {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			details = append(details, strings.ReplaceAll(k, "_", " ")+": "+strings.Join(fields[k], ", "))
		}
	}
	if t := strings.TrimSpace(string(e.Error)); t != "" && t != "null" && t != "false" && t != "0" && t != `""` {
		isErr = true
	}
	if t := strings.TrimSpace(string(e.Status)); t == "false" {
		isErr = true
	}
	msg = strings.TrimSpace(e.Message)
	if msg == "" {
		msg = strings.TrimSpace(e.ErrorDescription)
	}
	if len(details) > 0 {
		d := strings.Join(details, " | ")
		if msg == "" || msg == "Validation error" {
			msg = d
		} else {
			msg += " (" + d + ")"
		}
	}
	return truncate(msg, 300), isErr
}

// errorText is the httpx hook for non-2xx responses.
func errorText(_ int, body []byte) (string, string) {
	m, _ := errorMessage(body)
	return "", m
}

// embeddedError turns a 2xx body that states a failure (error set, status
// false or a non-empty errors object) into a definite rejection. Success
// bodies pass through.
func embeddedError(op string, body []byte) error {
	m, isErr := errorMessage(body)
	if !isErr {
		return nil
	}
	if m == "" {
		m = "carrybee reported an error"
	}
	return &courier.Error{Kind: courier.KindRejected, Provider: ProviderID, Op: op, Outcome: courier.OutcomeRejected, Message: m}
}

func jsonDecode(op string, raw []byte, out any) error {
	return httpx.DecodeJSON(ProviderID, op, bytes.TrimSpace(raw), out)
}

// normStatus lowercases and unifies separators so "order.in-transit",
// "In Transit" and "in_transit" compare equal.
func normStatus(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "order.")
	return strings.NewReplacer("_", "-", " ", "-").Replace(s)
}

// mapStatus maps status and event names; anything unrecognised stays unknown
// with the raw value kept.
func mapStatus(s string) courier.Status {
	switch normStatus(s) {
	case "created":
		return courier.StatusCreated
	case "picked", "picked-up":
		return courier.StatusPickedUp
	case "in-transit":
		return courier.StatusInTransit
	case "at-the-sorting-hub", "received-at-last-mile-hub":
		return courier.StatusInTransit
	case "assigned-for-delivery":
		return courier.StatusOutForDelivery
	case "delivered":
		return courier.StatusDelivered
	case "delivery-failed":
		return courier.StatusDeliveryFailed
	case "returned-to-merchant", "returned":
		return courier.StatusReturned
	case "cancelled", "canceled":
		return courier.StatusCancelled
	}
	return courier.StatusUnknown
}

func wholeTaka(field string, m courier.Money) (int64, error) {
	if err := m.Validate(field, false); err != nil {
		return 0, err
	}
	if m.Code() != courier.BDT {
		return 0, courier.Validation(field+".currency", "carrybee operates in BDT only")
	}
	if !m.Amount.IsInteger() {
		return 0, courier.Validation(field, "carrybee takes a whole-taka integer; the SDK never rounds money")
	}
	if !m.Amount.BigInt().IsInt64() {
		return 0, courier.Validation(field, "amount too large")
	}
	return m.Amount.BigInt().Int64(), nil
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
		return "", courier.Validation(field, "carrybee needs an 11-digit Bangladeshi mobile number")
	}
	return "0" + n[4:], nil
}

func required(field, v string, n int) (string, error) {
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

func validationID(field string) error {
	return courier.Validation(field, "required and must be numeric where an ID")
}
