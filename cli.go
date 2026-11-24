package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
)

// cli carries the resolved options and the lazily dialed daemon connection of one invocation.
type cli struct {
	ctx       context.Context
	state     string
	mount     string
	json      bool
	color     string // --color: auto, always or never
	autostart bool   // start the daemon when its socket does not accept connections
	conn      *admin.Client
}

// call sends one operation and returns the raw reply, starting the daemon if allowed.
func (c *cli) call(op string, args any) (json.RawMessage, error) {
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	return client.Call(c.ctx, op, args)
}

// close releases the daemon connection, if one was made.
func (c *cli) close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

// call performs op, prints the raw reply under --json, and decodes it for human output.
func call[T any](c *cli, op string, args any) (T, error) {
	raw, err := c.call(op, args)
	if err == nil && c.json {
		fmt.Println(string(raw))
	}
	return decodeReply[T](op, raw, err)
}

// decodeReply decodes the raw reply to op unless the call failed.
func decodeReply[T any](op string, raw json.RawMessage, err error) (T, error) {
	var reply T
	if err != nil {
		return reply, err
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return reply, fmt.Errorf("%s: decoding reply: %w", op, err)
	}
	return reply, nil
}

// lsCmd lists checkouts, those of a folder or picked by flags, or the snapshots
// of one checkout. A path spelling (".", "..", a slash) makes the argument a
// folder, so a checkout named after a folder beside it stays reachable by name.
type lsCmd struct {
	Target string `arg:"" optional:"" name:"CHECKOUT|DIR" help:"checkout, or a folder (. or a path with /) whose checkouts to list" completion-predictor:"checkout"`
	selectFlags
}

func (cmd *lsCmd) Run(c *cli) error {
	a := api.LsArgs{Select: cmd.selectFlags.args()}
	name := ""
	switch {
	case cmd.Target != "" && !pathArg(cmd.Target):
		name = cmd.Target
		if a.Select != (api.Select{}) {
			return fmt.Errorf("ls: --idle and --no-diff pick checkouts, not snapshots of %s", name)
		}
	case cmd.Target != "":
		dir, err := filepath.Abs(cmd.Target)
		if err != nil {
			return fmt.Errorf("ls: resolving %s: %w", cmd.Target, err)
		}
		a.Dir = dir
	}
	if name == "" {
		r, err := call[api.LsReply](c, "ls", a)
		if err == nil && !c.json {
			printCheckouts(stdout, r)
		}
		return err
	}
	raw, callErr := c.call("ls", a)
	r, err := decodeReply[api.LsReply]("ls", raw, callErr)
	if err != nil {
		return err
	}
	r = scopeLsReply(r, name)
	if len(r.Checkouts) == 0 && len(r.Snapshots) == 0 {
		if isDirArg(name) {
			return fmt.Errorf("ls: no checkout %s; for the folder, ls ./%s", name, name)
		}
		return fmt.Errorf("ls: no checkout %s", name)
	}
	if c.json {
		out, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("ls: encoding reply: %w", err)
		}
		fmt.Println(string(out))
		return nil
	}
	if len(r.Snapshots) == 0 {
		return nil
	}
	rows := [][]string{{paint(stdout, styleBold, "ID"), paint(stdout, styleBold, "LABEL"), paint(stdout, styleBold, "CREATED"), paint(stdout, styleBold, "TREE")}}
	for _, s := range r.Snapshots {
		rows = append(rows, []string{paint(stdout, styleAccent, s.ID), s.Label, paint(stdout, styleDim, stamp(s.Created)), treeCell(stdout, s.Tree)})
	}
	table(stdout, rows)
	return nil
}

// scopeLsReply lists snapshots of the current checkout or, when its name is
// no longer in use, retained snapshots made under that name.
func scopeLsReply(r api.LsReply, name string) api.LsReply {
	out := api.LsReply{Checkouts: []api.Checkout{}, Snapshots: []api.Snapshot{}}
	for _, h := range r.Checkouts {
		if h.Name == name {
			out.Checkouts = append(out.Checkouts, h)
		}
	}
	for _, s := range r.Snapshots {
		if s.Checkout == name && (!s.Orphan || len(out.Checkouts) == 0) {
			out.Snapshots = append(out.Snapshots, s)
		}
	}
	return out
}

// printCheckouts lists live checkouts and retained snapshots without a live owner.
func printCheckouts(w io.Writer, r api.LsReply) {
	snaps := make(map[string]int)
	live := make(map[string]bool)
	for _, s := range r.Snapshots {
		if !s.Orphan {
			snaps[s.Checkout]++
		}
	}
	if len(r.Checkouts) > 0 {
		rows := [][]string{{paint(w, styleBold, "CHECKOUT"), paint(w, styleBold, "SOURCE"), paint(w, styleBold, "SNAPS"), paint(w, styleBold, "CHANGED"), paint(w, styleBold, "TOUCHED"), paint(w, styleBold, "PATH")}}
		for _, h := range r.Checkouts {
			live[h.Name] = true
			rows = append(rows, []string{
				paint(w, styleAccent.Bold(true), h.Name),
				paint(w, styleDim, h.Source),
				fmt.Sprint(snaps[h.Name]),
				paintIf(w, h.Dirty, styleYellow, paintIf(w, !h.Dirty, styleGreen, yesNo(h.Dirty))),
				paint(w, styleDim, stamp(h.Touched)),
				h.Path,
			})
		}
		table(w, rows)
	}
	var orphans [][]string
	for _, s := range r.Snapshots {
		if !s.Orphan && live[s.Checkout] {
			continue
		}
		if len(orphans) == 0 {
			orphans = append(orphans, []string{paint(w, styleBold, "ORPHAN SNAPSHOT"), paint(w, styleBold, "CHECKOUT"), paint(w, styleBold, "LABEL"), paint(w, styleBold, "CREATED"), paint(w, styleBold, "TREE")})
		}
		orphans = append(orphans, []string{paint(w, styleAccent, s.ID), paint(w, styleAccent, s.Checkout), s.Label, paint(w, styleDim, stamp(s.Created)), treeCell(w, s.Tree)})
	}
	if len(orphans) > 0 {
		if len(r.Checkouts) > 0 {
			fmt.Fprintln(w)
		}
		table(w, orphans)
	}
}

