// Command buildbench times cargo-shaped build work inside ovnfs checkouts
// beside the same work in a plain host directory, and reports each daemon's
// memory, so daemon changes can be checked for IO-time regressions.
//
// Every -ovnfs binary gets a private daemon (temporary state, mount and HOME,
// so its default settings) opened on one shared fixture: a git repository
// holding a crate (the -project's files, or a synthetic one) plus -files bulk
// files that make the base realistically large, built with git fast-import in
// seconds. Each round of a workload runs its phases on every target in turn,
// starting one target later than the previous round so warm-up and drift
// favor none, and phases report the median:
//
//	io     cargo's filesystem traffic without compilers: a cold build writing
//	       fingerprints, dep-info, rmeta and rlib archives through rustc-style
//	       temporary directories, build-script outputs, codegen objects and a
//	       linked binary hard-linked into place; a no-op build re-reading
//	       fingerprints and dep-info and stat'ing outputs and sources; a
//	       relink after touching one source; a clean.
//	cargo  `cargo build --offline` of the crate: cold in the first -cold
//	       rounds (default 1, as rustc dominates it), then no-op and after a
//	       touch every round.
//
// macOS mounts NFS as the user; Linux needs root for NFS mounts.
//
// Example (old binary built at the previous revision):
//
//	go run ./scripts/buildbench -ovnfs /tmp/ovnfs-old -ovnfs /tmp/ovnfs-new -project /work/jegrep -modes io,cargo
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

