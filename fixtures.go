package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
)

// fixtureFS holds canned assurance test responses captured from the real
// Leaptel API (see docs/assurance/). Each file's `test_result` has the
// exact shape our portal UI expects to parse.
//
// These files are mirrored verbatim into the public github.com/the-it-dept/
// leaptel-api repo, so they are scrubbed: addresses, AVC/NTD/serial numbers
// and Leaptel service/test ids are replaced with synthetic values of the
// same shape. Scrub anything you add here before committing.
//
// fixtures/scenarios/<scenario>/ holds a service's answers in a red, down or
// refused scenario (set-assurance-scenario). router_off's are a real capture
// (8 Nov 2026), scrubbed the same way; the rest are inferred from the green
// captures and nbn's indicator rules until a real capture replaces them. Each
// says which in its "_captured" or "_inferred" key, which the fake never
// echoes. TestAssuranceFixturesScrubbed holds every one to synthetic ids.
//
//go:embed fixtures/*.json fixtures/scenarios/*/*.json
var fixtureFS embed.FS

// assuranceFixtures is testNumber -> the parsed response as a free-form
// map so we can faithfully echo every field (including ones absent from
// internal/leaptel.AssuranceResult like indicator_array, test_abbreviation,
// datetime). Loaded once at init.
var assuranceFixtures = map[int]map[string]any{}

func init() {
	entries, err := fixtureFS.ReadDir("fixtures")
	if err != nil {
		slog.Error("loading assurance fixtures", "err", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// Filenames look like "test8.json" or "test15.json".
		base := strings.TrimSuffix(e.Name(), ".json")
		base = strings.TrimPrefix(base, "test")
		n, err := strconv.Atoi(base)
		if err != nil {
			slog.Warn("skipping fixture with non-numeric test number", "name", e.Name())
			continue
		}
		raw, err := fixtureFS.ReadFile("fixtures/" + e.Name())
		if err != nil {
			slog.Error("reading fixture", "name", e.Name(), "err", err)
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			slog.Error("parsing fixture", "name", e.Name(), "err", err)
			continue
		}
		assuranceFixtures[n] = parsed
	}
	slog.Info("loaded assurance fixtures", "count", len(assuranceFixtures))
	loadScenarios()
}

// Assurance scenarios a service can be set to (set-assurance-scenario):
// green is the captured fixtures; the others answer tests 1, 15, 18, 2, 4
// and 7 from fixtures/scenarios/<scenario>/ where they have one, else as
// green; refused refuses every run.
const (
	scenarioGreen   = "green"
	scenarioRefused = "refused"
)

var scenarioNames = []string{scenarioGreen, "nbn_fault", "ntd_power", "fibre_dark", "router_off", "fttn_no_sync",
	"fttc_no_reverse_power", scenarioRefused}

// scenarioTests are the tests a scenario answers itself.
var scenarioTests = map[int]bool{1: true, 15: true, 18: true, 2: true, 4: true, 7: true}

// scenarioFixtures is scenario -> testNumber -> response; refusalBody is
// refused's 422 body.
var (
	scenarioFixtures = map[string]map[int]map[string]any{}
	refusalBody      = map[string]any{"message": "The test couldn't be run on this service right now."}
)

func loadScenarios() {
	for _, name := range scenarioNames {
		dir := "fixtures/scenarios/" + name
		entries, err := fixtureFS.ReadDir(dir)
		if err != nil {
			continue // a scenario with no answers of its own
		}
		for _, e := range entries {
			raw, err := fixtureFS.ReadFile(dir + "/" + e.Name())
			if err != nil {
				slog.Error("reading scenario fixture", "scenario", name, "name", e.Name(), "err", err)
				continue
			}
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				slog.Error("parsing scenario fixture", "scenario", name, "name", e.Name(), "err", err)
				continue
			}
			withoutNotes(parsed)
			if e.Name() == "refusal.json" {
				refusalBody = parsed
				continue
			}
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(e.Name(), "test"), ".json"))
			if err != nil || !scenarioTests[n] {
				slog.Warn("skipping scenario fixture", "scenario", name, "name", e.Name())
				continue
			}
			if scenarioFixtures[name] == nil {
				scenarioFixtures[name] = map[int]map[string]any{}
			}
			scenarioFixtures[name][n] = parsed
		}
	}
}

// withoutNotes drops a fixture's own notes ("_inferred"): they're for whoever
// reads the file, not part of any answer.
func withoutNotes(m map[string]any) {
	for k := range m {
		if strings.HasPrefix(k, "_") {
			delete(m, k)
		}
	}
}

// knownScenario reports whether name is a scenario.
func knownScenario(name string) bool { return slices.Contains(scenarioNames, name) }

// fixtureFor returns a deep copy of the fixture for the given test number in
// the scenario (its own, else the green one), or nil when no fixture exists.
// Callers must mutate the returned map (test_id, service_id, timestamps)
// before returning it to the client.
func fixtureFor(scenario string, testNumber int) map[string]any {
	if src, ok := scenarioFixtures[scenario][testNumber]; ok {
		return deepCopy(src)
	}
	if src, ok := scenarioFixtures[scenarioGreen][testNumber]; ok {
		return deepCopy(src)
	}
	src, ok := assuranceFixtures[testNumber]
	if !ok {
		return nil
	}
	return deepCopy(src)
}

// hasFixture reports whether the test has a real-data answer in any
// scenario.
func hasFixture(testNumber int) bool {
	if _, ok := assuranceFixtures[testNumber]; ok {
		return true
	}
	for _, f := range scenarioFixtures {
		if _, ok := f[testNumber]; ok {
			return true
		}
	}
	return false
}

func deepCopy(in map[string]any) map[string]any {
	b, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// syntheticAssuranceResponse is the fallback for test numbers not covered
// by a real-data fixture. It mimics the Service Health envelope so the
// portal UI's parser still finds a known shape.
func syntheticAssuranceResponse(testNumber int) map[string]any {
	inner := fmt.Sprintf(`{
  "serviceHealth": {
    "status": "Complete",
    "id": "FAKE000000000000",
    "currentCondition": {
      "code": "OTH60001",
      "status": "Green",
      "alertMessage": "No Fault Detected (synthetic fixture)",
      "summary": "Test #%d has no recorded sample. Returning a synthetic green result."
    },
    "overviewIndicator": {
      "connectivity": "Green",
      "performance": "Green",
      "stability": "Green"
    },
    "healthCategory": []
  }
}`, testNumber)
	return map[string]any{
		"test_name":    fmt.Sprintf("Synthetic Test %d", testNumber),
		"test_number":  testNumber,
		"test_result":  inner,
		"provider":     "fake",
		"accessType":   "NFAS",
		"serviceType":  "FTTP",
		"serviceSpeed": "100Mbps/40Mbps",
		"address":      "1 Fake Street",
		"locality":     "Sydney",
		"postcode":     "2000",
	}
}