// treeCell renders a snapshot's content id: abbreviated and dim, or a placeholder
// while the daemon hashes it.
func treeCell(w io.Writer, tree string) string {
	if tree == "" {
		return paint(w, styleDim, "hashing…")
	}
	return paint(w, styleDim, abbrev(tree, 12))
}

// selectFlags are the options ls and prune pick checkouts by (see api.Select);
// kong flattens the embedded struct into their flags.
type selectFlags struct {
	Idle   seconds `name:"idle" help:"only checkouts untouched for this long: 90m, 12h, 7d, 2w"`
	NoDiff bool    `name:"no-diff" help:"only checkouts without source changes since their base (an empty diff)"`
}

// args converts the flags to api.Select.
func (s selectFlags) args() api.Select {
	return api.Select{Idle: float64(s.Idle), NoDiff: s.NoDiff}
}

// seconds is a duration option held in seconds, as api fields are; besides Go
// durations (90m, 12h) it takes whole days and weeks (7d, 2w).
type seconds float64

// String renders the duration for help text.
func (s seconds) String() string { return time.Duration(float64(s) * float64(time.Second)).String() }

// UnmarshalText parses one duration, as kong hands a flag's value over.
func (s *seconds) UnmarshalText(text []byte) error {
	v := string(text)
	d, err := time.ParseDuration(v)
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(v, suffix); ok {
			var whole uint64
			whole, err = strconv.ParseUint(n, 10, 12) // up to 4095 weeks, within a Duration
			d = time.Duration(whole) * unit
		}
	}
	if err != nil || d < 0 {
		return fmt.Errorf("invalid duration %q", v)
	}
	*s = seconds(d.Seconds())
	return nil
}

// pruneCmd removes the checkouts a folder and flags pick and prints their names.
type pruneCmd struct {
	Target          string `arg:"" optional:"" name:"DIR" help:"folder whose checkouts to remove"`
	StickySnapshots bool   `name:"sticky-snapshots" help:"retain snapshots of removed checkouts"`
	DryRun          bool   `name:"dry-run" help:"print what would be removed; remove nothing"`
	selectFlags
}

func (cmd *pruneCmd) Run(c *cli) error {
	a := api.PruneArgs{Select: cmd.selectFlags.args(), StickySnapshots: cmd.StickySnapshots, DryRun: cmd.DryRun}
	if cmd.Target != "" {
		dir, err := filepath.Abs(cmd.Target)
		if err != nil {
			return fmt.Errorf("prune: resolving %s: %w", cmd.Target, err)
		}
		a.Dir = dir
	}
	if a.Select == (api.Select{}) {
		return errors.New("prune: pick checkouts by DIR, --idle or --no-diff")
	}
	r, err := call[api.PruneReply](c, "prune", a)
	if err != nil || c.json {
		return err
	}
	for _, h := range r.Removed {
		fmt.Fprintln(stdout, h.Name)
	}
	switch n := plural(len(r.Removed), "checkout", "checkouts"); {
	case len(r.Removed) == 0:
		fmt.Fprintln(stderr, paint(stderr, styleYellow, "no checkout matches"))
	case a.DryRun:
		fmt.Fprintf(stderr, "%s %s\n", paint(stderr, styleYellow, "would remove"), n)
	case a.StickySnapshots:
		fmt.Fprintf(stderr, "%s %s; snapshots retained\n", paint(stderr, styleGreen, "removed"), n)
	default:
		fmt.Fprintf(stderr, "%s %s and their snapshots\n", paint(stderr, styleGreen, "removed"), n)
	}
	return nil
}

// newCmd opens a checkout on a folder, a snapshot or a capture and prints its path.
type newCmd struct {
	Dir      string   `arg:"" optional:"" name:"DIR" help:"folder to open the checkout on (default .)"`
	Name     string   `name:"name" help:"checkout name (default: the folder's name)"`
	Snapshot string   `name:"from" help:"open on this snapshot instead of a folder" completion-predictor:"snapshot"`
	Base     string   `name:"base" help:"open on what this checkout (or capture id) started from, untouched: a baseline to compare it with" completion-predictor:"checkout"`
	Rev      string   `name:"rev" help:"start at this git commit or local branch; keep ignored files"`
	Take     []string `name:"take" sep:"none" help:"replace source path from working tree (.), index (empty), or revision (repeatable)"`
	Rules    []string `name:"rule" sep:"none" help:"extra rule line (repeatable)"`
}