func main() {
	var bins []string
	flag.Func("ovnfs", "ovnfs binary to measure (repeatable)", func(s string) error {
		abs, err := filepath.Abs(s)
		bins = append(bins, abs)
		return err
	})
	project := flag.String("project", "", "cargo project whose files form the fixture crate (default: a synthetic crate)")
	modes := flag.String("modes", "io", "comma-separated workloads: io, cargo")
	runs := flag.Int("runs", 3, "rounds per target; phases report the median")
	files := flag.Int("files", 100000, "bulk files in the fixture besides the crate")
	jobs := flag.Int("jobs", runtime.NumCPU(), "parallel units in io builds, like cargo -j")
	idle := flag.Duration("idle", 0, "pause before an extra idle memory sample (0: none)")
	stall := flag.Duration("stall", 30*time.Second, "phase duration past which the daemon's goroutines are saved (0: never)")
	cold := flag.Int("cold", 1, "rounds that start with a cold cargo build (cargo mode)")
	keep := flag.Bool("keep", false, "keep the temporary directory")
	flag.Parse()
	if len(bins) == 0 {
		fail(errors.New("at least one -ovnfs binary is required"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	b := &bench{runs: *runs, colds: *cold, jobs: *jobs, idle: *idle, stall: *stall}
	for m := range strings.SplitSeq(*modes, ",") {
		if m != "io" && m != "cargo" {
			fail(fmt.Errorf("unknown mode %q", m))
		}
		b.modes = append(b.modes, m)
	}
	// Unix socket paths are short, so the state lives below /tmp, not $TMPDIR.
	root, err := os.MkdirTemp("/tmp", "bb-")
	if err != nil {
		fail(err)
	}
	err = b.run(ctx, root, *project, *files, bins)
	if !*keep {
		os.RemoveAll(root)
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "buildbench:", err)
	os.Exit(1)
}

// bench holds one invocation's settings and its measurements.
type bench struct {
	modes   []string
	runs    int
	colds   int // rounds whose cold-only phases run
	jobs    int
	idle    time.Duration
	stall   time.Duration
	targets []*target
	plan    []unit
	sources []string // crate-relative source files rustc reads
	touch   string   // crate-relative source touched before relinking
	pattern []byte   // compressible bytes written as artifact content
}

// target is one directory the workloads run in: the host copy of the crate or
// an ovnfs checkout of the fixture.
type target struct {
	name    string
	dir     string
	bin     string   // ovnfs binary; "" for the host
	env     []string // ovnfs command environment
	pid     int
	capture time.Duration
	times   map[string][]time.Duration
	memory  [][2]string // label, value
}

func (b *bench) run(ctx context.Context, root, project string, files int, bins []string) error {
	fixture := filepath.Join(root, "fixture")
	start := time.Now()
	if err := b.makeFixture(fixture, project, files); err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	fmt.Fprintf(os.Stderr, "fixture: crate %s + %d bulk files in %.1fs\n", cmp(project, "synthetic"), files, time.Since(start).Seconds())
	host := &target{name: "host", dir: filepath.Join(root, "host"), times: map[string][]time.Duration{}}
	if err := copyCrate(fixture, host.dir); err != nil {
		return fmt.Errorf("host copy: %w", err)
	}
	b.targets = append(b.targets, host)
	defer b.stopDaemons()
	for i, bin := range bins {
		t, err := startCheckout(ctx, filepath.Join(root, strconv.Itoa(i)), bin, fixture)
		if t != nil {
			b.targets = append(b.targets, t)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", bin, err)
		}
		t.sampleMemory("after capture")
	}
	b.prepare()
	for _, mode := range b.modes {
		start := time.Now()
		for round := range b.runs {
			for i := range b.targets {
				t := b.targets[(round+i)%len(b.targets)]
				if err := b.round(ctx, t, mode, round); err != nil {
					return fmt.Errorf("%s: %w", t.label(), err)
				}
			}
		}
		for _, t := range b.targets {
			if err := os.RemoveAll(filepath.Join(t.dir, "target")); err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "%s: %d rounds in %.1fs\n", mode, b.runs, time.Since(start).Seconds())
	}
	for _, t := range b.targets[1:] {
		t.sampleMemory("after work")
	}
	if b.idle > 0 {
		time.Sleep(b.idle)
		for _, t := range b.targets[1:] {
			t.sampleMemory(fmt.Sprintf("idle %s", b.idle))
		}
	}
	b.report()
	return nil
}

func (t *target) label() string {
	if t.bin == "" {
		return t.name
	}
	return t.name + " (" + t.bin + ")"
}

// round runs mode's phases once in t, timing each; cold-only phases run in
// the first b.colds rounds, starting from a removed target directory, and
// later rounds build on what earlier ones left.
func (b *bench) round(ctx context.Context, t *target, mode string, round int) error {
	for _, p := range b.phases(ctx, mode) {
		if p.cold {
			if round >= b.colds {
				continue
			}
			if err := os.RemoveAll(filepath.Join(t.dir, "target")); err != nil {
				return err
			}
		}
		key := mode + " " + p.name
		start := time.Now()
		var watchdog *time.Timer
		if b.stall > 0 && t.bin != "" {
			watchdog = time.AfterFunc(b.stall, func() { t.saveGoroutines(key, round) })
		}
		err := p.run(t.dir)
		if watchdog != nil {
			watchdog.Stop()
		}
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		t.times[key] = append(t.times[key], time.Since(start))
	}
	return nil
}

// phase is one timed step of a workload, run in a crate directory.
type phase struct {
	name string
	cold bool // only in the first -cold rounds
	run  func(dir string) error
}

func (b *bench) phases(ctx context.Context, mode string) []phase {
	if mode == "io" {
		return b.ioPhases()
	}
	return b.cargoPhases(ctx)
}

func (b *bench) ioPhases() []phase {
	return []phase{
		{name: "cold", run: b.cold},
		{name: "no-op", run: b.noop},
		{name: "touch", run: func(dir string) error {
			if err := touch(filepath.Join(dir, b.touch)); err != nil {
				return err
			}
			return b.link(dir)
		}},
		{name: "clean", run: func(dir string) error { return os.RemoveAll(filepath.Join(dir, "target")) }},
	}
}

func (b *bench) cargoPhases(ctx context.Context) []phase {
	build := func(dir string) error {
		cmd := exec.CommandContext(ctx, "cargo", "build", "--offline")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("cargo build: %w\n%s", err, tail(out, 2000))
		}
		return nil
	}
	return []phase{
		{name: "cold", cold: true, run: build},
		{name: "no-op", run: build},
		{name: "touch", run: func(dir string) error {
			if err := touch(filepath.Join(dir, b.touch)); err != nil {
				return err
			}
			return build(dir)
		}},
	}
}

// report prints one row per phase: the host's median, then every checkout's
// median and its ratio to the host; then memory samples.
func (b *bench) report() {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	header := []string{"phase"}
	for _, t := range b.targets {
		header = append(header, t.label())
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))
	var keys []string
	for _, mode := range b.modes {
		for _, p := range b.phases(context.Background(), mode) {
			keys = append(keys, mode+" "+p.name)
		}
	}
	for _, key := range keys {
		row := []string{key}
		hostMedian := median(b.targets[0].times[key])
		for i, t := range b.targets {
			m := median(t.times[key])
			cell := fmt.Sprintf("%.3fs", m.Seconds())
			if i > 0 && hostMedian > 0 {
				cell += fmt.Sprintf(" (%.2fx)", float64(m)/float64(hostMedian))
			}
			if len(t.times[key]) > 1 {
				lo, hi := slices.Min(t.times[key]), slices.Max(t.times[key])
				cell += fmt.Sprintf(" [%.3f-%.3f]", lo.Seconds(), hi.Seconds())
			}
			row = append(row, cell)
		}
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	row := []string{"capture (ovnfs new)", "-"}
	for _, t := range b.targets[1:] {
		row = append(row, fmt.Sprintf("%.3fs", t.capture.Seconds()))
	}
	fmt.Fprintln(w, strings.Join(row, "\t"))
	if len(b.targets) > 1 {
		for i := range b.targets[1].memory {
			row := []string{b.targets[1].memory[i][0], "-"}
			for _, t := range b.targets[1:] {
				row = append(row, t.memory[i][1])
			}
			fmt.Fprintln(w, strings.Join(row, "\t"))
		}
	}
	w.Flush()
}

func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(ds))
	return s[len(s)/2]
}

