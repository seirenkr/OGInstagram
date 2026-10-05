package main

import (
	"encoding/json"
	"net/url"
	"os"
	"testing"
)

func TestCanonicalMediaIndexParsing(t *testing.T) {
	var cases []struct {
		Raw   string `json:"raw"`
		Value *int   `json:"value"`
	}
	readFixture(t, "../shared/decimal-cases.json", &cases)
	for _, tc := range cases {
		got, ok := parseCanonicalDecimal(tc.Raw)
		if tc.Value == nil && ok {
			t.Errorf("parseCanonicalDecimal(%q) = %d, want invalid", tc.Raw, got)
		}
		if tc.Value != nil && (!ok || got != *tc.Value) {
			t.Errorf("parseCanonicalDecimal(%q) = %d, %v; want %d", tc.Raw, got, ok, *tc.Value)
		}
	}
}

func TestMediaSelectionSharedCases(t *testing.T) {
	var cases []struct {
		Query     string `json:"query"`
		PathIndex *int   `json:"pathIndex"`
		Selected  *int   `json:"selected"`
	}
	readFixture(t, "../shared/media-selection-cases.json", &cases)
	for _, tc := range cases {
		pathIndex := -1
		if tc.PathIndex != nil {
			pathIndex = *tc.PathIndex
		}
		values, err := url.ParseQuery(tc.Query)
		if err != nil {
			t.Fatal(err)
		}
		selected, specified := mediaSelection(values, pathIndex)
		if tc.Selected == nil && specified {
			t.Errorf("mediaSelection(%q, %d) = %d, specified", tc.Query, pathIndex, selected)
		}
		if tc.Selected != nil && (!specified || selected != *tc.Selected) {
			t.Errorf("mediaSelection(%q, %d) = %d, %v; want %d", tc.Query, pathIndex, selected, specified, *tc.Selected)
		}
	}
}

func readFixture(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