func (cmd *newCmd) Run(c *cli) error {
	a := api.NewArgs{Name: cmd.Name, Snapshot: cmd.Snapshot, Base: cmd.Base, Rev: cmd.Rev, Take: cmd.Take, Rules: cmd.Rules}
	switch {
	case a.Snapshot != "" && a.Base != "":
		return errors.New("new: give either --from SNAP or --base CHECKOUT, not both")
	case a.Snapshot != "" || a.Base != "":
		if a.Rev != "" {
			return errors.New("new: --rev needs a folder")
		}
		if len(a.Take) != 0 {
			return errors.New("new: --take needs a folder")
		}
		if cmd.Dir != "" {
			return errors.New("new: give either DIR, --from SNAP or --base CHECKOUT")
		}
	default:
		dir, err := filepath.Abs(orDefault(cmd.Dir, "."))
		if err != nil {
			return fmt.Errorf("new: resolving %s: %w", orDefault(cmd.Dir, "."), err)
		}
		a.Dir = dir
	}
	r, err := call[api.NewReply](c, "new", a)
	if err != nil || c.json {
		return err
	}
	printResult(r.Checkout.Path, r.Warnings, func() {
		from := r.Checkout.Source
		if a.Snapshot != "" {
			from = strings.TrimPrefix(from+"@", "@") + a.Snapshot
		}
		fmt.Fprintf(stderr, "%s  %s  %s\n", paint(stderr, styleAccent.Bold(true), r.Checkout.Name), paint(stderr, styleDim, "←"), paint(stderr, styleDim, from))
		if cp := r.Capture; cp != nil {
			fmt.Fprintf(stderr, "  %s %s files (%s) · %s %s (%s) in %.1fs\n", paint(stderr, styleDim, "source"),
				commas(int64(cp.Files)), humanBytes(cp.Bytes), paint(stderr, styleDim, "hashed"), commas(int64(cp.Hashed)), humanBytes(cp.HashedBytes), cp.Seconds)
			git := "none"
			if len(cp.Repos) > 0 {
				repos := make([]string, len(cp.Repos))
				for i, r := range cp.Repos {
					repos[i] = orDefault(r, ".")
				}
				git = strings.Join(repos, ", ")
			}
			vcs := "git: " + git
			if r.Checkout.JJ != "" {
				vcs += " · jj: " + r.Checkout.JJ
			}
			fmt.Fprintf(stderr, "  %s · %s · %s · %s\n",
				paint(stderr, styleDim, "cache "+commas(int64(cp.Cache))),
				paint(stderr, styleDim, "local "+commas(int64(cp.Local))),
				paint(stderr, styleDim, "hidden "+commas(int64(cp.Hidden))),
				paint(stderr, styleDim, vcs))
		} else if r.Checkout.JJ != "" {
			fmt.Fprintf(stderr, "  %s\n", paint(stderr, styleDim, "jj: "+r.Checkout.JJ))
		}
	})
	return nil
}

// printResult prints the one value a verb exists to produce, such as new's
// path, alone on stdout and last: after summary, the detail a person may want,
// which runs only when stderr is a terminal, and after the warnings. So a
// caller that captures stdout gets the value, and one that merges the streams
// finds it on the last line.
func printResult(value string, warnings []string, summary func()) {
	if isTerminal(os.Stderr) {
		summary()
	}
	printWarnings(warnings)
	fmt.Fprintln(stdout, value)
}

// printWarnings shows a reply's warnings on stderr.
func printWarnings(ws []string) {
	for _, w := range ws {
		fmt.Fprintln(stderr, paint(stderr, styleYellow, "warning:"), w)
	}
}

// forkCmd snapshots a checkout, opens a child on it and prints the child's path.
type forkCmd struct {
	Checkout string `arg:"" name:"CHECKOUT" help:"checkout to fork" completion-predictor:"checkout"`
	Name     string `name:"name" help:"child checkout name (default: <checkout>-<n>)"`
	Label    string `name:"label" short:"m" help:"snapshot label"`
}

func (cmd *forkCmd) Run(c *cli) error {
	a := api.ForkArgs{Checkout: cmd.Checkout, Name: cmd.Name, Label: cmd.Label}
	r, err := call[api.ForkReply](c, "fork", a)
	if err != nil || c.json {
		return err
	}
	printResult(r.Checkout.Path, r.Warnings, func() {
		fmt.Fprintf(stderr, "%s  %s  %s@%s\n", paint(stderr, styleAccent.Bold(true), r.Checkout.Name), paint(stderr, styleDim, "←"),
			paint(stderr, styleAccent, a.Checkout), paint(stderr, styleDim, r.Snapshot.ID))
		if r.Checkout.JJ != "" {
			fmt.Fprintf(stderr, "  %s\n", paint(stderr, styleDim, "jj: "+r.Checkout.JJ))
		}
	})
	return nil
}

// snapCmd freezes a checkout into a snapshot and prints its id.
type snapCmd struct {
	Checkout string `arg:"" name:"CHECKOUT" help:"checkout to snapshot" completion-predictor:"checkout"`
	Label    string `name:"label" short:"m" help:"snapshot label"`
	Wait     bool   `name:"wait" help:"wait until the tree id is known"`
}

func (cmd *snapCmd) Run(c *cli) error {
	a := api.SnapArgs{Checkout: cmd.Checkout, Label: cmd.Label, Wait: cmd.Wait}
	r, err := call[api.SnapReply](c, "snap", a)
	if err != nil || c.json {
		return err
	}
	printResult(r.Snapshot.ID, nil, func() {
		line := fmt.Sprintf("%s %s %s %s", paint(stderr, styleDim, "snapshot"), paint(stderr, styleAccent, r.Snapshot.ID),
			paint(stderr, styleDim, "of"), paint(stderr, styleAccent, a.Checkout))
		if r.Snapshot.Tree != "" {
			line += " " + paint(stderr, styleDim, "tree "+abbrev(r.Snapshot.Tree, 12))
		}
		fmt.Fprintln(stderr, line)
	})
	return nil
}

// diffCmd prints a checkout's changed source paths, or their line counts, or
// the patch alone, so that it pipes into git apply.
type diffCmd struct {
	Checkout string   `arg:"" name:"CHECKOUT" help:"checkout to diff" completion-predictor:"checkout"`
	Snapshot string   `arg:"" optional:"" name:"SNAP" help:"snapshot to compare against (default: the base)" completion-predictor:"snapshot"`
	Paths    []string `arg:"" optional:"" name:"-- PATH" help:"after --: only these files and folders, relative to the checkout's root"`
	Patch    bool     `name:"patch" short:"p" help:"print a git-format patch instead of the list (binary files are named, not carried)"`
	Binary   bool     `name:"binary" help:"print the patch as git diff --binary does, which git apply takes whole"`
	Stat     bool     `name:"stat" help:"list each path with the lines it adds and deletes"`
}