// startCheckout starts a private daemon of bin below dir by opening a
// checkout of fixture, timing the capture.
func startCheckout(ctx context.Context, dir, bin, fixture string) (*target, error) {
	t := &target{name: filepath.Base(bin), bin: bin, times: map[string][]time.Duration{}}
	state, mount, home := filepath.Join(dir, "state"), filepath.Join(dir, "mount"), filepath.Join(dir, "home")
	for _, d := range []string{state, mount, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	t.env = append(os.Environ(), "OVNFS_STATE="+state, "OVNFS_MOUNT="+mount, "HOME="+home, "OVNFS_GIT_REMOTES=false")
	start := time.Now()
	out, err := t.ovnfs(ctx, "new", fixture, "--name", "bench", "--json")
	t.capture = time.Since(start)
	if err != nil {
		return t, err
	}
	var reply struct {
		Checkout struct {
			Path string `json:"path"`
		} `json:"checkout"`
	}
	if err := json.Unmarshal(lastJSON(out), &reply); err != nil || reply.Checkout.Path == "" {
		return t, fmt.Errorf("ovnfs new: no checkout path in %q", out)
	}
	t.dir = reply.Checkout.Path
	out, err = t.ovnfs(ctx, "status", "--json")
	if err != nil {
		return t, err
	}
	var st struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(lastJSON(out), &st); err != nil || st.PID == 0 {
		return t, fmt.Errorf("ovnfs status: no pid in %q", out)
	}
	t.pid = st.PID
	return t, nil
}

// lastJSON returns the last line of out that looks like a JSON object.
func lastJSON(out []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if bytes.HasPrefix(bytes.TrimSpace(lines[i]), []byte("{")) {
			return lines[i]
		}
	}
	return out
}

