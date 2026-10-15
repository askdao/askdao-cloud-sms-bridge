// [INPUT]: strings only
// [OUTPUT]: NormalizeNumber
// [POS]: internal/phone's phone-number rule, shared by inbound senders (inbound.go) and number targets (outbound.go)
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package phone

import "strings"

// NormalizeNumber turns the number Google Messages reports into E.164 where it can, assuming
// North America for numbers without a country code: a leading "+" or 11+ digits becomes
// "+<digits>", 10 digits becomes "+1<digits>". Shorter numbers (short codes) are returned as
// bare digits. An address with "@" (an email member of an MMS group) is returned lowercased.
func NormalizeNumber(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "@") {
		return strings.ToLower(raw)
	}
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	switch {
	case d == "":
		return ""
	case strings.HasPrefix(raw, "+") || len(d) >= 11:
		return "+" + d
	case len(d) == 10:
		return "+1" + d
	default:
		return d
	}
}