// AfterApply sorts the positional arguments around a bare --, which kong
// reads through: before it CHECKOUT and SNAP, after it the paths.
func (cmd *diffCmd) AfterApply(ctx *kong.Context) error {
	args := ctx.Args[1:]
	separator := slices.Index(args, "--")
	if separator < 0 {
		if len(cmd.Paths) != 0 {
			return errors.New("diff takes paths after --: ovnfs diff CHECKOUT [SNAP] -- PATH ...")
		}
		return nil
	}
	paths := args[separator+1:]
	before := slices.DeleteFunc(append([]string{cmd.Checkout, cmd.Snapshot}, cmd.Paths...), func(s string) bool { return s == "" })
	before = before[:len(before)-len(paths)]
	if len(before) == 0 || len(before) > 2 {
		return errors.New("diff takes CHECKOUT [SNAP] before --")
	}
	cmd.Checkout, cmd.Snapshot, cmd.Paths = before[0], "", nil
	if len(before) == 2 {
		cmd.Snapshot = before[1]
	}
	for _, p := range paths {
		cmd.Paths = append(cmd.Paths, filepath.ToSlash(p))
	}
	return nil
}

func (cmd *diffCmd) Run(c *cli) error {
	a := api.DiffArgs{Checkout: cmd.Checkout, From: cmd.Snapshot, Patch: cmd.Patch, Binary: cmd.Binary, Stat: cmd.Stat, Paths: cmd.Paths}
	r, err := call[api.DiffReply](c, "diff", a)
	if err != nil {
		// A second argument that is no snapshot was probably meant as a path.
		if e, ok := errors.AsType[*admin.RemoteError](err); ok && e.Code == "not_found" && strings.HasSuffix(e.Message, "snapshot "+a.From) && len(a.Paths) == 0 {
			return fmt.Errorf("%w; paths go after --: ovnfs diff %s -- %s", err, a.Checkout, a.From)
		}
		return c.predates(c.predates(err, len(a.Paths) != 0, "diff.paths", "diff -- PATH"), a.Stat, "diff.stat", "diff --stat")
	}
	if c.json {
		return nil
	}
	patch := a.Patch || a.Binary
	for i, ch := range r.Changes {
		switch {
		case a.Stat && r.Stat[i].Binary:
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", mark(stdout, ch.Status), ch.Path, paint(stdout, styleDim, "binary"))
		case a.Stat:
			fmt.Fprintf(stdout, "%s\t%s\t%s %s\n", mark(stdout, ch.Status), ch.Path,
				paint(stdout, styleGreen, fmt.Sprintf("+%d", r.Stat[i].Added)), paint(stdout, styleRed, fmt.Sprintf("-%d", r.Stat[i].Deleted)))
		case !patch:
			fmt.Fprintf(stdout, "%s\t%s\n", mark(stdout, ch.Status), ch.Path)
		}
	}
	if r.Patch != "" {
		colorPatch(stdout, r.Patch)
		if !strings.HasSuffix(r.Patch, "\n") {
			fmt.Fprintln(stdout)
		}
	}
	if a.Stat {
		added, deleted := 0, 0
		for _, s := range r.Stat {
			added, deleted = added+s.Added, deleted+s.Deleted
		}
		fmt.Fprintf(stderr, "%s changed, +%d -%d\n", plural(len(r.Changes), "path", "paths"), added, deleted)
	}
	if len(r.Changes) == 0 && r.Left != 0 {
		// Paths count from the root, not from the working directory as git's do.
		fmt.Fprintf(stderr, "no change under %s, counted from the root of %s; %s elsewhere\n",
			strings.Join(a.Paths, ", "), a.Checkout, plural(r.Left, "path changed", "paths changed"))
	}
	return nil
}

// applyCmd merges a checkout back into its source and fails when conflicts were left.
type applyCmd struct {
	Checkout string `arg:"" name:"CHECKOUT" help:"checkout to merge back" completion-predictor:"checkout"`
	DryRun   bool   `name:"dry-run" help:"report without writing"`
	Atomic   bool   `name:"atomic" help:"write nothing unless every path merges without a conflict"`
	Branch   string `name:"branch" help:"fetch commits as this branch, files untouched (land moves the checked-out branch with its files)"`
}

func (cmd *applyCmd) Run(c *cli) error {
	a := api.ApplyArgs{Checkout: cmd.Checkout, DryRun: cmd.DryRun, Branch: cmd.Branch, Atomic: cmd.Atomic}
	r, err := call[api.ApplyReply](c, "apply", a)
	if err != nil {
		return c.predates(err, a.Atomic, "apply.atomic", "apply --atomic")
	}
	advice := mergeAdvice{
		resolve: fmt.Sprintf("resolve them, then run `%s` again", paint(stderr, styleAccent, "ovnfs apply "+a.Checkout)),
		nothing: fmt.Sprintf("nothing to apply: %s holds every change of %s already", r.Source, a.Checkout),
	}
	if r.Pullable {
		advice.instead = fmt.Sprintf("`%s` merges the folder into the checkout instead, to resolve them there", paint(stderr, styleAccent, "ovnfs pull "+a.Checkout))
	}
	return printMerge(c, r, a.DryRun, advice)
}

// landCmd puts a checkout's commits on the branch checked out in its folder
// and fails when it refused to.
type landCmd struct {
	Checkout string `arg:"" name:"CHECKOUT" help:"checkout whose commits to land" completion-predictor:"checkout"`
	DryRun   bool   `name:"dry-run" help:"report without writing"`
	Markers  bool   `name:"markers" help:"where a file conflicts with the folder's uncommitted edits, write conflict markers there instead of refusing"`
}

