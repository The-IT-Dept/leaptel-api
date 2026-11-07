package main

import "testing"

// TestFakeServesCatalogue: the assurance catalogue for a technology the fake
// answers tests for is its own, with dev credentials and no upstream call,
// so the automatic tests (and their red and refused scenarios) run end to
// end locally.
func TestFakeServesCatalogue(t *testing.T) {
	_, c, _ := fakeAPI(t)
	tests, _, err := c.ListAssuranceTests("nbn", "FTTP")
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int
	for _, x := range tests {
		numbers = append(numbers, x.TestNumber)
	}
	want := []int{1, 8, 15, 16, 18}
	if len(numbers) != len(want) {
		t.Fatalf("catalogue %v, want %v", numbers, want)
	}
	for i := range want {
		if numbers[i] != want[i] {
			t.Fatalf("catalogue %v, want %v", numbers, want)
		}
	}
	if tests[0].TestName != "Service Health" {
		t.Fatalf("test 1 is %q", tests[0].TestName)
	}
}
