// [INPUT]: go.mau.fi/util/curl Parse (the parser upstream's cookie login uses), encoding/json, net/url
// [OUTPUT]: ParseCookies, CookieNames, RequiredCookies, OptionalCookies
// [POS]: internal/session's cookie-file reader for the pair command; store.go persists what pairing produces
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"go.mau.fi/util/curl"
)

// RequiredCookies are the Google account cookies upstream's login flow asks for.
var RequiredCookies = []string{"SID", "HSID", "SSID", "OSID", "APISID", "SAPISID"}

// OptionalCookies are kept when present.
var OptionalCookies = []string{"__Secure-1PSIDTS"}

// ParseCookies reads a browser "Copy as cURL" text or a JSON object of cookie names to values,
// and keeps only RequiredCookies and OptionalCookies. Errors name cookies but never contain
// any part of the input, since the input is a live Google login.
func ParseCookies(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("cookie file is empty")
	}
	found := map[string]string{}
	if strings.HasPrefix(raw, "curl") {
		parsed, err := safeCurlParse(raw)
		if err != nil {
			return nil, err
		}
		for _, c := range parsed.Cookies() {
			v, err := url.PathUnescape(c.Value)
			if err != nil {
				return nil, fmt.Errorf("cookie %s is not valid URL encoding", c.Name)
			}
			found[c.Name] = v
		}
		if len(found) == 0 {
			return nil, errors.New("the cURL text has no cookies (neither -H 'Cookie: ...' nor -b '...'); use the JSON format instead")
		}
	} else {
		if err := json.Unmarshal([]byte(raw), &found); err != nil {
			return nil, jsonError(err)
		}
		for k, v := range found { // like upstream: URL-decode when possible, else keep as is
			if d, err := url.PathUnescape(v); err == nil {
				found[k] = d
			}
		}
	}
	out := map[string]string{}
	var missing []string
	for _, n := range RequiredCookies {
		if v := found[n]; v != "" {
			out[n] = v
		} else {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		hint := ""
		if strings.Contains(raw, "$'") {
			hint = "; the cURL text uses bash $'...' quoting, which the parser does not understand, so use the JSON format instead"
		}
		return nil, fmt.Errorf("missing cookies: %s (required: %s)%s",
			strings.Join(missing, ", "), strings.Join(RequiredCookies, ", "), hint)
	}
	for _, n := range OptionalCookies {
		if v := found[n]; v != "" {
			out[n] = v
		}
	}
	return out, nil
}

// CookieNames returns the sorted names in c, for logging without values.
func CookieNames(c map[string]string) []string {
	names := make([]string, 0, len(c))
	for n := range c {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// safeCurlParse wraps curl.Parse, which can panic on malformed headers and whose errors may
// quote the input. Neither its panic nor its error text is passed on.
func safeCurlParse(raw string) (p *curl.Parsed, err error) {
	defer func() {
		if recover() != nil {
			p, err = nil, errors.New("could not parse the cURL text (details hidden so no cookie is shown); copy it again in full, or use the JSON format")
		}
	}()
	if p, err = curl.Parse(raw); err != nil {
		return nil, errors.New("could not parse the cURL text (details hidden so no cookie is shown); copy it again in full, or use the JSON format")
	}
	return p, nil
}

// jsonError describes a JSON decoding error without quoting the input.
func jsonError(err error) error {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return fmt.Errorf("cookie JSON has a syntax error near byte %d", se.Offset)
	case errors.As(err, &te) && te.Field != "":
		return fmt.Errorf("cookie JSON: the value of %q must be a string", te.Field)
	default:
		return errors.New(`cookie JSON must be an object of string values, like {"SID": "...", ...}`)
	}
}