func (cmd *landCmd) Run(c *cli) error {
	a := api.LandArgs{Checkout: cmd.Checkout, DryRun: cmd.DryRun, Markers: cmd.Markers}
	r, err := call[api.LandReply](c, "land", a)
	if err != nil {
		return c.predates(err, true, "land", "land")
	}
	again := paint(stderr, styleAccent, "ovnfs land "+a.Checkout)
	if c.json {
		if len(r.Conflicts) != 0 {
			return exitCode(1)
		}
		return nil
	}
	if a.DryRun {
		fmt.Fprintln(stderr, paint(stderr, styleYellow, "dry run: nothing written"))
	}
	printMerged(r.ApplyReply, false)
	pending := !slices.ContainsFunc(r.Conflicts, func(cf api.Conflict) bool { return !cf.Pending })
	switch {
	case r.Refused && pending:
		fmt.Fprintf(stderr, "%s: %s conflict markers in %s; resolve them, then `%s`\n",
			paint(stderr, styleYellow, "nothing landed"), plural(len(r.Conflicts), "file still has", "files still have"), r.Source, again)
		return exitCode(1)
	case r.Refused:
		line := fmt.Sprintf("%s: %s with uncommitted changes in %s; `%s` writes conflict markers there to resolve, then `%s`",
			paint(stderr, styleYellow, "nothing landed"), plural(len(r.Conflicts), "file conflicts", "files conflict"), r.Source,
			paint(stderr, styleAccent, "ovnfs land "+a.Checkout+" --markers"), again)
		if r.Pullable {
			line += fmt.Sprintf("; or `%s` merges those changes into the checkout, to resolve them there first", paint(stderr, styleAccent, "ovnfs pull "+a.Checkout))
		}
		fmt.Fprintln(stderr, line)
		return exitCode(1)
	case len(r.Conflicts) != 0:
		// Markers were asked for, or the folder changed under an atomic merge.
		fmt.Fprintf(stderr, "%s: %s conflict markers in %s; resolve them, then `%s`\n",
			paint(stderr, styleYellow, r.Branch+" did not move"), plural(len(r.Conflicts), "file has", "files have"), r.Source, again)
		return exitCode(1)
	case r.Commit == r.Old:
		fmt.Fprintf(stderr, "%s: %s holds no commit beyond %s; `%s` merges its uncommitted changes\n",
			paint(stderr, styleYellow, "nothing to land"), a.Checkout, r.Branch, paint(stderr, styleAccent, "ovnfs apply "+a.Checkout))
		return nil
	}
	fmt.Fprintf(stdout, "%s %s %s %s %s\n", paint(stdout, styleDim, "branch"), paint(stdout, styleAccent, r.Branch), paint(stdout, styleDim, "at"),
		paint(stdout, styleDim, abbrev(r.Commit, 12)), paint(stdout, styleDim, "(was "+abbrev(r.Old, 12)+")"))
	if r.Left > 0 {
		fmt.Fprintf(stderr, "%s in %s outside those commits; `%s` merges them as uncommitted edits\n",
			paint(stderr, styleYellow, plural(r.Left, "path stays changed", "paths stay changed")), a.Checkout, paint(stderr, styleAccent, "ovnfs apply "+a.Checkout))
	}
	return nil
}

// pullCmd merges newer source changes into a checkout: its folder's, or those
// of the remote checkout it is attached to.
type pullCmd struct {
	Checkout string `arg:"" name:"CHECKOUT" help:"checkout to pull into" completion-predictor:"checkout"`
	From     string `name:"from" help:"pull the checkout attached on this host instead of the folder; implied by a single attachment"`
	DryRun   bool   `name:"dry-run" help:"report without writing"`
}

func (cmd *pullCmd) Run(c *cli) error {
	a := api.PullArgs{Checkout: cmd.Checkout, From: cmd.From, DryRun: cmd.DryRun}
	r, err := call[api.PullReply](c, "pull", a)
	if err != nil {
		return c.predates(err, a.From == "", "pull.folder", "pulling from the folder")
	}
	if !c.json {
		printWarnings(r.Warnings)
	}
	again := "ovnfs pull " + a.Checkout
	if a.From != "" {
		again += " --from " + a.From
	}
	advice := mergeAdvice{
		resolve: fmt.Sprintf("resolve them, then run `%s` again", paint(stderr, styleAccent, again)),
		nothing: "nothing to pull: " + a.Checkout + " is up to date",
	}
	if r.Folder != "" {
		advice.resolve = fmt.Sprintf("resolve them in %s, then `%s`, or `%s` for its commits", r.Source,
			paint(stderr, styleAccent, "ovnfs apply "+a.Checkout), paint(stderr, styleAccent, "ovnfs land "+a.Checkout))
	}
	return printMerge(c, r.ApplyReply, a.DryRun, advice)
}

// mergeAdvice words the outcomes of an apply or a pull that need a sentence.
type mergeAdvice struct {
	resolve string // once conflict markers were written
	instead string // when a dry run found that they would be; "" for nothing to offer
	nothing string // when no path changed or conflicted, so that silence is not the answer
}

// printMerge prints apply and pull outcomes and fails when conflicts remain,
// with advice on resolving the conflict markers.
func printMerge(c *cli, r api.ApplyReply, dryRun bool, advice mergeAdvice) error {
	if !c.json {
		if dryRun {
			fmt.Fprintln(stderr, paint(stderr, styleYellow, "dry run: nothing written"))
		}
		printMerged(r, r.Branch != "")
		if r.Branch != "" {
			fmt.Fprintf(stdout, "%s %s %s %s\n", paint(stdout, styleDim, "branch"), paint(stdout, styleAccent, r.Branch), paint(stdout, styleDim, "at"), paint(stdout, styleDim, abbrev(r.Commit, 12)))
		} else if len(r.Applied)+len(r.Merged)+len(r.Conflicts) == 0 {
			fmt.Fprintln(stderr, paint(stderr, styleDim, advice.nothing))
		}
	}
	if len(r.Conflicts) == 0 {
		return nil
	}
	if r.Refused {
		what := fmt.Sprintf("%s: %s", paint(stderr, styleYellow, "nothing written"), plural(len(r.Conflicts), "path conflicts", "paths conflict"))
		if advice.instead != "" {
			what += "; " + advice.instead
		}
		fmt.Fprintln(stderr, what)
		return exitCode(1)
	}
	marked := 0
	for _, cf := range r.Conflicts {
		if cf.Markers {
			marked++
		}
	}
	if untouched := len(r.Conflicts) - marked; untouched > 0 {
		fmt.Fprintf(stderr, "%s left untouched\n", paint(stderr, styleYellow, plural(untouched, "conflict", "conflicts")))
	}
	switch {
	case marked > 0 && dryRun && advice.instead != "":
		fmt.Fprintf(stderr, "%s would have conflict markers; %s\n", paint(stderr, styleYellow, plural(marked, "file", "files")), advice.instead)
	case marked > 0 && dryRun:
		fmt.Fprintf(stderr, "%s would have conflict markers\n", paint(stderr, styleYellow, plural(marked, "file", "files")))
	case marked > 0:
		fmt.Fprintf(stderr, "%s conflict markers: %s\n", paint(stderr, styleYellow, plural(marked, "file has", "files have")), advice.resolve)
	}
	return exitCode(1)
}

