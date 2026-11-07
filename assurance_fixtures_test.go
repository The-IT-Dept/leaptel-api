package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The assurance fixtures (the captured green answers, and every scenario's,
// captured or inferred) are mirrored into the public leaptel-api repo, so
// none may hold a real identifier. Every string is held to the synthetic
// shapes, and every string that is itself JSON (test_result, or anything
// nested deeper) or base64 is decoded and held to them too.
var (
	// nbn and upstream ids (AVC, NTD, location, test, health record):
	// synthetic ones are zeros but for an optional leading digit and their
	// last few digits (AVC000000000001, WRI600000000004, TST000000001007).
	fixtureIDRe   = regexp.MustCompile(`\b[A-Z]{3}(\d{12})\b`)
	syntheticIDRe = regexp.MustCompile(`^\d?0{6,}\d{1,5}$`)
	// An NTD's serial is its vendor's four letters and eight hex digits
	// (ALCL for Nokia); a synthetic one is zeros but for its last digit.
	fixtureSerial = regexp.MustCompile(`\b[A-Z]{4}[0-9A-Fa-f]{8}\b`)
	syntheticSer  = regexp.MustCompile(`^[A-Z]{4}0{7}\d$`)
	// MACs, colon or dash separated, dotted, or bare: only the documentation
	// range (00:00:5e:00:53:xx, RFC 7042).
	fixtureMAC     = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}\b|\b[0-9A-Fa-f]{4}\.[0-9A-Fa-f]{4}\.[0-9A-Fa-f]{4}\b|\b[0-9A-Fa-f]{12}\b`)
	fixtureIPv4    = regexp.MustCompile(`(?:^|[^\d.])((?:\d{1,3}\.){3}\d{1,3})(?:$|[^\d.])`)
	documentIPv4Re = regexp.MustCompile(`^(?:192\.0\.2|198\.51\.100|203\.0\.113)\.`)
	// IPv6 candidates: a run of hex digits, colons and dots with two colons
	// or more; only 2001:db8::/32 (RFC 3849).
	fixtureIPv6 = regexp.MustCompile(`[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*`)
	documentV6  = netip.MustParsePrefix("2001:db8::/32")
	fixtureMail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+)`)
	base64Blob  = regexp.MustCompile(`^[A-Za-z0-9+/_-]{24,}={0,2}$`)
)

// The one example address and the fake's id ranges.
const (
	fixtureAddress  = "1 EXAMPLE ST SAMPLETOWN NSW 2000 "
	fixtureLocality = "SAMPLETOWN"
	fixturePostcode = "2000"
)

// personKeys name a person (upstream staff, a technician, a contact): their
// values may only be empty or a synthetic staff_ name.
var personKeys = regexp.MustCompile(`(?i)(^|_)(by|agent|technician|assignee|contact|owner|user|username|email|phone|mobile|first_?name|last_?name|full_?name)$`)

func TestAssuranceFixturesScrubbed(t *testing.T) {
	files, _ := filepath.Glob("fixtures/test*.json")
	scen, _ := filepath.Glob("fixtures/scenarios/*/*.json")
	files = append(files, scen...)
	if len(files) < 10 {
		t.Fatalf("only %d assurance fixtures found", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("%s: not JSON: %v", f, err)
			continue
		}
		if s, ok := v["test_result"].(string); ok && s != "" && !json.Valid([]byte(s)) {
			t.Errorf("%s: test_result isn't JSON", f)
		}
		for _, p := range scrubProblems("$", v) {
			t.Errorf("%s %s", f, p)
		}
	}
}

// TestAssuranceScrubGuardCatches: the guard fires on each kind of real-shaped
// value, plain, nested in a JSON string, and base64'd. None of these is
// anyone's: they're shapes, not captures.
func TestAssuranceScrubGuardCatches(t *testing.T) {
	leaks := []string{
		"AVC999999999999", "LOC999999999999", "ALCLFFFFFFFF", "02:00:00:00:00:01", "0200.0000.0001", "020000000001",
		"10.1.2.3", "fd00::1", "someone@upstream.com.au",
	}
	for _, leak := range leaks {
		nested, _ := json.Marshal(map[string]any{"value": leak})
		for _, v := range []map[string]any{
			{"value": leak},
			{"test_result": string(nested)},
			{"blob": base64.StdEncoding.EncodeToString([]byte("the value is " + leak + " here"))},
		} {
			if len(scrubProblems("$", v)) == 0 {
				t.Errorf("the guard misses %q in %v", leak, v)
			}
		}
	}
	for _, v := range []map[string]any{
		{"address": "2 OTHER RD ELSEWHERE VIC 3000 "}, {"premisesAddress": "2 OTHER RD"}, {"locality": "ELSEWHERE"},
		{"postcode": "3000"}, {"service_id": float64(123456)}, {"test_id": float64(123456)}, {"updated_by": "Jane Citizen"},
	} {
		if len(scrubProblems("$", v)) == 0 {
			t.Errorf("the guard misses %v", v)
		}
	}
	for _, v := range []map[string]any{
		{"value": "AVC000000000001 NTD000000000001 ALCL00000001 00:00:5e:00:53:01 192.0.2.1 2001:db8::1 a@example.com"},
		{"updated_by": "staff_a"}, {"unidVlanMacAddress": "Unknown"}, {"timeStamp": "2026-11-07T17:15:18.240Z"},
	} {
		if p := scrubProblems("$", v); len(p) != 0 {
			t.Errorf("the guard flags synthetic %v: %v", v, p)
		}
	}
}

