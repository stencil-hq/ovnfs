package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// scoreTune keeps reporting order intact. The first complete result is not a
// substitute for a failed baseline: every comparison must use results[0].
func scoreTune(results []tuneResult, maxRegression float64) (int, error) {
	for i := range results {
		results[i].Score = 0
		results[i].Worst = 0
		results[i].Eligible = false
		results[i].Reason = "baseline unavailable"
	}
	if len(results) == 0 {
		return -1, fmt.Errorf("no tuning results")
	}
	if math.IsNaN(maxRegression) || math.IsInf(maxRegression, 0) || maxRegression < 0 || maxRegression > 1 {
		return -1, fmt.Errorf("maximum regression must be between 0 and 1")
	}
	baseline := &results[0]
	if baseline.Error != "" {
		baseline.Reason = "trial failed: " + baseline.Error
		return -1, fmt.Errorf("baseline %s", baseline.Reason)
	}
	identity, err := tuneMetricIndex(baseline.Metrics)
	if err != nil {
		baseline.Reason = err.Error()
		return -1, fmt.Errorf("baseline: %w", err)
	}
	baseline.Score, baseline.Worst, baseline.Eligible, baseline.Reason = 1, 1, true, ""
	best := 0
	for i := 1; i < len(results); i++ {
		candidate := &results[i]
		if candidate.Error != "" {
			candidate.Reason = "trial failed: " + candidate.Error
			continue
		}
		index, err := tuneMetricIndex(candidate.Metrics)
		if err == nil {
			err = matchTuneMetrics(baseline.Metrics, candidate.Metrics, identity, index)
		}
		if err != nil {
			candidate.Reason = err.Error()
			continue
		}

		var sums [3]float64
		var counts [3]int
		worst, worstName := math.Inf(1), ""
		valid := true
		for _, reference := range baseline.Metrics {
			metric := candidate.Metrics[index[reference.Name]]
			relative := metric.Rate / reference.Rate
			if metric.Group != "cargo" {
				relative = metric.Ratio / reference.Ratio
			}
			if !positiveTuneValue(relative) {
				candidate.Reason = fmt.Sprintf("metric %q has an invalid relative rate", metric.Name)
				valid = false
				break
			}
			if relative < worst {
				worst, worstName = relative, metric.Name
			}
			group := tuneMetricGroup(metric.Group)
			sums[group] += math.Log(math.Min(relative, 2))
			counts[group]++
		}
		if !valid {
			continue
		}
		var sum float64
		groups := 0
		for group, count := range counts {
			if count != 0 {
				sum += sums[group] / float64(count)
				groups++
			}
		}
		// Cap the weakest rate for scoring too, while reporting its actual
		// value. Otherwise uniformly extreme gains could bypass the 2x cap.
		candidate.Worst = worst
		candidate.Score = math.Exp((math.Log(math.Min(worst, 2)) + sum/float64(groups)) / 2)
		if worst < 1-maxRegression {
			candidate.Reason = fmt.Sprintf("metric %q regressed %.1f%% (maximum %.1f%%)", worstName, (1-worst)*100, maxRegression*100)
			continue
		}
		candidate.Eligible = true
		candidate.Reason = ""
		// Compare the noise floor to the configured baseline, not to the
		// preceding candidate, so candidate order cannot change the winner.
		if candidate.Score >= 1.03 && candidate.Score > results[best].Score {
			best = i
		}
	}
	return best, nil
}

// medianTune matches metrics by identity rather than round order and preserves
// the first round's order. Ratio is measured per round; it is not recomputed
// from independently aggregated mounted and native rates.
func medianTune(samples [][]tuneMetric) ([]tuneMetric, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("no tuning samples")
	}
	indexes := make([]map[string]int, len(samples))
	for round, metrics := range samples {
		index, err := tuneMetricIndex(metrics)
		if err == nil && round != 0 {
			err = matchTuneMetrics(samples[0], metrics, indexes[0], index)
		}
		if err != nil {
			return nil, fmt.Errorf("sample %d: %w", round+1, err)
		}
		indexes[round] = index
	}
	medians := make([]tuneMetric, len(samples[0]))
	values := make([]float64, len(samples))
	for i, metric := range samples[0] {
		medians[i] = metric
		for round, metrics := range samples {
			if metrics[indexes[round][metric.Name]].Details == metric.Details {
				continue
			}
			var details strings.Builder
			details.WriteString("varied across samples: ")
			for sample, measurements := range samples {
				if sample != 0 {
					details.WriteString("; ")
				}
				fmt.Fprintf(&details, "sample %d: %q", sample+1, measurements[indexes[sample][metric.Name]].Details)
			}
			medians[i].Details = details.String()
			break
		}
		for field := range 3 {
			for round, metrics := range samples {
				m := metrics[indexes[round][metric.Name]]
				switch field {
				case 0:
					values[round] = m.Rate
				case 1:
					values[round] = m.Native
				case 2:
					values[round] = m.Ratio
				}
			}
			sort.Float64s(values)
			middle := len(values) / 2
			value := values[middle]
			if len(values)%2 == 0 {
				// Avoid overflow when both finite rates are near MaxFloat64.
				value = values[middle-1] + (value-values[middle-1])/2
			}
			switch field {
			case 0:
				medians[i].Rate = value
			case 1:
				medians[i].Native = value
			case 2:
				medians[i].Ratio = value
			}
		}
	}
	return medians, nil
}

func tuneMetricGroup(group string) int {
	switch group {
	case "io":
		return 0
	case "metadata":
		return 1
	case "cargo":
		return 2
	default:
		return -1
	}
}

func positiveTuneValue(value float64) bool {
	return value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}

func tuneMetricIndex(metrics []tuneMetric) (map[string]int, error) {
	if len(metrics) == 0 {
		return nil, fmt.Errorf("no completed metrics")
	}
	index := make(map[string]int, len(metrics))
	for i, metric := range metrics {
		if metric.Name == "" {
			return nil, fmt.Errorf("metric %d has no name", i+1)
		}
		if _, exists := index[metric.Name]; exists {
			return nil, fmt.Errorf("duplicate metric %q", metric.Name)
		}
		if tuneMetricGroup(metric.Group) < 0 {
			return nil, fmt.Errorf("metric %q has unknown group %q", metric.Name, metric.Group)
		}
		if !positiveTuneValue(metric.Rate) {
			return nil, fmt.Errorf("metric %q has an invalid rate", metric.Name)
		}
		// Cargo has no native checkout reference. Other groups must have a
		// completed native measurement as well as the mounted measurement.
		noReference := metric.Group == "cargo" && metric.Native == 0 && metric.Ratio == 0
		if !noReference && (!positiveTuneValue(metric.Native) || !positiveTuneValue(metric.Ratio)) {
			return nil, fmt.Errorf("metric %q has an invalid native reference", metric.Name)
		}
		index[metric.Name] = i
	}
	return index, nil
}

func matchTuneMetrics(reference, metrics []tuneMetric, referenceIndex, index map[string]int) error {
	for _, metric := range reference {
		i, ok := index[metric.Name]
		if !ok {
			return fmt.Errorf("missing metric %q", metric.Name)
		}
		if metric.Group != metrics[i].Group || metric.Unit != metrics[i].Unit {
			return fmt.Errorf("metric %q changed group or unit", metric.Name)
		}
	}
	for _, metric := range metrics {
		if _, ok := referenceIndex[metric.Name]; !ok {
			return fmt.Errorf("unexpected metric %q", metric.Name)
		}
	}
	return nil
}