func (t *target) ovnfs(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, t.bin, args...)
	cmd.Env = t.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("ovnfs %s: %w\n%s", strings.Join(args, " "), err, tail(stderr.Bytes(), 2000))
	}
	return out, nil
}

// saveGoroutines writes the daemon's goroutine profile beside the temporary
// directory, which a run removes, while a phase is stalled.
func (t *target) saveGoroutines(phase string, round int) {
	out := filepath.Join(os.TempDir(), fmt.Sprintf("buildbench-stall-%s-%s-r%d.pb.gz", t.name, strings.ReplaceAll(phase, " ", "-"), round+1))
	if _, err := t.ovnfs(context.Background(), "profile", "goroutine", "-o", out); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s stalled in round %d; goroutine profile failed: %v\n", t.label(), phase, round+1, err)
		return
	}
	fmt.Fprintf(os.Stderr, "%s: %s stalled in round %d; goroutines: go tool pprof -traces %s\n", t.label(), phase, round+1, out)
}

func (b *bench) stopDaemons() {
	for _, t := range b.targets {
		if t.bin == "" {
			continue
		}
		if _, err := t.ovnfs(context.Background(), "stop"); err != nil {
			fmt.Fprintln(os.Stderr, "buildbench:", err)
		}
	}
}

var footprintLine = regexp.MustCompile(`(?m)^\s*(phys_footprint(?:_peak)?): (\d+) B`)

// sampleMemory records the daemon's footprint (macOS) or resident set
// (Linux), its peak, and the cache usage `ovnfs status` reports.
func (t *target) sampleMemory(label string) {
	add := func(name, value string) { t.memory = append(t.memory, [2]string{label + ": " + name, value}) }
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("footprint", "--noCategories", "-f", "bytes", strconv.Itoa(t.pid)).Output()
		values := map[string]string{"phys_footprint": "?", "phys_footprint_peak": "?"}
		if err == nil {
			for _, m := range footprintLine.FindAllStringSubmatch(string(out), -1) {
				n, _ := strconv.ParseInt(m[2], 10, 64)
				values[m[1]] = mib(n)
			}
		}
		add("footprint", values["phys_footprint"])
		add("footprint peak", values["phys_footprint_peak"])
	} else {
		status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", t.pid))
		for _, key := range []string{"VmRSS", "VmHWM"} {
			v := "?"
			if m := regexp.MustCompile(key + `:\s+(\d+) kB`).FindSubmatch(status); m != nil {
				n, _ := strconv.ParseInt(string(m[1]), 10, 64)
				v = mib(n << 10)
			}
			add(key, v)
		}
	}
	out, err := t.ovnfs(context.Background(), "status", "--json")
	var st map[string]any
	if err == nil && json.Unmarshal(lastJSON(out), &st) == nil {
		keys := slices.Sorted(func(yield func(string) bool) {
			for k := range st {
				if !yield(k) {
					return
				}
			}
		})
		var parts []string
		for _, k := range keys {
			if u, ok := st[k].(map[string]any); ok && u["used"] != nil {
				parts = append(parts, fmt.Sprintf("%s %s", k, mib(int64(u["used"].(float64)))))
			} else if strings.Contains(k, "heap") || strings.Contains(k, "footprint") {
				if n, ok := st[k].(float64); ok {
					parts = append(parts, fmt.Sprintf("%s %s", k, mib(int64(n))))
				}
			}
		}
		add("status", strings.Join(parts, ", "))
	}
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

// unit is one library crate of the replayed build.
type unit struct {
	name, hash, scriptHash, runHash string
	rlib, rmeta                     int
	script                          bool
	level                           int
	deps                            []int // earlier units whose rmeta rustc reads
}

// Shape of the replayed build, measured on a cold `cargo build` of a
// 149-package workspace: 82 rlibs (median 130 KB, p90 1.7 MB, max 7 MB), 29
// build scripts, 126 fingerprint directories, 256 codegen objects of the
// binary and a 24 MB linked executable.
const (
	units       = 82
	levels      = 8
	scriptEvery = 3
	binObjects  = 256
	binObject   = 160 << 10
	binSize     = 24 << 20
	binName     = "app"
	binHash     = "0f3c9a1b2d4e5f60"
)