// scrubProblems is every way v, at path, isn't scrubbed.
func scrubProblems(path string, v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			at := path + "." + k
			out = append(out, keyProblems(at, k, c)...)
			out = append(out, scrubProblems(at, c)...)
		}
	case []any:
		for i, c := range x {
			out = append(out, scrubProblems(path+"["+strconv.Itoa(i)+"]", c)...)
		}
	case string:
		out = append(out, stringProblems(path, x)...)
		if s := strings.TrimSpace(x); strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
			var inner any
			if json.Unmarshal([]byte(s), &inner) == nil {
				out = append(out, scrubProblems(path+"<json>", inner)...)
			}
		}
		if s := strings.TrimSpace(x); base64Blob.MatchString(s) {
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
				if b, err := enc.DecodeString(s); err == nil && utf8.Valid(b) {
					out = append(out, scrubProblems(path+"<base64>", string(b))...)
					break
				}
			}
		}
	}
	return out
}

// keyProblems holds the keys that name a place, a service, a test or a
// person to the fixtures' synthetic values.
func keyProblems(at, k string, c any) []string {
	lk := strings.ToLower(k)
	switch {
	case lk == "locality":
		if c != fixtureLocality {
			return []string{fmt.Sprintf("%s: %q isn't the example locality", at, c)}
		}
	case lk == "postcode":
		if c != fixturePostcode {
			return []string{fmt.Sprintf("%s: %q isn't the example postcode", at, c)}
		}
	case strings.Contains(lk, "address") && !strings.Contains(lk, "mac") && !strings.Contains(lk, "ip"):
		if s, ok := c.(string); ok && s != "" && s != fixtureAddress {
			return []string{fmt.Sprintf("%s: %q isn't the example address", at, s)}
		}
	case k == "service_id":
		if n, ok := c.(float64); !ok || n < 900000 || n > 999999 {
			return []string{fmt.Sprintf("%s: %v isn't a synthetic service id (900000 up)", at, c)}
		}
	case k == "test_id", k == "externalId":
		if !syntheticTestID(c) {
			return []string{fmt.Sprintf("%s: %v isn't a synthetic test id (100000-100999)", at, c)}
		}
	case personKeys.MatchString(k):
		if s, ok := c.(string); ok && s != "" && !strings.HasPrefix(s, "staff_") {
			return []string{fmt.Sprintf("%s: %q names someone (use a staff_ name)", at, s)}
		}
	}
	return nil
}

// stringProblems is the real-shaped ids, serials, MACs, addresses and mail
// in one string.
func stringProblems(path, x string) []string {
	var out []string
	for _, m := range fixtureIDRe.FindAllStringSubmatch(x, -1) {
		if !syntheticIDRe.MatchString(m[1]) {
			out = append(out, fmt.Sprintf("%s: %s isn't a synthetic id", path, m[0]))
		}
	}
	for _, m := range fixtureSerial.FindAllString(x, -1) {
		if !syntheticSer.MatchString(m) {
			out = append(out, fmt.Sprintf("%s: %s isn't a synthetic serial", path, m))
		}
	}
	for _, m := range fixtureMAC.FindAllString(x, -1) {
		hex := strings.ToLower(strings.NewReplacer(":", "", "-", "", ".", "").Replace(m))
		if !strings.HasPrefix(hex, "00005e0053") {
			out = append(out, fmt.Sprintf("%s: MAC %s isn't in the documentation range", path, m))
		}
	}
	for _, m := range fixtureIPv4.FindAllStringSubmatch(x, -1) {
		if !documentIPv4Re.MatchString(m[1]) {
			out = append(out, fmt.Sprintf("%s: IPv4 %s isn't a documentation address", path, m[1]))
		}
	}
	for _, m := range fixtureIPv6.FindAllString(x, -1) {
		if a, err := netip.ParseAddr(strings.Trim(m, ".")); err == nil && a.Is6() && !a.Is4In6() && !documentV6.Contains(a) {
			out = append(out, fmt.Sprintf("%s: IPv6 %s isn't a documentation address", path, m))
		}
	}
	for _, m := range fixtureMail.FindAllStringSubmatch(x, -1) {
		d := strings.ToLower(m[1])
		if d != "example.com" && d != "example.net" && d != "example.org" && !strings.HasSuffix(d, ".example") &&
			!strings.HasSuffix(d, ".invalid") && !strings.HasSuffix(d, ".test") {
			out = append(out, fmt.Sprintf("%s: mail %s isn't at an example domain", path, m[0]))
		}
	}
	return out
}

func syntheticTestID(v any) bool {
	var n int
	switch x := v.(type) {
	case float64:
		n = int(x)
	case string:
		var err error
		if n, err = strconv.Atoi(x); err != nil {
			return false
		}
	default:
		return false
	}
	return n >= 100000 && n <= 100999
}
