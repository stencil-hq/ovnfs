package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/store"
)

func TestSearchMergedCheckoutWithoutMount(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{
		"base.txt": "needle original\n", "gone.txt": "needle deleted\n", "sub/lower.txt": "needle lower\n",
	})
	f.newCheckout(dir, "search")
	f.remove("search", "gone.txt")
	f.put("search", "sub/added.txt", "needle layer\n")
	if _, err := f.reg.Snap(t.Context(), api.SnapArgs{Checkout: "search", Wait: true}); err != nil {
		t.Fatal(err)
	}
	f.put("search", "base.txt", "header\nneedle memory\ntail\n")
	if _, err := os.Stat(filepath.Join(f.mount, "search")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture unexpectedly has a mounted checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.state, "uppers", "search", "base.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("written content should still be in memory: %v", err)
	}
	got, err := f.reg.Grep(t.Context(), api.GrepArgs{
		Checkout: "search", Pattern: "needle", Before: 1, After: 1,
	})
	want := []api.GrepMatch{
		{Path: "base.txt", Line: 2, Text: "needle memory", Before: []api.ContextLine{{Line: 1, Text: "header"}}, After: []api.ContextLine{{Line: 3, Text: "tail"}}},
		{Path: "sub/added.txt", Line: 1, Text: "needle layer"},
		{Path: "sub/lower.txt", Line: 1, Text: "needle lower"},
	}
	if err != nil || !reflect.DeepEqual(got.Matches, want) {
		t.Fatalf("merged grep: %+v, %v", got, err)
	}
	paths, err := f.reg.Glob(t.Context(), api.GlobArgs{Dir: filepath.Join(f.mount, "search"), Pattern: "*.txt"})
	if err != nil || !slices.Equal(globPaths(paths), []string{"base.txt", "sub/added.txt", "sub/lower.txt"}) {
		t.Fatalf("mounted-path glob: %+v, %v", paths, err)
	}
	sub, err := f.reg.Grep(t.Context(), api.GrepArgs{Dir: filepath.Join(f.mount, "search", "sub"), Pattern: "needle", MaxCount: 1})
	if err != nil || len(sub.Matches) != 1 || sub.Matches[0].Path != "added.txt" || sub.Matches[0].Text != "needle layer" || !sub.LimitReached {
		t.Fatalf("mounted subdirectory grep: %+v, %v", sub, err)
	}
	one, err := f.reg.Grep(t.Context(), api.GrepArgs{Checkout: "search", Path: "base.txt", Pattern: "needle"})
	if err != nil || len(one.Matches) != 1 || one.Matches[0].Text != "needle memory" {
		t.Fatalf("single file grep: %+v, %v", one, err)
	}
}

func TestSearchIgnoreRulesAndLivePaths(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{
		".gitignore": "ignored/\n", "keep.txt": "needle\n", "ignored/live.txt": "old\n",
		".hidden.txt": "needle\n", "secret.txt": "needle\n", "node_modules/pkg/index.js": "needle\n",
	})
	// Local, not cache: a checkout hides cache files the folder changes after it opens.
	f.newCheckout(dir, "search", "hidden secret.txt", "local ignored & dir")
	write(t, filepath.Join(dir, "ignored", "live.txt"), "needle live\n")
	defaults, err := f.reg.Glob(t.Context(), api.GlobArgs{Checkout: "search", Pattern: "*.txt"})
	if err != nil || !slices.Equal(globPaths(defaults), []string{"keep.txt"}) {
		t.Fatalf("default ignores: %+v, %v", defaults, err)
	}
	all, err := f.reg.Grep(t.Context(), api.GrepArgs{
		Checkout: "search", Hidden: true, NoIgnore: true, NodeModules: true, Pattern: "needle",
	})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, match := range all.Matches {
		paths = append(paths, match.Path)
		if match.Path == "ignored/live.txt" && match.Text != "needle live" {
			t.Fatalf("live path was searched through captured content: %+v", match)
		}
	}
	if !slices.Equal(paths, []string{".hidden.txt", "ignored/live.txt", "keep.txt", "node_modules/pkg/index.js"}) {
		t.Fatalf("visibility overrides must not reveal hidden-class paths: %v", paths)
	}
	node, err := f.reg.Glob(t.Context(), api.GlobArgs{Checkout: "search", Pattern: "node_modules/**/*.js"})
	if err != nil || !slices.Equal(globPaths(node), []string{"node_modules/pkg/index.js"}) {
		t.Fatalf("explicit node_modules pattern: %+v, %v", node, err)
	}
}

// TestSearchSkipsAppleDoubleSidecars checks that searches see the listing a
// client of the mount sees: no AppleDouble sidecars.
func TestSearchSkipsAppleDoubleSidecars(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "needle\n"})
	f.newCheckout(dir, "search")
	f.put("search", "._a.txt", "needle\n")
	got, err := f.reg.Glob(t.Context(), api.GlobArgs{Checkout: "search", Hidden: true, Pattern: "*"})
	if err != nil || !slices.Equal(globPaths(got), []string{"a.txt"}) {
		t.Fatalf("glob: %+v, %v", got, err)
	}
	found, err := f.reg.Grep(t.Context(), api.GrepArgs{Checkout: "search", Hidden: true, Pattern: "needle"})
	if err != nil || len(found.Matches) != 1 || found.Matches[0].Path != "a.txt" {
		t.Fatalf("grep: %+v, %v", found, err)
	}
}