// prepare fixes the replayed build's shape and content.
func (b *bench) prepare() {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range units {
		u := unit{
			name:       fmt.Sprintf("dep%03d", i),
			hash:       fmt.Sprintf("%016x", r.Uint64()),
			scriptHash: fmt.Sprintf("%016x", r.Uint64()),
			runHash:    fmt.Sprintf("%016x", r.Uint64()),
			script:     i%scriptEvery == 0,
			level:      i * levels / units,
		}
		size := math.Exp(math.Log(130e3) + 2.0*r.NormFloat64())
		u.rlib = int(min(max(size, 8<<10), 7<<20))
		u.rmeta = u.rlib / 3
		for range min(6, i) {
			if d := r.IntN(i); b.plan == nil || b.plan[d].level < u.level {
				u.deps = append(u.deps, d)
			}
		}
		b.plan = append(b.plan, u)
	}
	b.pattern = make([]byte, 8<<20)
	for off := 0; off < len(b.pattern); off += 4096 {
		block := b.pattern[off : off+4096]
		if off > 0 && r.IntN(5) < 3 {
			copy(block, b.pattern[r.IntN(off/4096)*4096:])
		} else {
			for i := range block {
				block[i] = byte(r.Uint32())
			}
		}
	}
}

// cold replays a full build in dir/target/debug.
func (b *bench) cold(dir string) error {
	t := filepath.Join(dir, "target", "debug")
	for _, d := range []string{"deps", "build", ".fingerprint", "incremental", "examples"} {
		if err := os.MkdirAll(filepath.Join(t, d), 0o755); err != nil {
			return err
		}
	}
	if err := b.write(filepath.Join(t, ".cargo-lock"), 0, 0o644); err != nil {
		return err
	}
	for level := range levels {
		var batch []unit
		for _, u := range b.plan {
			if u.level == level {
				batch = append(batch, u)
			}
		}
		if err := parallel(b.jobs, batch, func(u unit) error { return b.compile(t, u) }); err != nil {
			return err
		}
	}
	return b.link(dir)
}

// compile replays one library unit: its build script if any, reading its
// dependencies' metadata, then rustc's outputs and cargo's fingerprint.
func (b *bench) compile(t string, u unit) error {
	if u.script {
		scriptDir := filepath.Join(t, "build", u.name+"-"+u.scriptHash)
		bin := filepath.Join(scriptDir, "build_script_build-"+u.scriptHash)
		runDir := filepath.Join(t, "build", u.name+"-"+u.runHash)
		steps := []func() error{
			func() error { return os.MkdirAll(scriptDir, 0o755) },
			func() error { return b.write(bin+".d", 1<<10, 0o644) },
			func() error { return b.write(bin, 900<<10, 0o755) },
			func() error { return os.Link(bin, filepath.Join(scriptDir, "build-script-build")) },
			func() error { return b.fingerprint(t, "build-script-build-script-build", u.name+"-"+u.scriptHash) },
			func() error { return os.MkdirAll(filepath.Join(runDir, "out"), 0o755) },
			func() error { return b.write(filepath.Join(runDir, "out", "generated.rs"), 40<<10, 0o644) },
			func() error { return b.write(filepath.Join(runDir, "output"), 1<<10, 0o644) },
			func() error { return b.write(filepath.Join(runDir, "root-output"), 100, 0o644) },
			func() error { return b.write(filepath.Join(runDir, "stderr"), 0, 0o644) },
			func() error { return b.write(filepath.Join(runDir, "invoked.timestamp"), 0, 0o644) },
			func() error { return b.fingerprint(t, "run-build-script-build-script-build", u.name+"-"+u.runHash) },
		}
		for _, step := range steps {
			if err := step(); err != nil {
				return err
			}
		}
	}
	deps := filepath.Join(t, "deps")
	for _, d := range u.deps {
		dep := b.plan[d]
		if err := readAll(filepath.Join(deps, "lib"+dep.name+"-"+dep.hash+".rmeta")); err != nil {
			return err
		}
	}
	stem := u.name + "-" + u.hash
	if err := b.write(filepath.Join(deps, stem+".d"), 2<<10, 0o644); err != nil {
		return err
	}
	if err := b.viaTemp(deps, "rmeta"+u.hash[:6], "full.rmeta", filepath.Join(deps, "lib"+stem+".rmeta"), u.rmeta); err != nil {
		return err
	}
	var objects []string
	for k := range 4 {
		o := filepath.Join(deps, fmt.Sprintf("%s.%s.%d-cgu.0.rcgu.o", stem, u.name, k))
		if err := b.write(o, u.rlib/4, 0o644); err != nil {
			return err
		}
		objects = append(objects, o)
	}
	if err := b.viaTemp(deps, "rustc"+u.hash[6:12], "lib"+u.name+".rlib", filepath.Join(deps, "lib"+stem+".rlib"), u.rlib); err != nil {
		return err
	}
	for _, o := range objects {
		if err := os.Remove(o); err != nil {
			return err
		}
	}
	return b.fingerprint(t, "lib-"+u.name, stem)
}

