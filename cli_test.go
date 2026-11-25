package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/stencil-hq/ovnfs/api"
)

func TestPrintCheckoutsOrphanSnapshots(t *testing.T) {
	for _, live := range []bool{false, true} {
		r := api.LsReply{Snapshots: []api.Snapshot{
			{ID: "orphan-id", Checkout: "removed", Label: "saved", Orphan: true},
		}}
		if live {
			r.Checkouts = []api.Checkout{{Name: "removed", Source: "/current"}}
			r.Snapshots = append(r.Snapshots, api.Snapshot{ID: "live-id", Checkout: "removed"})
		}
		var out strings.Builder
		printCheckouts(&out, r)
		got := out.String()
		if !strings.Contains(got, "ORPHAN SNAPSHOT") || !hasRow(got, "orphan-id", "removed", "saved") {
			t.Fatalf("orphan missing from %q", got)
		}
		if strings.Contains(got, "live-id") || live && !hasRow(got, "removed", "/current", "1", "no") {
			t.Fatalf("wrong snapshot count for reused name in %q", got)
		}
	}
}

// TestPrintCheckoutsSession checks that ls adds a SESSION column only once a
// checkout records the agent session that opened it, with "-" for the rest.
func TestPrintCheckoutsSession(t *testing.T) {
	touched := time.Now()
	r := api.LsReply{Checkouts: []api.Checkout{{Name: "plain", Path: "/m/plain", Touched: touched}}}
	var out strings.Builder
	printCheckouts(&out, r)
	if got := out.String(); strings.Contains(got, "SESSION") {
		t.Fatalf("SESSION column without sessions in %q", got)
	}
	r.Checkouts = append(r.Checkouts, api.Checkout{Name: "agent", Path: "/m/agent", Touched: touched, Session: "0199c2e4-agent"})
	out.Reset()
	printCheckouts(&out, r)
	got := out.String()
	if !hasRow(got, "TOUCHED", "SESSION", "PATH") || !hasRow(got, "plain", stamp(touched), "-", "/m/plain") || !hasRow(got, "agent", "0199c2e4-agent", "/m/agent") {
		t.Fatalf("sessions missing from %q", got)
	}
}

