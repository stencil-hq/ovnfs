package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/source"
)

const (
	benchFileSize  = 32 << 20
	benchBlockSize = 1 << 20
	benchSample    = 750 * time.Millisecond
	benchBudget    = 23 * time.Second
	benchCleanup   = 3 * time.Second
)

// benchCmd compares native files with new files in a disposable mounted checkout.
type benchCmd struct {
	Dir string `arg:"" optional:"" name:"DIR" help:"disk scratch directory (default: daemon state, beside its backing files)"`
}

// Run measures both filesystems without modifying existing files or checkouts.
func (cmd *benchCmd) Run(c *cli) (err error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(c.ctx, benchBudget)
	defer cancel()
	client := *c
	client.ctx, client.json = ctx, false
	defer client.close()
	if _, err := client.client(); err != nil {
		return fmt.Errorf("bench: %w", err)
	}
	dir, err := filepath.EvalSymlinks(orDefault(cmd.Dir, c.state))
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	disk, err := os.MkdirTemp(dir, "ovnfs-bench-")
	if err != nil {
		return fmt.Errorf("bench: disk directory: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(disk)) }()
	base, err := os.MkdirTemp("", "ovnfs-bench-source-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(base)) }()
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	// A temporary directory inside a repository would capture that entire repo.
	root, err := source.Resolve(resolvedBase)
	if err != nil {
		return err
	}
	if root != resolvedBase {
		return fmt.Errorf("bench: temporary directory is inside %s; set TMPDIR outside a repository", root)
	}
	raw, callErr := client.call("new", api.NewArgs{Dir: resolvedBase, Name: filepath.Base(disk)})
	created, err := decodeReply[api.NewReply]("new", raw, callErr)
	if err != nil {
		return fmt.Errorf("bench: creating checkout: %w", err)
	}
	defer func() {
		// Cleanup survives interruption, but cannot consume the benchmark's budget.
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), benchCleanup)
		defer stop()
		client.ctx = cleanup
		if _, removeErr := client.call("rm", api.RmArgs{Checkout: created.Checkout.Name}); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("bench: cleanup failed; run ovnfs rm %s: %w", created.Checkout.Name, removeErr))
		}
	}()
	mount := filepath.Dir(created.Checkout.Path)
	if rel, relErr := filepath.Rel(mount, disk); relErr == nil && filepath.IsLocal(rel) {
		return errors.New("bench: disk directory must be outside the ovnfs mount")
	}
	if !c.json {
		fmt.Fprintf(stderr, "%s %s\n", paint(stderr, styleAccent.Bold(true), "Benchmarking disk and ovnfs"),
			paint(stderr, styleDim, "(about 10s; 26s budget including cleanup)..."))
	}
	// Kernel filesystem calls cannot be canceled by a Go context. A child keeps
	// a stalled mount from holding the CLI past its deadline.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	worker := exec.CommandContext(ctx, exe, "--state", c.state, "--mount", c.mount, "bench-io", disk, created.Checkout.Path)
	worker.Stderr = stderr
	worker.WaitDelay = 100 * time.Millisecond
	out, err := worker.Output()
	if ctx.Err() != nil {
		return fmt.Errorf("bench: exceeded time budget or interrupted: %w", ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("bench: I/O worker: %w", err)
	}
	var results []benchRow
	if err := json.Unmarshal(out, &results); err != nil {
		return fmt.Errorf("bench: decoding measurements: %w", err)
	}
	report := benchReport{
		Disk: dir, State: c.state, Mount: mount,
		Mode:      "buffered, OS cache enabled, new files, QD1, final fsync included",
		FileBytes: benchFileSize, SampleSeconds: benchSample.Seconds(),
		Seconds: time.Since(started).Seconds(), Results: results,
	}
	if c.json {
		return json.NewEncoder(stdout).Encode(report)
	}
	fmt.Fprintf(stdout, "%s %s\n%s %s\n%s %s\n",
		paint(stdout, styleBold, "Disk:"), paint(stdout, styleDim, report.Disk),
		paint(stdout, styleBold, "Backing state:"), paint(stdout, styleDim, report.State),
		paint(stdout, styleBold, "Mount:"), paint(stdout, styleDim, report.Mount))
	fmt.Fprintln(stdout, paint(stdout, styleDim, "32 MiB files · single thread / QD1 · buffered I/O (OS cache enabled)"))
	fmt.Fprintln(stdout, paint(stdout, styleDim, "Reads use newly written files; writes include a final fsync. Not raw device speeds."))
	header := []string{"WORKLOAD", "DISK MiB/s", "OVNFS MiB/s", "OVNFS / DISK", "DISK IOPS", "OVNFS IOPS"}
	for i, label := range header {
		header[i] = paint(stdout, styleBold, label)
	}
	rows := [][]string{header}
	for _, r := range results {
		ratioStyle := styleYellow
		if r.Ratio >= 1 {
			ratioStyle = styleGreen
		}
		rows = append(rows, []string{paint(stdout, styleAccent, r.Name),
			paint(stdout, styleDim, fmt.Sprintf("%.1f", r.Disk.MiBPS)),
			paint(stdout, styleBold, fmt.Sprintf("%.1f", r.Ovnfs.MiBPS)),
			paint(stdout, ratioStyle, fmt.Sprintf("%.2fx", r.Ratio)),
			paint(stdout, styleDim, fmt.Sprintf("%.0f", r.Disk.IOPS)),
			paint(stdout, styleBold, fmt.Sprintf("%.0f", r.Ovnfs.IOPS))})
	}
	table(stdout, rows)
	fmt.Fprintln(stdout, paint(stdout, styleDim, fmt.Sprintf("Measured in %.1fs; higher is better.", report.Seconds)))
	return nil
}