// printMerged lists what a merge did or would do, path by path: the paths
// taken, those merged as text and the conflicts, with markers or left alone.
// index marks the paths of an apply to a checked-out branch, which moves index
// entries and no file.
func printMerged(r api.ApplyReply, index bool) {
	for _, ch := range r.Applied {
		if index {
			fmt.Fprintf(stdout, "%s %s\t%s\n", paint(stdout, styleDim, "index"), mark(stdout, ch.Status), ch.Path)
		} else {
			fmt.Fprintf(stdout, "%s\t%s\n", mark(stdout, ch.Status), ch.Path)
		}
	}
	for _, p := range r.Merged {
		fmt.Fprintf(stdout, "%s %s\n", paint(stdout, styleBlue, "merged"), p)
	}
	for _, cf := range r.Conflicts {
		verb := "conflict"
		if cf.Markers && !r.Refused {
			verb = "marked"
		}
		fmt.Fprintf(stdout, "%s %s: %s\n", paint(stdout, styleRed, verb), cf.Path, paint(stdout, styleDim, cf.Reason))
	}
}

// predates explains a daemon failure that only its age accounts for: when
// applies holds and the running daemon serves neither a feature nor an
// operation called name, it predates what this binary asked of it, and err
// alone would not say so.
func (c *cli) predates(err error, applies bool, name, what string) error {
	if !applies || c.conn == nil {
		return err
	}
	if w := c.conn.Welcome(); slices.Contains(w.Features, name) || slices.Contains(w.Ops, name) {
		return err
	}
	return fmt.Errorf("%w\nthe running daemon predates %s: `ovnfs restart` starts this version", err, what)
}

// rmCmd removes a checkout and says so, unless that would destroy commits.
type rmCmd struct {
	Checkout        string  `arg:"" name:"CHECKOUT" help:"checkout to remove" completion-predictor:"checkout"`
	Force           bool    `name:"force" short:"f" help:"remove even a checkout holding commits its folder's repository lacks"`
	StickySnapshots bool    `name:"sticky-snapshots" help:"retain snapshots of the removed checkout"`
	IfID            *string `name:"if-id" help:"remove only the checkout instance with this id (see ls --json)"`
}