func TestSearchRootBoundaries(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"sub/a.go": "needle\n", "b.go": "needle\n"})
	f.newCheckout(dir, "search")
	got, err := f.reg.Glob(t.Context(), api.GlobArgs{Dir: dir, Path: "sub", Pattern: "*.go", Type: "file", SortByMtime: true})
	if err != nil || !slices.Equal(globPaths(got), []string{"a.go"}) || got.Matches[0].Size != 7 || got.Matches[0].Mtime == 0 {
		t.Fatalf("native subdirectory search: %+v, %v", got, err)
	}
	for _, args := range []api.SearchArgs{
		{}, {Dir: dir, Checkout: "search"}, {Checkout: "search", Path: "../outside"},
		{Checkout: "search", Path: "/outside"}, {Dir: f.mount}, {Dir: "relative"},
	} {
		_, err := f.reg.Glob(t.Context(), api.GlobArgs{SearchArgs: args, Pattern: "*"})
		var public *Error
		if !errors.As(err, &public) || public.Code != "invalid" {
			t.Errorf("accepted invalid root %+v: %v", args, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.reg.Grep(ctx, api.GrepArgs{Checkout: "search", Pattern: "needle"})
	var public *Error
	if !errors.As(err, &public) || public.Code != "unavailable" {
		t.Fatalf("canceled search: %v", err)
	}
}

func TestSearchCheckoutScopeExcludeAndWindow(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("a", 600) + "needle" + strings.Repeat("b", 600)
	dir := folder(t, map[string]string{
		"packages/a/src/x.ts": "needle a\n", "packages/a/src/gen/y.ts": "needle gen\n",
		"packages/b/src/z.ts": "needle b\n", "packages/b/lib/w.ts": "needle lib\n",
		"packages/b/src/z.test.ts": "needle test\n", "top.ts": "needle top\n",
	})
	f.newCheckout(dir, "search")
	f.put("search", "packages/a/src/long.ts", long+"\n")
	got, err := f.reg.Grep(t.Context(), api.GrepArgs{
		Dir: filepath.Join(f.mount, "search"), Pattern: "needle",
		Glob: "packages/*/src", Exclude: []string{"gen/", "*.test.ts"}, MaxColumns: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, m := range got.Matches {
		paths = append(paths, m.Path)
		if m.Path == "packages/a/src/long.ts" && (!m.Truncated || m.Column != 601 || !strings.HasPrefix(m.Text, "...") ||
			!strings.HasSuffix(m.Text, "...") || !strings.Contains(m.Text, "needle") || len(m.Text) > 40) {
			t.Errorf("windowed long line: %+v", m)
		}
	}
	if want := []string{"packages/a/src/long.ts", "packages/a/src/x.ts", "packages/b/src/z.ts"}; !slices.Equal(paths, want) {
		t.Fatalf("scoped grep with exclude: %v, want %v", paths, want)
	}
}

// TestSearchRemoteCheckoutScansAtTheSource checks grep and glob in a checkout
// attached from another machine: they answer as the source checkout does,
// unfetched files are scanned where their bytes are, so only the ignore
// files the walk reads cross the link, and files written on the remote are
// searched there.
func TestSearchRemoteCheckoutScansAtTheSource(t *testing.T) {
	src := newFixture(t)
	files := map[string]string{".gitignore": "*.log\n", "sub/.gitignore": "*.tmp\n"}
	for i := range 40 {
		files[fmt.Sprintf("sub/f%02d.txt", i)] = fmt.Sprintf("line %d\nneedle %d\n", i, i)
	}
	src.newCheckout(folder(t, files), "app")
	l := attachOverPipe(t, src, "app")

	for _, args := range []api.GrepArgs{
		{Pattern: `needle \d+`, Before: 1},
		{Pattern: `needle \d+`, Offset: 5, MaxCount: 3},
		{Pattern: "needle", Mode: "count", Path: "sub"},
	} {
		args.Checkout = "app"
		want, err := src.reg.Grep(t.Context(), args)
		if err != nil {
			t.Fatal(err)
		}
		args.Checkout = l.name
		got, err := l.reg.Grep(t.Context(), args)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("remote grep %+v:\n got %+v, %v\nwant %+v", args, got, err, want)
		}
	}
	globbed, err := l.reg.Glob(t.Context(), api.GlobArgs{Checkout: l.name, Pattern: "*.txt"})
	if err != nil || len(globbed.Matches) != 40 {
		t.Fatalf("remote glob: %+v, %v", globbed, err)
	}
	for p, data := range files {
		fetched := l.reg.Store().Has(store.HashBytes("blob", []byte(data)))
		if fetched != strings.HasSuffix(p, ".gitignore") {
			t.Errorf("%s fetched = %v; searches fetch only ignore files", p, fetched)
		}
	}

	(&fixture{t: t, reg: l.reg}).put(l.name, "sub/f00.txt", "needle 999\n")
	got, err := l.reg.Grep(t.Context(), api.GrepArgs{Checkout: l.name, Pattern: `needle \d+`, MaxCount: 1})
	if err != nil || len(got.Matches) != 1 || got.Matches[0].Text != "needle 999" {
		t.Fatalf("grep after a remote write: %+v, %v", got, err)
	}
}

func globPaths(reply api.GlobReply) []string {
	paths := make([]string, len(reply.Matches))
	for i, match := range reply.Matches {
		paths[i] = match.Path
	}
	return paths
}
