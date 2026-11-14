package main

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func scoreTestMetric(name, group string, rate float64) tuneMetric {
	metric := tuneMetric{Name: name, Group: group, Unit: "ops/s", Rate: rate}
	if group != "cargo" {
		metric.Native, metric.Ratio = 100, rate/100
	}
	return metric
}

func scoreTestMetrics(io, metadata, cargo float64) []tuneMetric {
	return []tuneMetric{
		scoreTestMetric("random-write", "io", io),
		scoreTestMetric("lookup", "metadata", metadata),
		scoreTestMetric("incremental-build", "cargo", cargo),
	}
}

func scoreTestClose(t *testing.T, got, want float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > 1e-12 {
		t.Fatalf("got %.15g, want %.15g", got, want)
	}
}

func TestScoreTuneRejectsMaskedRegressions(t *testing.T) {
	for _, group := range []string{"metadata", "cargo"} {
		t.Run(group, func(t *testing.T) {
			candidate := scoreTestMetrics(100000, 100, 100)
			for i := range candidate {
				if candidate[i].Group == group {
					candidate[i] = scoreTestMetric(candidate[i].Name, group, 70)
				}
			}
			results := []tuneResult{{Metrics: scoreTestMetrics(100, 100, 100)}, {Metrics: candidate}}
			best, err := scoreTune(results, 0.20)
			if err != nil || best != 0 || results[1].Eligible {
				t.Fatalf("masked regression selected: best=%d err=%v result=%+v", best, err, results[1])
			}
			scoreTestClose(t, results[1].Worst, 0.7)
			if !strings.Contains(results[1].Reason, "30.0%") || !strings.Contains(results[1].Reason, "maximum 20.0%") {
				t.Fatalf("uninformative regression reason: %q", results[1].Reason)
			}
		})
	}
}

func TestScoreTuneCapsIsolatedAndUniformGains(t *testing.T) {
	results := []tuneResult{
		{Metrics: scoreTestMetrics(100, 100, 100)},
		{Metrics: scoreTestMetrics(100000, 85, 85)},
		{Metrics: scoreTestMetrics(100000, 100000, 100000)},
	}
	best, err := scoreTune(results, 0.20)
	if err != nil || best != 2 || !results[1].Eligible || results[1].Score >= 1 {
		t.Fatalf("unexpected capped ranking: best=%d err=%v results=%+v", best, err, results)
	}
	scoreTestClose(t, results[1].Score, math.Sqrt(0.85*math.Cbrt(2*0.85*0.85)))
	scoreTestClose(t, results[2].Score, 2)
	scoreTestClose(t, results[2].Worst, 1000)
}

func TestScoreTuneUniformImprovementAndReportOrder(t *testing.T) {
	results := []tuneResult{
		{Config: tuneConfig{Proto: "baseline"}, Metrics: scoreTestMetrics(100, 100, 100)},
		{Config: tuneConfig{Proto: "better"}, Metrics: scoreTestMetrics(125, 125, 125)},
		{Config: tuneConfig{Proto: "middle"}, Metrics: scoreTestMetrics(110, 110, 110)},
	}
	// Rounds and candidates need not emit metrics in the same order.
	results[1].Metrics[0], results[1].Metrics[2] = results[1].Metrics[2], results[1].Metrics[0]
	best, err := scoreTune(results, 0.20)
	if err != nil || best != 1 {
		t.Fatalf("best=%d err=%v", best, err)
	}
	for i, proto := range []string{"baseline", "better", "middle"} {
		if results[i].Config.Proto != proto || !results[i].Eligible || results[i].Reason != "" {
			t.Fatalf("report %d changed order or eligibility: %+v", i, results[i])
		}
	}
	scoreTestClose(t, results[0].Score, 1)
	scoreTestClose(t, results[0].Worst, 1)
	scoreTestClose(t, results[1].Score, 1.25)
	scoreTestClose(t, results[1].Worst, 1.25)
}

