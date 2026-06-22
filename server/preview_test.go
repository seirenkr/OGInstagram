package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLocalPreviewStatus(t *testing.T) {
	var got previewStatusSeries
	if err := json.Unmarshal(localPreviewStatus(time.Unix(1_800_000_000, 0)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.T) != 144 || len(got.Latency) != 144 || len(got.Resolved) != 144 || len(got.Failed) != 144 {
		t.Fatalf("unexpected series lengths: %+v", got)
	}
	if got.T[1]-got.T[0] != 600 {
		t.Fatalf("unexpected interval: %d", got.T[1]-got.T[0])
	}
}