// viaTemp writes size bytes as name inside a fresh temporary directory below
// dir, renames it to dst and removes the directory, as rustc emits outputs.
func (b *bench) viaTemp(dir, tmp, name, dst string, size int) error {
	tmpDir := filepath.Join(dir, tmp)
	if err := os.Mkdir(tmpDir, 0o755); err != nil {
		return err
	}
	if err := b.write(filepath.Join(tmpDir, name), size, 0o644); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(tmpDir, name), dst); err != nil {
		return err
	}
	return os.Remove(tmpDir)
}

// fingerprint writes cargo's fingerprint files for one unit.
func (b *bench) fingerprint(t, kind, stem string) error {
	fp := filepath.Join(t, ".fingerprint", stem)
	if err := os.MkdirAll(fp, 0o755); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		size int
	}{{"invoked.timestamp", 0}, {"dep-" + kind, 1 << 10}, {kind, 16}, {kind + ".json", 2 << 10}} {
		if err := b.write(filepath.Join(fp, f.name), f.size, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// link replays the binary crate: rustc reads the sources and direct
// dependencies' metadata and writes codegen objects; the linker reads every
// rlib and object, writes the executable, and cargo hard-links it into place.
func (b *bench) link(dir string) error {
	t := filepath.Join(dir, "target", "debug")
	deps := filepath.Join(t, "deps")
	for _, s := range b.sources {
		if err := readAll(filepath.Join(dir, s)); err != nil {
			return err
		}
	}
	for _, u := range b.plan[len(b.plan)-20:] {
		if err := readAll(filepath.Join(deps, "lib"+u.name+"-"+u.hash+".rmeta")); err != nil {
			return err
		}
	}
	stem := binName + "-" + binHash
	if err := b.write(filepath.Join(deps, stem+".d"), 5<<10, 0o644); err != nil {
		return err
	}
	objects := make([]int, binObjects)
	for i := range objects {
		objects[i] = i
	}
	err := parallel(b.jobs, objects, func(i int) error {
		return b.write(filepath.Join(deps, fmt.Sprintf("%s.%s.%04d.rcgu.o", stem, binName, i)), binObject, 0o644)
	})
	if err != nil {
		return err
	}
	for _, u := range b.plan {
		if err := readAll(filepath.Join(deps, "lib"+u.name+"-"+u.hash+".rlib")); err != nil {
			return err
		}
	}
	for i := range binObjects {
		if err := readAll(filepath.Join(deps, fmt.Sprintf("%s.%s.%04d.rcgu.o", stem, binName, i))); err != nil {
			return err
		}
	}
	exe := filepath.Join(deps, stem)
	if err := b.write(exe, binSize, 0o755); err != nil {
		return err
	}
	final := filepath.Join(t, binName)
	if err := os.Remove(final); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Link(exe, final); err != nil {
		return err
	}
	if err := b.write(final+".d", 1<<10, 0o644); err != nil {
		return err
	}
	return b.fingerprint(t, "bin-"+binName, stem)
}

// noop replays a no-op build's checks: every fingerprint and dep-info file
// read, every output and source stat'ed.
func (b *bench) noop(dir string) error {
	t := filepath.Join(dir, "target", "debug")
	err := filepath.WalkDir(filepath.Join(t, ".fingerprint"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return readAll(p)
	})
	if err != nil {
		return err
	}
	deps := filepath.Join(t, "deps")
	for _, u := range b.plan {
		stem := u.name + "-" + u.hash
		if err := readAll(filepath.Join(deps, stem+".d")); err != nil {
			return err
		}
		for _, p := range []string{"lib" + stem + ".rlib", "lib" + stem + ".rmeta"} {
			if _, err := os.Stat(filepath.Join(deps, p)); err != nil {
				return err
			}
		}
		if u.script {
			if err := readAll(filepath.Join(t, "build", u.name+"-"+u.runHash, "output")); err != nil {
				return err
			}
		}
	}
	for _, s := range b.sources {
		if _, err := os.Lstat(filepath.Join(dir, s)); err != nil {
			return err
		}
	}
	_, err = os.Stat(filepath.Join(t, binName))
	return err
}

// write creates path with size bytes of the pattern, in 64 KiB writes like a
// buffered writer.
func (b *bench) write(path string, size int, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	for off := 0; off < size; {
		n := min(64<<10, size-off)
		start := off % (len(b.pattern) - n)
		if _, err := f.Write(b.pattern[start : start+n]); err != nil {
			f.Close()
			return err
		}
		off += n
	}
	return f.Close()
}

var readBufs = sync.Pool{New: func() any { b := make([]byte, 256<<10); return &b }}

func readAll(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := readBufs.Get().(*[]byte)
	defer readBufs.Put(buf)
	_, err = io.CopyBuffer(io.Discard, struct{ io.Reader }{f}, *buf)
	return err
}

func touch(path string) error {
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// parallel runs fn over items with at most n at once, returning the first error.
func parallel[T any](n int, items []T, fn func(T) error) error {
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
		slots = make(chan struct{}, n)
	)
	for _, item := range items {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			if err := fn(item); err != nil {
				once.Do(func() { first = err })
			}
		})
	}
	wg.Wait()
	return first
}