func TestScoreTuneNativeNormalizationAndCargoRate(t *testing.T) {
	baseline := scoreTestMetrics(100, 100, 100)
	candidate := scoreTestMetrics(300, 300, 120)
	for i := range 2 {
		baseline[i].Native, baseline[i].Ratio = 200, 0.5
		candidate[i].Native, candidate[i].Ratio = 500, 0.6
	}
	results := []tuneResult{{Metrics: baseline}, {Metrics: candidate}}
	best, err := scoreTune(results, 0.20)
	if err != nil || best != 1 {
		t.Fatalf("best=%d err=%v", best, err)
	}
	scoreTestClose(t, results[1].Score, 1.2)
	scoreTestClose(t, results[1].Worst, 1.2)
}

func TestScoreTuneBalancesGroupsNotMetricCounts(t *testing.T) {
	var previous float64
	for _, ioCount := range []int{1, 32} {
		baseline := scoreTestMetrics(100, 100, 100)
		candidate := scoreTestMetrics(150, 110, 120)
		for i := 1; i < ioCount; i++ {
			name := fmt.Sprintf("io-%d", i)
			baseline = append(baseline, scoreTestMetric(name, "io", 100))
			candidate = append(candidate, scoreTestMetric(name, "io", 150))
		}
		results := []tuneResult{{Metrics: baseline}, {Metrics: candidate}}
		best, err := scoreTune(results, 0.20)
		if err != nil || best != 1 {
			t.Fatalf("count=%d best=%d err=%v", ioCount, best, err)
		}
		scoreTestClose(t, results[1].Score, math.Sqrt(1.1*math.Cbrt(1.5*1.1*1.2)))
		if previous != 0 {
			scoreTestClose(t, results[1].Score, previous)
		}
		previous = results[1].Score
	}
}

func TestScoreTuneRejectsInvalidMeasurements(t *testing.T) {
	cases := []struct {
		name   string
		change func(*tuneResult)
	}{
		{"failed", func(r *tuneResult) { r.Error = "worker exited" }},
		{"empty", func(r *tuneResult) { r.Metrics = nil }},
		{"missing", func(r *tuneResult) { r.Metrics = r.Metrics[:2] }},
		{"extra", func(r *tuneResult) { r.Metrics = append(r.Metrics, scoreTestMetric("extra", "io", 100)) }},
		{"duplicate", func(r *tuneResult) { r.Metrics[1] = r.Metrics[0] }},
		{"name", func(r *tuneResult) { r.Metrics[0].Name = "" }},
		{"changed-name", func(r *tuneResult) { r.Metrics[0].Name = "other" }},
		{"unknown-group", func(r *tuneResult) { r.Metrics[0].Group = "iops" }},
		{"changed-group", func(r *tuneResult) { r.Metrics[0].Group = "metadata" }},
		{"changed-unit", func(r *tuneResult) { r.Metrics[0].Unit = "MiB/s" }},
		{"nan-rate", func(r *tuneResult) { r.Metrics[0].Rate = math.NaN() }},
		{"infinite-rate", func(r *tuneResult) { r.Metrics[0].Rate = math.Inf(1) }},
		{"zero-rate", func(r *tuneResult) { r.Metrics[0].Rate = 0 }},
		{"negative-rate", func(r *tuneResult) { r.Metrics[0].Rate = -1 }},
		{"nan-native", func(r *tuneResult) { r.Metrics[0].Native = math.NaN() }},
		{"zero-native", func(r *tuneResult) { r.Metrics[0].Native = 0 }},
		{"zero-ratio", func(r *tuneResult) { r.Metrics[0].Ratio = 0 }},
		{"nan-ratio", func(r *tuneResult) { r.Metrics[0].Ratio = math.NaN() }},
		{"infinite-ratio", func(r *tuneResult) { r.Metrics[0].Ratio = math.Inf(1) }},
		{"cargo-zero", func(r *tuneResult) { r.Metrics[2].Rate = 0 }},
		{"cargo-partial-reference", func(r *tuneResult) { r.Metrics[2].Native = 100 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := []tuneResult{
				{Metrics: scoreTestMetrics(100, 100, 100)},
				{Metrics: scoreTestMetrics(150, 150, 150), Score: 9, Worst: 9, Eligible: true},
			}
			tc.change(&results[1])
			best, err := scoreTune(results, 0.20)
			if err != nil || best != 0 || results[1].Eligible || results[1].Reason == "" || results[1].Score != 0 {
				t.Fatalf("invalid candidate survived: best=%d err=%v result=%+v", best, err, results[1])
			}
		})
	}
}

