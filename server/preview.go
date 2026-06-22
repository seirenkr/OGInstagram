package main

import (
	"math"
	"time"
)

type previewStatusSeries struct {
	T        []int64   `json:"t"`
	Latency  []float64 `json:"latency"`
	Resolved []float64 `json:"resolved"`
	Failed   []float64 `json:"failed"`
}

func localPreviewStatus(now time.Time) []byte {
	const points = 144
	series := previewStatusSeries{
		T:        make([]int64, points),
		Latency:  make([]float64, points),
		Resolved: make([]float64, points),
		Failed:   make([]float64, points),
	}
	end := now.Truncate(10 * time.Minute)
	start := end.Add(-time.Duration(points-1) * 10 * time.Minute)
	for i := range points {
		t := start.Add(time.Duration(i) * 10 * time.Minute)
		hourWave := math.Sin(float64(i) * math.Pi / 36)
		shortWave := math.Sin(float64(i) * math.Pi / 7)
		traffic := 76 + 34*hourWave + 11*shortWave
		failures := 1 + math.Max(0, 3*math.Sin(float64(i)*math.Pi/17))
		latency := 182 + 38*math.Sin(float64(i)*math.Pi/29) + 16*math.Cos(float64(i)*math.Pi/8)
		series.T[i] = t.Unix()
		series.Resolved[i] = math.Round(math.Max(8, traffic))
		series.Failed[i] = math.Round(failures)
		series.Latency[i] = math.Round(math.Max(80, latency)*100) / 100
	}
	return mustJSON(series)
}
