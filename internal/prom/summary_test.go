package prom

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func points(values ...float64) []Sample {
	result := make([]Sample, len(values))
	for i, v := range values {
		result[i] = Sample{Time: time.Unix(int64(i), 0), Value: v}
	}
	return result
}

func TestSummaryStatisticsAndChange(t *testing.T) {
	s, err := Summarize(points(2, 2, 2, 10, 10, 10), time.Unix(0, 0), time.Unix(5, 0), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if *s.First != 2 || *s.Last != 10 || *s.Min != 2 || *s.Max != 10 || *s.Mean != 6 || s.MissingSteps != 0 {
		t.Fatalf("statistics: %+v", s)
	}
	if s.Change == nil || !s.Change.Time.Equal(time.Unix(3, 0)) || s.Change.Before != 2 || s.Change.After != 10 {
		t.Fatalf("change: %+v", s.Change)
	}
	flat, err := Summarize(points(4, 4, 4, 4, 4), time.Unix(0, 0), time.Unix(4, 0), time.Second)
	if err != nil || flat.Change != nil {
		t.Fatalf("flat: %+v %v", flat, err)
	}
}

func TestSummaryMissingEdgesAndNonfinite(t *testing.T) {
	samples := points(2, math.NaN(), math.Inf(1), math.Inf(-1), 6)
	for i := range samples {
		samples[i].Time = samples[i].Time.Add(time.Second)
	}
	s, err := Summarize(samples, time.Unix(0, 0), time.Unix(7, 0), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.MissingSteps != 3 || s.NaNCount != 1 || s.PositiveInfCount != 1 || s.NegativeInfCount != 1 || *s.Mean != 4 {
		t.Fatalf("nonfinite: %+v", s)
	}
	for _, samples := range [][]Sample{nil, points(math.NaN(), math.Inf(1))} {
		s, err = Summarize(samples, time.Unix(0, 0), time.Unix(1, 0), time.Second)
		if err != nil || s.First != nil || s.Mean != nil || s.Change != nil {
			t.Fatalf("empty finite set: %+v %v", s, err)
		}
		if _, err := json.Marshal(s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSummaryRejectsInvalidGrid(t *testing.T) {
	for _, samples := range [][]Sample{
		{{Time: time.Unix(0, 0)}, {Time: time.Unix(0, 0)}},
		{{Time: time.Unix(1, 0)}, {Time: time.Unix(0, 0)}},
		{{Time: time.Unix(-1, 0)}}, {{Time: time.Unix(3, 0)}}, {{Time: time.Unix(0, 1)}},
	} {
		if _, err := Summarize(samples, time.Unix(0, 0), time.Unix(2, 0), time.Second); err == nil {
			t.Fatalf("accepted %+v", samples)
		}
	}
	for _, step := range []time.Duration{0, -1} {
		if _, err := Summarize(nil, time.Unix(0, 0), time.Unix(2, 0), step); err == nil {
			t.Fatal("accepted invalid step")
		}
	}
	if _, err := Summarize(nil, time.Unix(2, 0), time.Unix(0, 0), time.Second); err == nil {
		t.Fatal("accepted reversed range")
	}
}

func TestSummaryFractionalAndLargeValues(t *testing.T) {
	start := time.Unix(0, 125000000)
	s, err := Summarize([]Sample{{start, math.MaxFloat64}, {start.Add(250 * time.Millisecond), math.MaxFloat64}}, start, start.Add(250*time.Millisecond), 250*time.Millisecond)
	if err != nil || *s.Mean != math.MaxFloat64 || s.MissingSteps != 0 {
		t.Fatalf("fractional: %+v %v", s, err)
	}
	for _, values := range [][]float64{{-math.MaxFloat64, -math.MaxFloat64, math.MaxFloat64, math.MaxFloat64}, {1, 2, 3, 4}} {
		s, err = Summarize(points(values...), time.Unix(0, 0), time.Unix(3, 0), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := json.Marshal(s); err != nil {
			t.Fatal(err)
		}
	}
}
