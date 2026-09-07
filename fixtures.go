package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
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
//go:embed fixtures/*.json
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
}

// fixtureFor returns a deep copy of the fixture for the given test number,
// or nil when no fixture exists. Callers must mutate the returned map
// (test_id, service_id, timestamps) before returning it to the client.
func fixtureFor(testNumber int) map[string]any {
	src, ok := assuranceFixtures[testNumber]
	if !ok {
		return nil
	}
	return deepCopy(src)
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