// makeFixture builds the fixture repository in dir: the crate at its root
// (project's tracked and untracked-unignored files, or a synthetic crate) and
// bulk files below bulk/. One git fast-import stream commits everything and a
// parallel checkout-index writes the work tree with a clean index: seconds,
// where writing the files and running git add takes a minute per 100,000.
func (b *bench) makeFixture(dir, project string, files int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := git(dir, "init", "-q", "-b", "main"); err != nil {
		return err
	}
	imp := exec.Command("git", "fast-import", "--quiet")
	imp.Dir = dir
	var stderr bytes.Buffer
	imp.Stderr = &stderr
	in, err := imp.StdinPipe()
	if err != nil {
		return err
	}
	if err := imp.Start(); err != nil {
		return err
	}
	w := bufio.NewWriterSize(in, 1<<20)
	type entry struct {
		path, mode string
		mark       int
	}
	var entries []entry
	add := func(path, mode string, data []byte) {
		entries = append(entries, entry{path, mode, len(entries) + 1})
		fmt.Fprintf(w, "blob\nmark :%d\ndata %d\n", len(entries), len(data))
		w.Write(data)
		w.WriteByte('\n')
	}
	if project != "" {
		err = addProject(project, add)
	} else {
		addSyntheticCrate(add)
	}
	if err != nil {
		imp.Process.Kill()
		imp.Wait()
		return err
	}
	for i := range files {
		add(fmt.Sprintf("bulk/d%03d/f%05d.txt", i/250, i), "100644", fmt.Appendf(nil, "bulk file %d\n", i))
	}
	fmt.Fprintf(w, "commit refs/heads/main\ncommitter buildbench <buildbench@localhost> 0 +0000\ndata 7\nfixture\n")
	for _, e := range entries {
		path := e.path
		if strings.ContainsAny(path, "\n\"") {
			path = strconv.Quote(path)
		}
		fmt.Fprintf(w, "M %s :%d %s\n", e.mode, e.mark, path)
		if strings.HasPrefix(e.path, "src/") {
			b.sources = append(b.sources, filepath.FromSlash(e.path))
		}
	}
	if err := errors.Join(w.Flush(), in.Close()); err != nil {
		imp.Wait()
		return fmt.Errorf("git fast-import: %w\n%s", err, stderr.Bytes())
	}
	if err := imp.Wait(); err != nil {
		return fmt.Errorf("git fast-import: %w\n%s", err, stderr.Bytes())
	}
	if err := git(dir, "read-tree", "main"); err != nil {
		return err
	}
	if err := git(dir, "-c", "checkout.workers="+strconv.Itoa(runtime.NumCPU()), "checkout-index", "-a", "-u"); err != nil {
		return err
	}
	if len(b.sources) == 0 {
		return errors.New("crate has no files below src/")
	}
	slices.Sort(b.sources)
	b.touch = b.sources[0]
	for _, main := range []string{"src/main.rs", "src/lib.rs"} {
		if slices.Contains(b.sources, filepath.FromSlash(main)) {
			b.touch = filepath.FromSlash(main)
			break
		}
	}
	return nil
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", args[0], err, out)
	}
	return nil
}