// TestTablePadding checks that columns are padded by visible width, so a styled
// cell aligns with the plain text a script would see, and that a writer without
// colour gets no escapes at all.
func TestTablePadding(t *testing.T) {
	var plain strings.Builder
	table(&plain, [][]string{{"ID", "NAME"}, {"3f2a", "app"}})
	if got, want := plain.String(), "ID    NAME\n3f2a  app\n"; got != want {
		t.Fatalf("plain table = %q, want %q", got, want)
	}

	var styled strings.Builder
	w := &colorprofile.Writer{Forward: &styled, Profile: colorprofile.ANSI}
	table(w, [][]string{{paint(w, styleBold, "ID"), "NAME"}, {paint(w, styleAccent, "3f2a"), "app"}})
	lines := strings.Split(strings.TrimSuffix(styled.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "\x1b") {
		t.Fatalf("styled table = %q, want two coloured lines", styled.String())
	}
	for i, want := range []string{"ID    NAME", "3f2a  app"} {
		if got := lipgloss.Width(lines[i]); got != lipgloss.Width(want) {
			t.Fatalf("styled row %d is %d wide, want %d: %q", i, got, lipgloss.Width(want), lines[i])
		}
	}
}

// hasRow reports whether some line of out holds every field, in order.
func hasRow(out string, fields ...string) bool {
	for line := range strings.Lines(out) {
		at := 0
		for _, field := range fields {
			i := strings.Index(line[at:], field)
			if i < 0 {
				at = -1
				break
			}
			at += i + len(field)
		}
		if at >= 0 {
			return true
		}
	}
	return false
}

// TestGlobalOptionsBeforeCommand checks that global options are applied without
// consuming a child command's arguments after the separator.
func TestGlobalOptionsBeforeCommand(t *testing.T) {
	var c cli
	args, err := splitGlobals(&c, []string{"run", "--state", "/tmp/state", "--json", "--", "echo", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if c.state != "/tmp/state" || !c.json || !slices.Equal(args, []string{"run", "--", "echo", "--json"}) {
		t.Fatalf("state=%q json=%v args=%q", c.state, c.json, args)
	}
}

// TestGlobTypeOptional parses glob without --type as every type, and still
// refuses a type that is not one (#87).
func TestGlobTypeOptional(t *testing.T) {
	for _, tt := range []struct {
		args []string
		typ  string
		ok   bool
	}{
		{args: []string{"glob", "*.go"}, ok: true},
		{args: []string{"glob", "*.go", "--type", "dir"}, typ: "dir", ok: true},
		{args: []string{"glob", "*.go", "--type", "socket"}},
	} {
		var v verbs
		parser, err := newCLIParser(&cli{}, tt.args, &v)
		if err != nil {
			t.Fatal(err)
		}
		_, err = parser.Parse(tt.args)
		typ := ""
		if v.Glob.Type != nil {
			typ = *v.Glob.Type
		}
		if (err == nil) != tt.ok || tt.ok && typ != tt.typ {
			t.Errorf("%q: type %q, %v", tt.args, typ, err)
		}
	}
}

// TestResolveTarget checks DIR-versus-CHECKOUT detection and that dirs come back absolute.
func TestResolveTarget(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "notes"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmp)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(filepath.Dir(cwd), "abs", "where")
	tests := []struct {
		arg      string
		dir      string
		checkout string
	}{
		{arg: "", dir: cwd},
		{arg: ".", dir: cwd},
		{arg: "..", dir: filepath.Dir(cwd)},
		{arg: "app", dir: filepath.Join(cwd, "app")},  // existing directory wins over a checkout name
		{arg: "web/", dir: filepath.Join(cwd, "web")}, // a slash means a path even when missing
		{arg: abs, dir: abs},
		{arg: "notes", checkout: "notes"}, // a regular file is not a directory
		{arg: "web", checkout: "web"},     // missing, no slash: a checkout
	}
	for _, tt := range tests {
		dir, checkout, err := resolveTarget(tt.arg)
		if err != nil {
			t.Fatalf("resolveTarget(%q): %v", tt.arg, err)
		}
		if dir != tt.dir || checkout != tt.checkout {
			t.Errorf("resolveTarget(%q) = dir %q checkout %q; want dir %q checkout %q", tt.arg, dir, checkout, tt.dir, tt.checkout)
		}
	}
}

// TestSourceCheckout checks that source picks a checkout by name or by the
// current directory, through symlinks, and refuses checkouts attached from
// another machine.
func TestSourceCheckout(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(tmp, "mnt", "checkouts")
	for _, d := range []string{"ap/sub", "app/sub", "web/deep/er", "we", "other"} {
		if err := os.MkdirAll(filepath.Join(mount, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(filepath.Join(tmp, "mnt"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	into := filepath.Join(tmp, "into")
	if err := os.Symlink(filepath.Join(mount, "we"), into); err != nil {
		t.Fatal(err)
	}
	src := func(n string) string { return filepath.Join(tmp, "src", n) }
	// A Windows mount is a drive, whose parent mountSpellings does not
	// resolve; there the link cwd cases match through the resolved cwd instead.
	reported := filepath.Join(link, "checkouts")
	if runtime.GOOS == "windows" {
		reported = mount
	}
	r := api.LsReply{Checkouts: []api.Checkout{
		{Name: "ap", Path: filepath.Join(reported, "ap"), Source: src("ap")},
		{Name: "web", Path: filepath.Join(reported, "web"), Source: src("web")},
		{Name: "we", Path: filepath.Join(reported, "we"), Source: src("we")},
		{Name: "app", Path: filepath.Join(reported, "app"), Source: "laptop:/src/app"},
	}}
	tests := []struct {
		name, cwd string
		want      string // checkout name
		wantErr   string // substring of the error
	}{
		{name: "web", want: "web"},
		{cwd: filepath.Join(mount, "web"), want: "web"},
		{cwd: filepath.Join(mount, "web", "deep", "er"), want: "web"},
		{cwd: filepath.Join(link, "checkouts", "web", "deep"), want: "web"},
		{cwd: filepath.Join(mount, "ap", "sub"), want: "ap"},
		{cwd: filepath.Join(mount, "we"), want: "we"},
		{cwd: into, want: "we"},
		{cwd: filepath.Join(mount, "web", "gone"), want: "web"},
		{cwd: filepath.Join(mount, "other"), wantErr: "not inside a checkout"},
		{cwd: tmp, wantErr: "not inside a checkout"},
		{name: "nope", wantErr: "no checkout nope"},
		{name: "app", wantErr: "checkout app is attached from another machine (laptop:/src/app) and has no local folder"},
		{cwd: filepath.Join(mount, "app", "sub"), wantErr: "attached from another machine (laptop:/src/app)"},
	}
	for _, tt := range tests {
		got, err := sourceCheckout(r, tt.name, tt.cwd)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("sourceCheckout(%q, %q) error = %v; want containing %q", tt.name, tt.cwd, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got.Name != tt.want {
			t.Errorf("sourceCheckout(%q, %q) = %q, %v; want %q", tt.name, tt.cwd, got.Name, err, tt.want)
		}
	}
}

// TestLocalFolder checks which sources name a folder on Windows, where an
// attached checkout's one-letter host reads as a drive.
func TestLocalFolder(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows reads host:dir as a drive")
	}
	for source, want := range map[string]bool{`C:\src`: true, `\\host\share\src`: true, "c:/src": false, "c:src": false} {
		if got := localFolder(source); got != want {
			t.Errorf("localFolder(%q) = %v; want %v", source, got, want)
		}
	}
}

// TestResolveDirs checks the precedence of the daemon's folders: flag, then
// environment, then ~/.o2/ovnfs.yml, then the defaults.
func TestResolveDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("OVNFS_STATE", "")
	t.Setenv("OVNFS_MOUNT", "")
	resolve := func(flagState string) cli {
		t.Helper()
		c := cli{state: flagState}
		if err := c.resolveDirs(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := resolve(""); c.state != defaultState || c.mount != defaultMount {
		t.Errorf("no config = %q %q", c.state, c.mount)
	}
	if err := os.MkdirAll(filepath.Join(home, ".o2"), 0o755); err != nil {
		t.Fatal(err)
	}
	confMount := filepath.Join(t.TempDir(), "conf-mount")
	if err := os.WriteFile(filepath.Join(home, ".o2", "ovnfs.yml"), []byte("state: ~/conf-state\nmount: "+confMount+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := resolve(""); c.state != filepath.Join(home, "conf-state") || c.mount != confMount {
		t.Errorf("config = %q %q", c.state, c.mount)
	}
	t.Setenv("OVNFS_STATE", "/env/state")
	if c := resolve(""); c.state != "/env/state" || c.mount != confMount {
		t.Errorf("env over config = %q %q", c.state, c.mount)
	}
	if c := resolve("/flag/state"); c.state != "/flag/state" {
		t.Errorf("flag over env = %q", c.state)
	}
}

// TestSecondsFlag checks the --idle units: Go durations plus whole days and weeks,
// refusing negatives, fractions of days and durations a time.Duration cannot hold.
func TestSecondsFlag(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want float64
		bad  bool
	}{
		{in: "90m", want: 5400},
		{in: "7d", want: 7 * 86400},
		{in: "2w", want: 14 * 86400},
		{in: "0", want: 0},
		{in: "-1h", bad: true},
		{in: "1.5d", bad: true},
		{in: "5000w", bad: true},
		{in: "d", bad: true},
		{in: "", bad: true},
	} {
		var s seconds
		err := s.UnmarshalText([]byte(tt.in))
		if (err != nil) != tt.bad || !tt.bad && float64(s) != tt.want {
			t.Errorf("UnmarshalText(%q) = %v, %v; want %v, error %v", tt.in, float64(s), err, tt.want, tt.bad)
		}
	}
}

func TestScopeLsReply(t *testing.T) {
	r := api.LsReply{
		Checkouts: []api.Checkout{{Name: "x"}, {Name: "y"}},
		Snapshots: []api.Snapshot{
			{ID: "s1", Checkout: "x", Label: "one"},
			{ID: "s2", Checkout: "y", Label: "two"},
			{ID: "s0", Checkout: "x", Label: "old", Orphan: true},
		},
	}
	got := scopeLsReply(r, "x")
	if len(got.Checkouts) != 1 || got.Checkouts[0].Name != "x" {
		t.Fatalf("checkouts = %+v", got.Checkouts)
	}
	if len(got.Snapshots) != 1 || got.Snapshots[0].ID != "s1" {
		t.Fatalf("snapshots = %+v", got.Snapshots)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Checkouts []struct {
			Name string `json:"name"`
		} `json:"checkouts"`
		Snapshots []struct {
			ID       string `json:"id"`
			Checkout string `json:"checkout"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Checkouts) != 1 || payload.Checkouts[0].Name != "x" {
		t.Fatalf("json checkouts = %s", raw)
	}
	if len(payload.Snapshots) != 1 || payload.Snapshots[0].ID != "s1" || payload.Snapshots[0].Checkout != "x" {
		t.Fatalf("json snapshots = %s", raw)
	}

	missing := scopeLsReply(r, "z")
	raw, err = json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"checkouts":[],"snapshots":[]}` {
		t.Fatalf("missing name = %s, want empty lists", raw)
	}

	orphans := scopeLsReply(api.LsReply{
		Checkouts: []api.Checkout{{Name: "y"}},
		Snapshots: []api.Snapshot{{ID: "s3", Checkout: "gone", Orphan: true}},
	}, "gone")
	if len(orphans.Checkouts) != 0 || len(orphans.Snapshots) != 1 || orphans.Snapshots[0].ID != "s3" {
		t.Fatalf("orphan scope = %+v", orphans)
	}
}
