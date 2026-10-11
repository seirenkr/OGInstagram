package main

import "testing"

func TestPreviewRouteSharedCases(t *testing.T) {
	var cases []struct {
		Path  string `json:"path"`
		Valid bool   `json:"valid"`
	}
	readFixture(t, "../shared/route-cases.json", &cases)
	r := publicRequest("POST", "/api/embed")
	for _, tc := range cases {
		if _, _, ok := previewTarget(r, tc.Path); ok != tc.Valid {
			t.Errorf("previewTarget(%q) = %v, want %v", tc.Path, ok, tc.Valid)
		}
	}
}