type benchReport struct {
	Mode          string     `json:"mode"`
	Disk          string     `json:"disk"`
	State         string     `json:"state"`
	Mount         string     `json:"mount"`
	FileBytes     int        `json:"file_bytes"`
	SampleSeconds float64    `json:"sample_seconds"`
	Seconds       float64    `json:"seconds"`
	Results       []benchRow `json:"results"`
}

type benchRow struct {
	Name       string           `json:"name"`
	BlockBytes int              `json:"block_bytes"`
	Disk       benchMeasurement `json:"disk"`
	Ovnfs      benchMeasurement `json:"ovnfs"`
	Ratio      float64          `json:"ovnfs_over_disk"`
}

type benchMeasurement struct {
	Bytes      int64   `json:"bytes"`
	Operations int64   `json:"operations"`
	Seconds    float64 `json:"seconds"`
	MiBPS      float64 `json:"mib_per_second"`
	IOPS       float64 `json:"iops"`
}

type benchWorkload struct {
	name   string
	block  int
	write  bool
	random bool
}

// benchIOCmd is the killable I/O worker; only bench invokes this hidden verb.
type benchIOCmd struct {
	Disk  string `arg:""`
	Ovnfs string `arg:""`
}

// Run prepares identical files, alternates which filesystem runs first, and emits measurements.
func (cmd *benchIOCmd) Run(c *cli) error {
	results, err := measureBenchFiles(c.ctx, cmd.Disk, cmd.Ovnfs)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(results)
}

// measureBenchFiles is shared by bench and tune's killable workers.
func measureBenchFiles(ctx context.Context, disk, ovnfs string) (results []benchRow, err error) {
	data := make([]byte, benchFileSize)
	rng := rand.NewChaCha8([32]byte{1})
	if _, err := rng.Read(data); err != nil {
		return nil, err
	}
	var files [2]*os.File
	for i, dir := range []string{disk, ovnfs} {
		f, openErr := os.OpenFile(filepath.Join(dir, "io.bin"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if openErr != nil {
			return nil, openErr
		}
		defer func() { err = errors.Join(err, f.Close()) }()
		files[i] = f
		for offset := 0; offset < len(data); offset += benchBlockSize {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if _, err := f.Write(data[offset : offset+benchBlockSize]); err != nil {
				return nil, err
			}
		}
		if err := f.Sync(); err != nil {
			return nil, err
		}
	}
	buffer := make([]byte, benchBlockSize)
	workloads := []benchWorkload{
		{name: "Sequential read (1 MiB)", block: benchBlockSize},
		{name: "Sequential write (1 MiB)", block: benchBlockSize, write: true},
		{name: "Random read (4 KiB)", block: 4096, random: true},
		{name: "Random write (4 KiB)", block: 4096, write: true, random: true},
	}
	results = make([]benchRow, 0, len(workloads))
	for i, workload := range workloads {
		var measurements [2]benchMeasurement
		for _, side := range []int{i % 2, 1 - i%2} {
			measurements[side], err = measureBench(ctx, files[side], workload, data, buffer, benchSample)
			if err != nil {
				return nil, fmt.Errorf("%s on %s: %w", workload.name, files[side].Name(), err)
			}
		}
		results = append(results, benchRow{
			Name: workload.name, BlockBytes: workload.block,
			Disk: measurements[0], Ovnfs: measurements[1], Ratio: measurements[1].MiBPS / measurements[0].MiBPS,
		})
	}
	return results, nil
}

// measureBench wraps within a fixed-size file; writes preserve its seeded bytes.
func measureBench(ctx context.Context, f *os.File, w benchWorkload, data, buffer []byte, duration time.Duration) (benchMeasurement, error) {
	rng := rand.New(rand.NewPCG(1, 2))
	var m benchMeasurement
	started := time.Now()
	deadline := started.Add(duration)
	for offset := 0; time.Now().Before(deadline); {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		if w.random {
			offset = rng.IntN(len(data)/w.block) * w.block
		}
		var n int
		var err error
		if w.write {
			n, err = f.WriteAt(data[offset:offset+w.block], int64(offset))
		} else {
			n, err = f.ReadAt(buffer[:w.block], int64(offset))
		}
		if err != nil {
			return m, err
		}
		if n != w.block {
			return m, io.ErrUnexpectedEOF
		}
		m.Bytes += int64(n)
		m.Operations++
		offset = (offset + w.block) % len(data)
	}
	if w.write {
		if err := f.Sync(); err != nil {
			return m, err
		}
	}
	if err := ctx.Err(); err != nil {
		return m, err
	}
	m.Seconds = time.Since(started).Seconds()
	m.MiBPS = float64(m.Bytes) / (1 << 20) / m.Seconds
	m.IOPS = float64(m.Operations) / m.Seconds
	return m, nil
}