// addProject passes project's tracked and untracked-unignored regular files
// and symlinks to add, with git's modes; submodules and deleted paths are skipped.
func addProject(project string, add func(path, mode string, data []byte)) error {
	out, err := exec.Command("git", "-C", project, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return fmt.Errorf("git ls-files %s: %w", project, err)
	}
	for rel := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		p := filepath.Join(project, rel)
		info, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			add(rel, "120000", []byte(target))
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			mode := "100644"
			if info.Mode()&0o111 != 0 {
				mode = "100755"
			}
			add(rel, mode, data)
		}
	}
	return nil
}

// copyCrate copies dir's crate, everything but bulk/ and .git, to dst.
func copyCrate(dir, dst string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "bulk" || rel == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		return copyEntry(p, filepath.Join(dst, rel))
	})
}

// copyEntry copies one regular file or symlink, skipping anything else.
func copyEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.Mode().IsRegular():
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, info.Mode().Perm())
	}
	return nil
}

// addSyntheticCrate passes a dependency-free binary crate of 100 modules to add.
func addSyntheticCrate(add func(path, mode string, data []byte)) {
	add("Cargo.toml", "100644", []byte("[package]\nname = \""+binName+"\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"))
	var main strings.Builder
	for i := range 100 {
		fmt.Fprintf(&main, "mod m%03d;\n", i)
		var m strings.Builder
		for j := range 40 {
			fmt.Fprintf(&m, "pub fn f%d(x: u64) -> u64 { x.wrapping_mul(%d).rotate_left(%d) ^ %d }\n", j, i*40+j+1, j%63, i)
		}
		add(fmt.Sprintf("src/m%03d.rs", i), "100644", []byte(m.String()))
	}
	main.WriteString("fn main() {\n    let mut x = 1u64;\n")
	for i := range 100 {
		fmt.Fprintf(&main, "    x = m%03d::f%d(x);\n", i, i%40)
	}
	main.WriteString("    println!(\"{x}\");\n}\n")
	add("src/main.rs", "100644", []byte(main.String()))
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

func cmp(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
