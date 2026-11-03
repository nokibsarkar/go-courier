package courier

import "strings"

// NormalizePhone returns a canonical "+<digits>" form, preserving international
// dialing semantics (PRIV-03). Accepted: "+8801712345678", "008801712345678",
// "8801712345678" (when it begins with 880) and Bangladeshi national
// "01712345678". Anything else fails with ErrAmbiguous instead of guessing.
// The result identifies a number, not a person.
func NormalizePhone(s string) (string, error) {
	s = strings.TrimSpace(s)
	plus := strings.HasPrefix(s, "+")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' || r == '+':
		default:
			return "", Validation("phone", "contains invalid characters")
		}
	}
	d := b.String()
	switch {
	case plus:
	case strings.HasPrefix(d, "00"):
		d = d[2:]
	case len(d) == 11 && strings.HasPrefix(d, "01"):
		d = "880" + d[1:]
	case strings.HasPrefix(d, "880") && len(d) == 13:
	default:
		return "", &Error{Kind: KindAmbiguous, Field: "phone", Message: "cannot determine country; use +<country code> form"}
	}
	if len(d) < 8 || len(d) > 15 {
		return "", Validation("phone", "invalid length")
	}
	return "+" + d, nil
}
