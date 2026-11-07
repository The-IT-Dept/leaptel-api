package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestFakeScenarioRoute: a service set to a scenario answers its tests from
// that scenario's fixtures (falling back to the green ones), without the
// fixtures' own notes; refused refuses the run with 422 and a JSON body; an
// unknown scenario is a 400; the scenario survives a restart.
func TestFakeScenarioRoute(t *testing.T) {
	srv, c, base := fakeAPI(t)
	const svc = 3101

	result := func(test int) string {
		t.Helper()
		res, raw, err := c.RunAssuranceTest(svc, test)
		if err != nil {
			t.Fatalf("test %d: %v", test, err)
		}
		if strings.Contains(string(raw), "_inferred") {
			t.Fatalf("test %d echoes the fixture's note: %.200s", test, raw)
		}
		var s string
		if json.Unmarshal(res.TestResult, &s) != nil {
			s = string(res.TestResult)
		}
		return s
	}

	// Default (green): the captured fixtures.
	if r := result(18); !strings.Contains(r, "Carrier detected") {
		t.Fatalf("green UNI-D: %.300s", r)
	}

	for _, c := range []struct {
		scenario string
		test     int
		want     string
	}{
		{"router_off", 18, "No carrier"},
		{"router_off", 15, `"Operational State","value":"UP"`}, // its own copy of the NTD being up
		{"ntd_power", 15, "Dying Gasp"},
		{"ntd_power", 1, "Dying Gasp"},
		{"nbn_fault", 1, `"id":"currentOutage","status":null,"timeStamp":"2026-04-17T06:04:12.468828Z","unit":null,"value":"true"`},
		{"fttn_no_sync", 7, "Out of Sync"},
		{"fttc_no_reverse_power", 2, "No Reverse Power"},
		{"fibre_dark", 18, "Carrier detected"}, // none of its own: the green one
		{"green", 7, "In Sync"},
	} {
		if code, out := post(t, base+"/services/3101/set-assurance-scenario", map[string]string{"scenario": c.scenario}); code != 200 {
			t.Fatalf("set %s: %d %v", c.scenario, code, out)
		}
		if r := result(c.test); !strings.Contains(r, c.want) {
			t.Errorf("%s test %d: want %q in %.400s", c.scenario, c.test, c.want, r)
		}
	}

	if code, out := post(t, base+"/services/3101/set-assurance-scenario", map[string]string{"scenario": "refused"}); code != 200 {
		t.Fatalf("set refused: %d %v", code, out)
	}
	res, raw, err := c.RunAssuranceTest(svc, 1)
	if err == nil || res != nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("refused: %v %v", res, err)
	}
	want, _ := os.ReadFile("fixtures/scenarios/refused/refusal.json")
	var body, file map[string]any
	if json.Unmarshal(raw, &body) != nil || json.Unmarshal(want, &file) != nil || body["message"] != file["message"] || body["_inferred"] != nil {
		t.Fatalf("refused body: %s", raw)
	}
	// Another service is untouched.
	if res, _, err := c.RunAssuranceTest(svc+1, 1); err != nil || res == nil {
		t.Fatalf("another service: %v", err)
	}

	if code, _ := post(t, base+"/services/3101/set-assurance-scenario", map[string]string{"scenario": "melted"}); code != 400 {
		t.Fatalf("an unknown scenario: %d", code)
	}

	// The scenario is part of the saved state.
	again := loadState(srv.state.stateFile)
	if again.scenario(svc) != "refused" {
		t.Fatalf("after a restart: %q", again.scenario(svc))
	}
}
