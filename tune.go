package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/source"
)

// tuneCmd compares disposable mounts; only an explicit confirmation changes the
// user's configuration or restarts the live daemon.
type tuneCmd struct {
	Project       string        `help:"Cargo workspace to include in the trials (build scripts execute normally)."`
	Proto         []string      `help:"Candidate protocols, repeatable; the configured baseline is always measured." enum:"nfs,smb,fuse,winfsp"`
	Runs          int           `help:"I/O and metadata samples per configuration; warm Cargo repeats." default:"3"`
	Timeout       time.Duration `help:"Maximum time per configuration, excluding bounded cleanup." default:"2m"`
	MaxRegression float64       `help:"Maximum per-workload regression from baseline (0.20 = 20%)." default:"0.20"`
}

// tuneReport keeps the evidence and decision separate from application status.
type tuneReport struct {
	Baseline      tuneConfig   `json:"baseline"`
	Results       []tuneResult `json:"results"`
	Best          *tuneConfig  `json:"best,omitempty"`
	Applied       bool         `json:"applied"`
	Runs          int          `json:"runs"`
	Project       string       `json:"project,omitempty"`
	MaxRegression float64      `json:"max_regression"`
}

// Run measures safe disposable configurations, then asks before activation and persistence.
func (cmd *tuneCmd) Run(c *cli) (err error) {
	if cmd.Runs < 1 || cmd.Runs > 9 {
		return errors.New("tune: --runs must be between 1 and 9")
	}
	if cmd.Timeout <= 0 {
		return errors.New("tune: --timeout must be positive")
	}
	if math.IsNaN(cmd.MaxRegression) || math.IsInf(cmd.MaxRegression, 0) || cmd.MaxRegression < 0 || cmd.MaxRegression >= 1 {
		return errors.New("tune: --max-regression must be in [0,1)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	base, err := configuredTune(c)
	if err != nil {
		return err
	}
	before, err := tuneSettings(home)
	if err != nil {
		return err
	}
	profiles := tuneProfiles(base, cmd.Proto, cmd.Project != "")
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var project *tuneProject
	if cmd.Project != "" {
		project, err = prepareTuneProject(c.ctx, cmd.Project)
		if err != nil {
			return fmt.Errorf("tune Cargo: %w", err)
		}
	}
	state, err := filepath.Abs(c.state)
	if err != nil {
		return err
	}
	parent := filepath.Dir(state)
	if err = os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	root, err := os.MkdirTemp(parent, ".ovnfs-tune-")
	if err != nil {
		return err
	}
	retained := false
	defer func() {
		if !retained {
			err = errors.Join(err, os.RemoveAll(root))
		}
	}()
	empty, err := os.MkdirTemp("", "ovnfs-tune-source-")
	if err != nil {
		return err
	}
	defer func(path string) { err = errors.Join(err, os.RemoveAll(path)) }(empty)
	empty, err = filepath.EvalSymlinks(empty)
	if err != nil {
		return err
	}
	resolved, err := source.Resolve(empty)
	if err != nil {
		return err
	}
	if resolved != empty {
		return fmt.Errorf("tune: temporary source is inside %s; set TMPDIR outside a repository", resolved)
	}
	report := tuneReport{Baseline: profiles[0], Runs: cmd.Runs, Project: cmd.Project, MaxRegression: cmd.MaxRegression}
	if !c.json {
		fmt.Fprintf(stderr, "%s %d configurations; live mount stays unchanged during trials.\n", paint(stderr, styleAccent.Bold(true), "Tuning"), len(profiles))
		fmt.Fprintln(stderr, "Testing buffered I/O, artifact publication and metadata walks; fsync/security settings are not weakened.")
		if project == nil {
			fmt.Fprintln(stderr, "Use --project DIR to include real Cargo cold/no-op/incremental builds.")
		}
	}
	for i, profile := range profiles {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		if !c.json {
			fmt.Fprintf(stderr, "[%d/%d] %s\n", i+1, len(profiles), profile.name())
		}
		result, cleanupErr := tuneTrial(c.ctx, root, empty, home, exe, base, profile, project, cmd.Runs, cmd.Timeout)
		report.Results = append(report.Results, result)
		if cleanupErr != nil {
			retained = true
			return fmt.Errorf("tune stopped to avoid an abandoned mount: %w", cleanupErr)
		}
		if project != nil && project.Changed {
			return errors.New("Cargo source changed between tuning trials; no configuration was applied")
		}
		if result.Error != "" && !c.json {
			fmt.Fprintf(stderr, "  %s\n", paint(stderr, styleYellow, result.Error))
		}
	}
	best, rankErr := scoreTune(report.Results, cmd.MaxRegression)
	if rankErr == nil {
		chosen := report.Results[best].Config
		report.Best = &chosen
	}
	if c.json {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return err
		}
		return rankErr
	}
	printTuneReport(report)
	if rankErr != nil {
		return rankErr
	}
	fmt.Fprintf(stdout, "\nBest measured balanced configuration: %s\n", paint(stdout, styleBold, report.Best.name()))
	fmt.Fprintf(stdout, "No workload may regress more than %.0f%%; gains above 2x cannot dominate the score.\n", cmd.MaxRegression*100)
	fmt.Fprintln(stdout, "NFS has no application-fsync forwarding; SMB on macOS has no hard links or durable handles.")
	fmt.Fprintf(stdout, "Apply proto=%s, memory=%s, nfs_io_size=%s to %s and restart this daemon?\n", report.Best.Proto, report.Best.Memory, report.Best.NFSIOSize, filepath.Join(home, ".o2", "ovnfs.yml"))
	fmt.Fprintln(stdout, "A protocol or mount-option change forces remounting; finish active checkout jobs first.")
	confirmed, err := confirmTune(c.ctx, os.Stdin, stderr)
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Fprintln(stdout, "Not applied; live daemon and configuration unchanged.")
		return nil
	}
	if err := applyTune(c, home, before, *report.Best); err != nil {
		return err
	}
	fmt.Fprintln(stdout, paint(stdout, styleGreen, "Applied and restarted."))
	return nil
}