func TestScoreTuneRequiresCompleteBaseline(t *testing.T) {
	for _, baseline := range []tuneResult{
		{},
		{Metrics: scoreTestMetrics(100, 100, 100), Error: "timed out"},
		{Metrics: []tuneMetric{scoreTestMetric("bad", "unknown", 100)}},
		{Metrics: scoreTestMetrics(math.NaN(), 100, 100)},
	} {
		results := []tuneResult{baseline, {Metrics: scoreTestMetrics(150, 150, 150), Eligible: true}}
		best, err := scoreTune(results, 0.20)
		if err == nil || best != -1 || results[0].Eligible || results[1].Eligible {
			t.Fatalf("invalid baseline accepted: best=%d err=%v results=%+v", best, err, results)
		}
	}
	if best, err := scoreTune(nil, 0.20); err == nil || best != -1 {
		t.Fatalf("empty results: best=%d err=%v", best, err)
	}
	for _, regression := range []float64{-0.1, 1.1, math.NaN(), math.Inf(1)} {
		if _, err := scoreTune([]tuneResult{{Metrics: scoreTestMetrics(100, 100, 100)}}, regression); err == nil {
			t.Fatalf("accepted invalid regression %v", regression)
		}
	}
}

func TestScoreTuneStableTiesAndNoise(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rates []float64
		best  int
	}{
		{"baseline-tie", []float64{100, 100}, 0},
		{"below-noise", []float64{100, 102.99}, 0},
		{"noise-boundary", []float64{100, 103}, 1},
		{"candidate-tie", []float64{100, 120, 120}, 1},
		{"highest-first", []float64{100, 121, 120}, 1},
		{"highest-last", []float64{100, 120, 121}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results := make([]tuneResult, len(tc.rates))
			for i, rate := range tc.rates {
				results[i].Metrics = scoreTestMetrics(rate, rate, rate)
			}
			for range 2 {
				best, err := scoreTune(results, 0.20)
				if err != nil || best != tc.best {
					t.Fatalf("best=%d want=%d err=%v", best, tc.best, err)
				}
			}
		})
	}
}

