// Package prom queries Prometheus and summarizes float range vectors.
package prom

import (
	"errors"
	"math"
	"time"
)

type Sample struct {
	Time  time.Time
	Value float64
}

type Change struct {
	Time   time.Time `json:"time"`
	Before float64   `json:"before"`
	After  float64   `json:"after"`
}

type Summary struct {
	First            *float64 `json:"first"`
	Last             *float64 `json:"last"`
	Min              *float64 `json:"min"`
	Max              *float64 `json:"max"`
	Mean             *float64 `json:"mean"`
	Change           *Change  `json:"change"`
	MissingSteps     int64    `json:"missing_steps"`
	NaNCount         int      `json:"nan_count"`
	PositiveInfCount int      `json:"positive_inf_count"`
	NegativeInfCount int      `json:"negative_inf_count"`
}

// Summarize counts every evaluation step, including the two window edges.
// Nonfinite samples occupy steps but never contribute to numeric statistics.
func Summarize(samples []Sample, start, end time.Time, step time.Duration) (Summary, error) {
	var out Summary
	if step <= 0 || end.Before(start) || end.Sub(start)/step == math.MaxInt64 {
		return out, errors.New("invalid summary range or step")
	}
	out.MissingSteps = int64(end.Sub(start)/step) + 1
	finite := make([]Sample, 0, len(samples))
	scale := 0.0
	for i, s := range samples {
		if s.Time.Before(start) || s.Time.After(end) || s.Time.Sub(start)%step != 0 || (i > 0 && !s.Time.After(samples[i-1].Time)) {
			return Summary{}, errors.New("samples must be ordered, unique and on the requested evaluation grid")
		}
		out.MissingSteps--
		switch {
		case math.IsNaN(s.Value):
			out.NaNCount++
		case math.IsInf(s.Value, 1):
			out.PositiveInfCount++
		case math.IsInf(s.Value, -1):
			out.NegativeInfCount++
		default:
			finite = append(finite, s)
			scale = math.Max(scale, math.Abs(s.Value))
		}
	}
	if len(finite) == 0 {
		return out, nil
	}
	first, last := finite[0].Value, finite[len(finite)-1].Value
	low, high := first, first
	for _, s := range finite {
		low = math.Min(low, s.Value)
		high = math.Max(high, s.Value)
	}
	out.First, out.Last, out.Min, out.Max = &first, &last, &low, &high
	// Normalize before addition or squaring so even MaxFloat64 inputs produce
	// finite means and least-squares costs. Clamp rounding at the scale boundary.
	if scale == 0 {
		scale = 1
	} else {
		_, exponent := math.Frexp(scale)
		scale = math.Ldexp(1, exponent-1)
	}
	sums, squares := make([]float64, len(finite)+1), make([]float64, len(finite)+1)
	for i, s := range finite {
		v := s.Value / scale
		sums[i+1] = sums[i] + v
		squares[i+1] = squares[i] + v*v
	}
	n := len(finite)
	limit := math.MaxFloat64 / scale
	level := func(sum float64, count int) float64 {
		return math.Max(-limit, math.Min(limit, sum/float64(count))) * scale
	}
	mean := level(sums[n], n)
	out.Mean = &mean
	cost := func(sum, sq float64, count int) float64 { return math.Max(0, sq-sum*sum/float64(count)) }
	unsplit := cost(sums[n], squares[n], n)
	best, split := unsplit, 0
	for i := 2; i <= n-2; i++ {
		candidate := cost(sums[i], squares[i], i) + cost(sums[n]-sums[i], squares[n]-squares[i], n-i)
		if candidate < best {
			best, split = candidate, i
		}
	}
	// This is a descriptive heuristic, not a significance test. Require a 20%
	// reduction and a numerical noise floor; choose the earliest tied split.
	if split != 0 && unsplit > 1e-12*float64(n) && best <= 0.8*unsplit {
		out.Change = &Change{Time: finite[split].Time, Before: level(sums[split], split), After: level(sums[n]-sums[split], n-split)}
	}
	return out, nil
}

func magnitude(s Summary) (float64, int) {
	before, after := 0.0, 0.0
	if s.Change != nil {
		before, after = s.Change.Before, s.Change.After
	} else if s.First != nil {
		before, after = *s.First, *s.Last
	}
	difference := math.Abs(after - before)
	if difference == 0 {
		return 0, -1075
	}
	if math.IsInf(difference, 1) {
		fraction, exponent := math.Frexp(math.Abs(after/2 - before/2))
		return fraction, exponent + 1
	}
	return math.Frexp(difference)
}
