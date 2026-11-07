package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// outageFixtureDir holds the upstream outage records the fake serves. They're
// mirrored into the public leaptel-api repo, so every one is scrubbed: service
// ids are synthetic (9000001 up), staff usernames are staff_a or staff_b, and
// customer keys are gone.
const outageFixtureDir = "fixtures/outages"

// outageFixturesWanted is the corpus the parser and fake tests read by name.
var outageFixturesWanted = []string{
	"crq-planned-service.json",
	"crq-planned-national.json",
	"crq-cancelled.json",
	"crq-cancelled-2.json",
	"inc-ticketed-service.json",
	"inc-classified.json",
	"inc-open-local-finis.json",
	"upstream-poi.json",
	"upstream-state.json",
	"upstream-chg.json",
	"opticomm-type2-state.json",
	"blank-ocr.json",
	"other-network.json",
	"internal.json",
	"wholesaler.json",
	"hazard.json",
	"draft.json",
	"empty.json",
	"service-page-past-last.json",
	"national-page.json",
}

var (
	scrubbedUpdatedBy = map[string]bool{
		"nbn_outage_notification": true,
		"get_trouble_tickets":     true,
		"staff_a":                 true,
		"staff_b":                 true,
	}
	scrubbedLOC = regexp.MustCompile(`\bLOC\d{12}\b`)
	scrubbedAVC = regexp.MustCompile(`\bAVC\d{12}\b`)
)

func TestOutageFixturesScrubbed(t *testing.T) {
	entries, err := os.ReadDir(outageFixtureDir)
	if err != nil {
		t.Fatalf("reading %s: %v", outageFixtureDir, err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		have[e.Name()] = true
		raw, err := os.ReadFile(filepath.Join(outageFixtureDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("%s: not JSON: %v", e.Name(), err)
			continue
		}
		checkScrubbed(t, e.Name(), "$", v)
	}
	for _, name := range outageFixturesWanted {
		if !have[name] {
			t.Errorf("missing fixture %s", name)
		}
	}
}

// checkScrubbed walks every value of a fixture, pages and records alike.
func checkScrubbed(t *testing.T, file, path string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			at := path + "." + k
			if strings.HasPrefix(k, "customer_") {
				t.Errorf("%s %s: customer key kept", file, at)
			}
			switch k {
			case "updated_by":
				if s, _ := child.(string); !scrubbedUpdatedBy[s] {
					t.Errorf("%s %s: updated_by %q isn't scrubbed", file, at, s)
				}
			case "scope":
				checkScopeServices(t, file, at, child)
			}
			checkScrubbed(t, file, at, child)
		}
	case []any:
		for i, child := range x {
			checkScrubbed(t, file, path+"["+strconv.Itoa(i)+"]", child)
		}
	case string:
		if scrubbedLOC.MatchString(x) || scrubbedAVC.MatchString(x) {
			t.Errorf("%s %s: holds a LOC or AVC id: %q", file, path, x)
		}
	}
}

// checkScopeServices holds every scope.service value, string or number, to a
// synthetic id.
func checkScopeServices(t *testing.T, file, path string, scope any) {
	t.Helper()
	m, ok := scope.(map[string]any)
	if !ok {
		return // "scope": [] is PHP's empty object
	}
	list, _ := m["service"].([]any)
	for i, sv := range list {
		var s string
		switch x := sv.(type) {
		case string:
			s = x
		case float64:
			s = strconv.FormatFloat(x, 'f', -1, 64)
		default:
			t.Errorf("%s %s.service[%d]: %v isn't an id", file, path, i, sv)
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 9000000 {
			t.Errorf("%s %s.service[%d]: %q isn't a synthetic id (≥ 9000000)", file, path, i, s)
		}
	}
}
