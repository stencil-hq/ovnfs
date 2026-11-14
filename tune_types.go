package main

// tuneConfig contains only settings the tuner is allowed to change.
type tuneConfig struct {
	Proto     string   `json:"proto"`
	Memory    byteSize `json:"memory_bytes"`
	NFSIOSize byteSize `json:"nfs_io_size_bytes"`
}

func (p tuneConfig) name() string {
	if p.Proto == "nfs" {
		return p.Proto + " " + p.NFSIOSize.String() + " / " + p.Memory.String()
	}
	return p.Proto + " / " + p.Memory.String()
}

// tuneMetric is a completed rate; higher is better. Native is zero for Cargo,
// whose comparison reference is the configured baseline rather than a copied
// native checkout. Ratio is the per-sample mounted/native rate when available.
type tuneMetric struct {
	Name    string  `json:"name"`
	Group   string  `json:"group"`
	Unit    string  `json:"unit"`
	Details string  `json:"details,omitempty"`
	Rate    float64 `json:"rate"`
	Native  float64 `json:"native_rate,omitempty"`
	Ratio   float64 `json:"over_native,omitempty"`
}

// tuneResult retains failed trials for reporting but never ranks them as winners.
type tuneResult struct {
	Config   tuneConfig   `json:"config"`
	Metrics  []tuneMetric `json:"metrics,omitempty"`
	Error    string       `json:"error,omitempty"`
	Score    float64      `json:"balanced_score,omitempty"`
	Worst    float64      `json:"worst_over_baseline,omitempty"`
	Eligible bool         `json:"eligible"`
	Reason   string       `json:"reason,omitempty"`
}