func TestMedianTuneIndependentFieldsAndCargo(t *testing.T) {
	samples := [][]tuneMetric{
		{scoreTestMetric("read", "io", 10), scoreTestMetric("build", "cargo", 4)},
		{scoreTestMetric("build", "cargo", 8), scoreTestMetric("read", "io", 30)},
		{scoreTestMetric("read", "io", 20), scoreTestMetric("build", "cargo", 6)},
	}
	samples[0][0].Native, samples[0][0].Ratio = 20, 0.5
	samples[1][1].Native, samples[1][1].Ratio = 10, 3
	samples[2][0].Native, samples[2][0].Ratio = 40, 0.5
	got, err := medianTune(samples)
	if err != nil {
		t.Fatal(err)
	}
	want := []tuneMetric{
		{Name: "read", Group: "io", Unit: "ops/s", Rate: 20, Native: 20, Ratio: 0.5},
		scoreTestMetric("build", "cargo", 6),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if samples[0][0].Rate != 10 || samples[1][0].Name != "build" {
		t.Fatal("median changed input samples")
	}
	got, err = medianTune(samples[:2])
	if err != nil || got[0].Rate != 20 || got[0].Native != 15 || got[0].Ratio != 1.75 || got[1].Rate != 6 {
		t.Fatalf("even median: %+v, err=%v", got, err)
	}
}

func TestMedianTunePreservesReportingDetails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details []string
		want    string
	}{
		{"absent", []string{"", ""}, ""},
		{"unchanged", []string{"native=link mounted=copy", "native=link mounted=copy"}, "native=link mounted=copy"},
		{
			"varied",
			[]string{"native=link mounted=link", "native=link mounted=copy", ""},
			`varied across samples: sample 1: "native=link mounted=link"; sample 2: "native=link mounted=copy"; sample 3: ""`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples := make([][]tuneMetric, len(tc.details))
			for i, details := range tc.details {
				samples[i] = scoreTestMetrics(100, 100, 100)
				samples[i][1].Details = details
			}
			got, err := medianTune(samples)
			if err != nil {
				t.Fatal(err)
			}
			if got[1].Details != tc.want {
				t.Fatalf("details=%q, want %q", got[1].Details, tc.want)
			}
			for i, details := range tc.details {
				if samples[i][1].Details != details {
					t.Fatal("median changed input details")
				}
			}
		})
	}
}

func TestScoreTuneIgnoresReportingDetails(t *testing.T) {
	results := []tuneResult{
		{Metrics: scoreTestMetrics(100, 100, 100)},
		{Metrics: scoreTestMetrics(125, 125, 125)},
	}
	results[0].Metrics[1].Details = "native=link mounted=link"
	results[1].Metrics[1].Details = "native=link mounted=copy"
	best, err := scoreTune(results, 0.20)
	if err != nil || best != 1 || !results[1].Eligible {
		t.Fatalf("reporting details changed eligibility: best=%d err=%v result=%+v", best, err, results[1])
	}
	scoreTestClose(t, results[1].Score, 1.25)
}

func TestMedianTuneRejectsIdentityMismatchAndInvalidSamples(t *testing.T) {
	first := scoreTestMetrics(100, 100, 100)
	for _, tc := range []struct {
		name   string
		change func([]tuneMetric) []tuneMetric
	}{
		{"missing", func(m []tuneMetric) []tuneMetric { return m[:2] }},
		{"empty", func(m []tuneMetric) []tuneMetric { return nil }},
		{"extra", func(m []tuneMetric) []tuneMetric { return append(m, scoreTestMetric("extra", "io", 100)) }},
		{"name", func(m []tuneMetric) []tuneMetric { m[0].Name = "other"; return m }},
		{"group", func(m []tuneMetric) []tuneMetric { m[0].Group = "metadata"; return m }},
		{"unit", func(m []tuneMetric) []tuneMetric { m[0].Unit = "bytes/s"; return m }},
		{"duplicate", func(m []tuneMetric) []tuneMetric { m[1] = m[0]; return m }},
		{"unknown-group", func(m []tuneMetric) []tuneMetric { m[0].Group = "unknown"; return m }},
		{"nan", func(m []tuneMetric) []tuneMetric { m[0].Rate = math.NaN(); return m }},
		{"zero", func(m []tuneMetric) []tuneMetric { m[0].Rate = 0; return m }},
		{"infinite-native", func(m []tuneMetric) []tuneMetric { m[0].Native = math.Inf(1); return m }},
		{"zero-native", func(m []tuneMetric) []tuneMetric { m[0].Native = 0; return m }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			second := tc.change(scoreTestMetrics(120, 120, 120))
			if got, err := medianTune([][]tuneMetric{first, second}); err == nil || got != nil {
				t.Fatalf("invalid samples accepted: got=%+v err=%v", got, err)
			}
		})
	}
	for _, samples := range [][][]tuneMetric{nil, {nil}, {scoreTestMetrics(0, 100, 100)}} {
		if _, err := medianTune(samples); err == nil {
			t.Fatal("accepted missing or invalid first round")
		}
	}
}
