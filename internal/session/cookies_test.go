package session

import (
	"strings"
	"testing"
)

// Distinctive fake values. Nothing in these tests touches the network.
var sentinel = map[string]string{
	"SID": "zzSIDval111", "HSID": "zzHSIDval222", "SSID": "zzSSIDval333", "OSID": "zzOSIDval444",
	"APISID": "zzAPISIDval555", "SAPISID": "zzSAPISIDval666", "__Secure-1PSIDTS": "zzPSIDTSval777",
}

func assertNoSentinel(t *testing.T, where, s string) {
	t.Helper()
	for name, v := range sentinel {
		if strings.Contains(s, v) {
			t.Errorf("%s contains the value of %s", where, name)
		}
	}
}

// Shape of Chrome's "Copy as cURL" on macOS: multi-line, cookies via -b, one value URL-encoded.
const chromeCurl = `curl 'https://messages.google.com/web/config' \
  -H 'accept: */*' \
  -H 'accept-language: en-US,en;q=0.9' \
  -b 'NID=other; SID=zzSIDval111; HSID=zzHSIDval222; SSID=zzSSIDval333; OSID=zzOSIDval444; APISID=zzAPISIDval555; SAPISID=zzSAPISIDval666%2Fx; __Secure-1PSIDTS=zzPSIDTSval777' \
  -H 'referer: https://messages.google.com/' \
  -H 'user-agent: Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36'`

// Shape of Safari Web Inspector's "Copy as cURL": -X 'GET' and a Cookie request header.
const safariCurl = `curl 'https://messages.google.com/web/config' \
-X 'GET' \
-H 'Accept: */*' \
-H 'Cookie: SID=zzSIDval111; HSID=zzHSIDval222; SSID=zzSSIDval333; OSID=zzOSIDval444; APISID=zzAPISIDval555; SAPISID=zzSAPISIDval666' \
-H 'Referer: https://messages.google.com/' \
-H 'User-Agent: Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.6 Safari/605.1.15'`

func TestParseChromeCurl(t *testing.T) {
	c, err := ParseCookies(chromeCurl)
	if err != nil {
		t.Fatal(err)
	}
	if c["SAPISID"] != "zzSAPISIDval666/x" {
		t.Errorf("SAPISID not URL-decoded")
	}
	if c["__Secure-1PSIDTS"] != sentinel["__Secure-1PSIDTS"] || c["SID"] != sentinel["SID"] || c["OSID"] != sentinel["OSID"] {
		t.Error("wrong values")
	}
	if _, ok := c["NID"]; ok || len(c) != 7 {
		t.Errorf("should keep only the 7 known cookies, got %v", CookieNames(c))
	}
}

func TestParseSafariCurl(t *testing.T) {
	c, err := ParseCookies(safariCurl)
	if err != nil {
		t.Fatal(err)
	}
	if len(c) != 6 || c["HSID"] != sentinel["HSID"] || c["APISID"] != sentinel["APISID"] {
		t.Errorf("unexpected result: %v", CookieNames(c))
	}
	if _, ok := c["__Secure-1PSIDTS"]; ok {
		t.Error("optional cookie should be absent")
	}
}

func TestParseJSON(t *testing.T) {
	c, err := ParseCookies(`{"SID":"zzSIDval111","HSID":"zzHSIDval222","SSID":"zzSSIDval333","OSID":"zzOSIDval444",
	  "APISID":"zzAPISIDval555","SAPISID":"zzSAPISIDval666%3D","__Secure-1PSIDTS":"zzPSIDTSval777","extra":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if c["SAPISID"] != "zzSAPISIDval666=" || len(c) != 7 {
		t.Errorf("unexpected result: %v", CookieNames(c))
	}
}

func TestMissingCookiesAreNamedNotShown(t *testing.T) {
	_, err := ParseCookies(`{"SID":"zzSIDval111","HSID":"zzHSIDval222","SSID":"zzSSIDval333","APISID":"zzAPISIDval555"}`)
	if err == nil || !strings.Contains(err.Error(), "missing cookies: OSID, SAPISID (") {
		t.Fatalf("error should name OSID and SAPISID, got: %v", err)
	}
	assertNoSentinel(t, "missing-cookie error", err.Error())

	_, err = ParseCookies(strings.Replace(safariCurl, "OSID=zzOSIDval444; ", "", 1))
	if err == nil || !strings.Contains(err.Error(), "missing cookies: OSID (") {
		t.Fatalf("cURL without OSID should name OSID, got: %v", err)
	}
	assertNoSentinel(t, "missing-cookie error", err.Error())

	// Every required cookie is checked, not only the first ones.
	for _, name := range RequiredCookies {
		in := map[string]string{}
		for _, n := range RequiredCookies {
			if n != name {
				in[n] = sentinel[n]
			}
		}
		_, err := ParseCookies(jsonOf(in))
		if err == nil || !strings.Contains(err.Error(), "missing cookies: "+name+" (") {
			t.Errorf("without %s: got %v", name, err)
		}
	}
}

func jsonOf(m map[string]string) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, `"`+k+`":"`+v+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func TestBadInputsNeverEchoValues(t *testing.T) {
	noCookieCurl := strings.Replace(safariCurl, "-H 'Cookie: SID=zzSIDval111; HSID=zzHSIDval222; SSID=zzSSIDval333; OSID=zzOSIDval444; APISID=zzAPISIDval555; SAPISID=zzSAPISIDval666' \\\n", "", 1)
	bad := map[string]string{
		"empty":                    "   \n",
		"curl without cookies":     noCookieCurl,
		"header without colon":     `curl 'https://messages.google.com/web/config' -H 'zzSIDval111;'`, // the parser panics on this
		"unterminated quote":       `curl 'https://messages.google.com/web/config' -b 'SID=zzSIDval111`,
		"bad url escape":           strings.Replace(chromeCurl, "zzSIDval111", "zzSIDval111%zz", 1),
		"json array":               `["zzSIDval111"]`,
		"json number value":        `{"SID": 5, "HSID": "zzHSIDval222"}`,
		"json syntax":              `{"SID": "zzSIDval111", "HSID": zzHSIDval222}`,
		"bash $'' quoting":         strings.Replace(chromeCurl, "-b 'NID=other; ", "-b $'", 1),
		"curl with a stray letter": `curl 'https://messages.google.com/web/config' -b 'SID=zzSIDval111; HSID' x`,
	}
	for name, in := range bad {
		_, err := ParseCookies(in)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		assertNoSentinel(t, name+" error", err.Error())
	}
}