func (cmd *rmCmd) Run(c *cli) error {
	a := api.RmArgs{Checkout: cmd.Checkout, StickySnapshots: cmd.StickySnapshots}
	if cmd.IfID != nil {
		if *cmd.IfID == "" {
			return errors.New("rm: --if-id needs a nonempty checkout id")
		}
		a.IfID = api.CheckoutID(*cmd.IfID)
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	features := client.Welcome().Features
	if a.IfID != "" && !slices.Contains(features, "rm.if_id") {
		return errors.New("rm: the running daemon does not support --if-id; restart it with this version of ovnfs")
	}
	// A daemon that predates the check refuses the field itself.
	a.KeepCommits = !cmd.Force && slices.Contains(features, "rm.keep_commits")
	if _, err := call[json.RawMessage](c, "rm", a); err != nil || c.json {
		return err
	}
	line := paint(stderr, styleGreen, "removed") + " " + paint(stderr, styleAccent, a.Checkout)
	if a.StickySnapshots {
		line += "; snapshots retained"
	}
	fmt.Fprintln(stderr, line)
	return nil
}

// attachCmd offers a folder or checkout to another machine and prints where it appeared.
type attachCmd struct {
	Target    string `arg:"" optional:"" name:"DIR|CHECKOUT" help:"folder or checkout to attach (default .)" completion-predictor:"checkout"`
	To        string `name:"to" required:"" help:"ssh destination of the remote machine"`
	Name      string `name:"name" help:"checkout name on the remote machine"`
	RemoteBin string `name:"remote-bin" help:"path of ovnfs on the remote machine (default: ovnfs on its PATH)"`
}

func (cmd *attachCmd) Run(c *cli) error {
	dir, checkoutName, err := resolveTarget(orDefault(cmd.Target, "."))
	if err != nil {
		return err
	}
	reply, err := call[api.AttachReply](c, "attach", api.AttachArgs{Dir: dir, Checkout: checkoutName, To: cmd.To, Name: cmd.Name, RemoteBin: cmd.RemoteBin})
	if err != nil || c.json {
		return err
	}
	printAttached(cmd.To, reply.RemotePath, reply)
	return nil
}

// printAttached reports an attach: host:path on stdout for scripts, the
// checkout and link on stderr.
func printAttached(host, path string, r api.AttachReply) {
	fmt.Fprintf(stdout, "%s:%s\n", host, path)
	fmt.Fprintf(stderr, "%s@%s  %s  %s:%s %s\n", paint(stderr, styleAccent.Bold(true), r.Checkout), paint(stderr, styleDim, r.Snapshot.ID),
		paint(stderr, styleDim, "→"), paint(stderr, styleAccent, r.Host), paint(stderr, styleAccent, r.RemoteCheckout),
		paint(stderr, styleDim, "(content moves on first read; the link reconnects while this daemon runs)"))
}

// linkCmd pipes stdin and stdout to the daemon's link socket; attach runs it over
// ssh. Nothing else may be written to stdout: it carries the link protocol.
type linkCmd struct{}

func (cmd *linkCmd) Run(c *cli) error {
	if _, err := c.client(); err != nil {
		return err
	}
	conn, err := net.Dial("unix", filepath.Join(c.state, linkSocket))
	if err != nil {
		return fmt.Errorf("connecting to the link socket: %w", err)
	}
	defer conn.Close()
	go func() {
		io.Copy(conn, os.Stdin)
		if u, ok := conn.(*net.UnixConn); ok {
			u.CloseWrite()
		}
	}()
	_, err = io.Copy(os.Stdout, conn)
	return err
}

// shareCmd prints the remote-fs or kernel-client socket of a checkout,
// starting its share if needed.
type shareCmd struct {
	Checkout  string `arg:"" name:"CHECKOUT" help:"checkout to share" completion-predictor:"checkout"`
	ReadWrite bool   `name:"rw" help:"let the client write"`
	Kernel    bool   `name:"kernel" help:"serve the ovnfs guest kernel client (vmon vmm --ovnfs SOCK) instead of remote-fs"`
}

// Run starts the share and prints its socket, then the VMM flag that uses it.
func (cmd *shareCmd) Run(c *cli) error {
	reply, err := call[api.ShareReply](c, "share", api.ShareArgs{Checkout: cmd.Checkout, Writable: cmd.ReadWrite, Kernel: cmd.Kernel})
	if err != nil || c.json {
		return err
	}
	fmt.Fprintln(stdout, reply.Socket)
	if cmd.Kernel {
		fmt.Fprintf(stderr, "%s %s\n", paint(stderr, styleAccent, "vmon vmm --ovnfs"), paint(stderr, styleDim, reply.Socket))
		return nil
	}
	mode := ""
	if reply.Writable {
		mode = ":rw"
	}
	fmt.Fprintf(stderr, "%s home:%s\n", paint(stderr, styleAccent, "vmon --remote-fs"), paint(stderr, styleDim, reply.Socket+mode))
	return nil
}

// dropCmd releases a snapshot.
type dropCmd struct {
	Snapshot string `arg:"" name:"SNAP" help:"snapshot to release" completion-predictor:"snapshot"`
}

func (cmd *dropCmd) Run(c *cli) error {
	_, err := call[json.RawMessage](c, "drop", api.DropArgs{Snapshot: cmd.Snapshot})
	return err
}

// explainCmd prints the class of one path and the rule that decided it.
type explainCmd struct {
	First  string   `arg:"" name:"DIR|CHECKOUT|PATH" help:"a path, or the folder or checkout it belongs to"`
	Second string   `arg:"" optional:"" name:"PATH" help:"path, when the first argument named the folder or checkout"`
	Rules  []string `name:"rule" sep:"none" help:"extra rule line (repeatable)"`
}

func (cmd *explainCmd) Run(c *cli) error {
	a := api.ExplainArgs{Rules: cmd.Rules}
	target, path := "", cmd.First
	if cmd.Second != "" {
		target, path = cmd.First, cmd.Second
	}
	var err error
	if a.Dir, a.Checkout, err = resolveTarget(target); err != nil {
		return fmt.Errorf("explain: %w", err)
	}
	if a.Path, err = relativeTo(a.Dir, path); err != nil {
		return fmt.Errorf("explain: %w", err)
	}
	r, err := call[api.ExplainReply](c, "explain", a)
	if err != nil || c.json {
		return err
	}
	line := paint(stdout, classStyle(r.Class), r.Class) + strings.Repeat(" ", max(1, 9-len(r.Class))) +
		paint(stdout, styleDim, r.Rule.Origin) + "  " + paint(stdout, styleAccent, r.Rule.Class) + " " + r.Rule.Text
	if r.At != "" && r.At != r.Path {
		line += paint(stdout, styleDim, " (decided at "+r.At+"/)")
	}
	if !r.Exists {
		line += paint(stdout, styleDim, " (no such path; classified by name)")
	}
	fmt.Fprintln(stdout, line)
	return nil
}

// planCmd prints how a launch elsewhere would recreate a folder or checkout.
type planCmd struct {
	Target string   `arg:"" optional:"" name:"DIR|CHECKOUT" help:"folder or checkout to plan (default .)" completion-predictor:"checkout"`
	On     string   `name:"on" default:"cloud" enum:"cloud,peer" help:"where the launch would run"`
	Rules  []string `name:"rule" sep:"none" help:"extra rule line (repeatable)"`
}

func (cmd *planCmd) Run(c *cli) error {
	a := api.PlanArgs{On: cmd.On, Rules: cmd.Rules}
	var err error
	if a.Dir, a.Checkout, err = resolveTarget(cmd.Target); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	r, err := call[api.PlanReply](c, "plan", a)
	if err != nil || c.json {
		return err
	}
	printPlan(os.Stdout, r)
	return nil
}

// isDirArg reports whether a DIR|CHECKOUT argument names a folder rather than a checkout.
func isDirArg(s string) bool {
	if pathArg(s) {
		return true
	}
	info, err := os.Stat(s)
	return err == nil && info.IsDir()
}

// pathArg reports whether an argument is spelled as a path: ".", "..", or with a separator.
func pathArg(s string) bool {
	return s == "." || s == ".." || strings.ContainsRune(s, '/') || strings.ContainsRune(s, filepath.Separator)
}

// resolveTarget turns an optional DIR|CHECKOUT argument into exactly one of an absolute dir or a checkout name.
func resolveTarget(arg string) (dir, checkout string, err error) {
	if arg != "" && !isDirArg(arg) {
		return "", arg, nil
	}
	if dir, err = filepath.Abs(orDefault(arg, ".")); err != nil {
		return "", "", fmt.Errorf("resolving %s: %w", arg, err)
	}
	return dir, "", nil
}

// relativeTo converts a user path into the slash-separated form relative to dir ("" for a checkout's root).
func relativeTo(dir, path string) (string, error) {
	if dir == "" {
		if filepath.IsAbs(path) {
			return "", fmt.Errorf("path %s must be relative to the checkout", path)
		}
		dir = string(filepath.Separator)
		path = filepath.Join(dir, path)
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "", fmt.Errorf("path %s: %w", path, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path %s is outside %s", path, dir)
	}
	if rel == "." {
		return "", nil
	}
	return rel, nil
}

// printPlan renders a plan as one header line and one labelled block per class.
func printPlan(w io.Writer, r api.PlanReply) {
	fmt.Fprintf(w, "%s %s %s     %s\n", paint(w, styleAccent, r.Root), paint(w, styleDim, "→"), r.On, paint(w, styleDim, gitSummary(r.Repos)))
	const indent = "         " // width of the class label column
	label := func(s string) string { return paint(w, styleDim, s) + strings.Repeat(" ", max(0, 9-len(s))) }
	counts := []string{
		plural(r.Source.Files, "file", "files"),
		plural(r.Local.Files, "path", "paths"),
		plural(r.Hidden.Files, "path", "paths"),
	}
	width := 0
	for _, s := range counts {
		width = max(width, len(s))
	}
	column := func(count string) string { return fmt.Sprintf("%-*s   ", width, count) }

	var parts []string
	if r.Origin.Files > 0 {
		at := ""
		if len(r.Repos) == 1 && r.Repos[0].Head != "" {
			at = " @" + abbrev(r.Repos[0].Head, 7)
		}
		parts = append(parts, commas(int64(r.Origin.Files))+" from origin"+at)
	}
	if r.Here.Files > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%s from this machine · %s%s", commas(int64(r.Here.Files)), humanBytes(r.Here.Bytes), pathSample(r.Here, 3)))
	}
	fmt.Fprintf(w, "%s%s%s\n", label("source"), column(counts[0]), parts[0])
	for _, p := range parts[1:] {
		fmt.Fprintf(w, "%s%s%s\n", indent, strings.Repeat(" ", width+3), p)
	}

	if len(r.Cache) == 0 {
		fmt.Fprintf(w, "%s%s\n", label("cache"), paint(w, styleDim, "none"))
	}
	rootWidth := 0
	for _, cache := range r.Cache {
		rootWidth = max(rootWidth, len(cache.Root))
	}
	for i, cache := range r.Cache {
		prefix := indent
		if i == 0 {
			prefix = label("cache")
		}
		action := paint(w, styleDim, "starts empty")
		if cache.Rebuild != "" {
			action = paint(w, styleDim, "rebuild: ") + cache.Rebuild
		}
		if cache.KeyHash != "" {
			key := cache.KeyHash
			if len(key) > 11 {
				key = key[:11] + "…"
			}
			action += paint(w, styleDim, "  key "+key)
		}
		fmt.Fprintf(w, "%s%-*s  %s\n", prefix, rootWidth, cache.Root, action)
	}

	if r.Local.Files == 0 {
		fmt.Fprintf(w, "%s%s\n", label("local"), paint(w, styleDim, "none"))
	} else {
		list := strings.Join(r.Local.Paths, " · ")
		if len(r.Local.Paths) < r.Local.Files {
			list += " · …"
		}
		if r.Local.Bytes > 0 {
			list += " (" + humanBytes(r.Local.Bytes) + ")"
		}
		fmt.Fprintf(w, "%s%s%s — stay here\n", label("local"), column(counts[1]), list)
	}
	if r.Hidden.Files == 0 {
		fmt.Fprintf(w, "%s%s\n", label("hidden"), paint(w, styleDim, "none"))
	} else {
		fmt.Fprintf(w, "%s%s\n", label("hidden"), counts[2])
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(w, "%s%s\n", label("warn"), paint(w, styleYellow, warning))
	}
	fmt.Fprintf(w, "%s%s\n", label("lock"), r.Lock)
}

// gitSummary renders one `git · origin … · HEAD …` fragment per repo, or "no git".
func gitSummary(repos []api.PlanRepo) string {
	if len(repos) == 0 {
		return "no git"
	}
	fragments := make([]string, len(repos))
	for i, repo := range repos {
		parts := []string{strings.TrimSpace("git " + repo.Path)}
		if repo.Origin != "" {
			parts = append(parts, "origin "+shortOrigin(repo.Origin))
		} else {
			parts = append(parts, "no origin")
		}
		if repo.Head != "" {
			head := "HEAD " + abbrev(repo.Head, 7)
			if repo.Pushed {
				head += " (pushed)"
			} else {
				head += " (not pushed)"
			}
			parts = append(parts, head)
		}
		fragments[i] = strings.Join(parts, " · ")
	}
	return strings.Join(fragments, "   ")
}

// shortOrigin strips scheme, user and .git from a remote URL so it reads as host/owner/repo.
func shortOrigin(url string) string {
	s := url
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	} else if at := strings.Index(s, "@"); at >= 0 && strings.Contains(s[at:], ":") {
		s = strings.Replace(s[at+1:], ":", "/", 1) // scp-like git@host:owner/repo
	}
	if at := strings.Index(s, "@"); at >= 0 && at < strings.Index(s+"/", "/") {
		s = s[at+1:]
	}
	return strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
}

// pathSample lists up to n of a set's paths, with an ellipsis when more exist.
func pathSample(set api.PlanSet, n int) string {
	if len(set.Paths) == 0 {
		return ""
	}
	shown := set.Paths[:min(n, len(set.Paths))]
	list := strings.Join(shown, ", ")
	if len(shown) < set.Files {
		list += ", …"
	}
	return "  (" + list + ")"
}

// humanBytes is the shared base-1000 size rendering: B, KB, MB, GB, TB with at most one decimal.
func humanBytes(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10) + " B"
	}
	v := float64(n)
	unit := ""
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		v /= 1000
		unit = u
		if v < 999.95 {
			break
		}
	}
	return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + " " + unit
}

// plural renders a count with the singular or plural noun, e.g. "1 file", "1,024 files".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return commas(int64(n)) + " " + many
}

// commas renders an integer with thousands separators.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// abbrev shortens an id to its first n characters.
func abbrev(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// orDefault returns s, or fallback when s is empty.
func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// stamp renders a time for tables, local, to the minute and as one word, so
// that the columns after it split on blanks; "-" for none, as from a daemon
// older than the field.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02T15:04")
}

// yesNo renders a flag for tables.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