// configuredTune uses the actual serve parser, including user and environment
// precedence, instead of maintaining a second copy of daemon defaults.
func configuredTune(c *cli) (serveCmd, error) {
	var commands verbs
	parser, err := newCLIParser(c, []string{"serve"}, &commands)
	if err != nil {
		return serveCmd{}, err
	}
	if _, err = parser.Parse([]string{"serve"}); err != nil {
		return serveCmd{}, err
	}
	base := commands.Serve
	if base.NFSIOSize == 0 {
		base.NFSIOSize = byteSize(defaultNFSIOSize)
	}
	if base.NFSIOSize > 1<<20 {
		return base, errors.New("NFS I/O size must not exceed 1 MiB")
	}
	return base, (mountSettings{Atime: base.Atime, NFSIOSize: int(base.NFSIOSize)}).validate()
}

// tuneProfiles varies only measured knobs. Environment overrides constrain the
// search so a confirmed result can actually survive the next normal restart.
func tuneProfiles(base serveCmd, protocols []string, cargo bool) []tuneConfig {
	baseline := tuneConfig{Proto: base.Proto, Memory: base.Memory, NFSIOSize: base.NFSIOSize}
	out := []tuneConfig{baseline}
	if len(protocols) == 0 {
		switch runtime.GOOS {
		case "darwin":
			protocols = []string{"nfs", "smb"}
		case "windows":
			protocols = []string{"smb", "winfsp"}
		default:
			protocols = []string{"fuse", "nfs", "smb"}
		}
	}
	add := func(p tuneConfig) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, proto := range protocols {
		if proto == "nfs" && base.Fsync && base.Proto != "nfs" {
			continue // NFS cannot preserve an explicitly enabled fsync guarantee.
		}
		p := baseline
		p.Proto = proto
		add(p)
		if proto == "nfs" && os.Getenv("OVNFS_NFS_IO_SIZE") == "" {
			for _, size := range []byteSize{16 << 10, 64 << 10, 128 << 10, 256 << 10, 1 << 20} {
				p.NFSIOSize = size
				add(p)
			}
		}
		p.NFSIOSize = baseline.NFSIOSize
		if os.Getenv("OVNFS_MEMORY") == "" {
			p.Memory = 0
			add(p)
			if cargo {
				p.Memory = 256 << 20
				add(p)
			}
		}
	}
	return out
}

func printTuneReport(report tuneReport) {
	rows := [][]string{{"CONFIGURATION", "WORST / BASE", "BALANCED", "RESULT"}}
	for _, r := range report.Results {
		status := "eligible"
		if r.Error != "" {
			status = "failed"
		} else if !r.Eligible {
			status = r.Reason
		}
		rows = append(rows, []string{r.Config.name(), fmt.Sprintf("%.2fx", r.Worst), fmt.Sprintf("%.3f", r.Score), status})
	}
	table(stdout, rows)
	if report.Best == nil {
		return
	}
	var winner tuneResult
	for _, r := range report.Results {
		if r.Config == *report.Best {
			winner = r
			break
		}
	}
	metrics := [][]string{{"WORKLOAD", "BASELINE", "WINNER", "UNIT"}}
	index, _ := tuneMetricIndex(report.Results[0].Metrics)
	for _, m := range winner.Metrics {
		baseline := report.Results[0].Metrics[index[m.Name]]
		if m.Group == "cargo" {
			metrics = append(metrics, []string{m.Name, fmt.Sprintf("%.3f", 1/baseline.Rate), fmt.Sprintf("%.3f", 1/m.Rate), "seconds"})
		} else {
			metrics = append(metrics, []string{m.Name, fmt.Sprintf("%.1f", baseline.Rate), fmt.Sprintf("%.1f", m.Rate), m.Unit})
		}
	}
	fmt.Fprintln(stdout)
	table(stdout, metrics)
	for _, m := range winner.Metrics {
		if m.Details != "" {
			fmt.Fprintf(stdout, "%s: %s\n", m.Name, m.Details)
		}
	}
}

// confirmTune defaults to no on EOF or any answer other than y/yes. Cancellation
// does not wait for a terminal read to complete; JSON mode never calls it.
func confirmTune(ctx context.Context, input io.Reader, output io.Writer) (bool, error) {
	fmt.Fprint(output, "Apply? [y/N] ")
	type answer struct {
		line string
		err  error
	}
	done := make(chan answer, 1)
	go func() { line, err := bufio.NewReader(input).ReadString('\n'); done <- answer{line, err} }()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case a := <-done:
		if a.err != nil && !errors.Is(a.err, io.EOF) {
			return false, a.err
		}
		s := strings.TrimSpace(a.line)
		return strings.EqualFold(s, "y") || strings.EqualFold(s, "yes"), nil
	}
}
