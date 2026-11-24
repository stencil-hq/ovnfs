package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

// selectFixture opens app and app-edit (with a source change) on one folder and
// other on a second, all created three hours ago.
func selectFixture(t *testing.T) (f *fixture, app string) {
	t.Helper()
	f = newFixture(t)
	app = folder(t, map[string]string{"a": "base", "sub/b": "b"})
	f.newCheckout(app, "app")
	f.newCheckout(app, "app-edit")
	f.newCheckout(folder(t, map[string]string{"a": "other"}), "other")
	f.put("app-edit", "a", "edited")
	f.reg.mu.Lock()
	for _, h := range f.reg.checkouts {
		h.meta.Created = h.meta.Created.Add(-3 * time.Hour)
	}
	f.reg.mu.Unlock()
	return f, app
}

// picked lists the names sel picks.
func (f *fixture) picked(sel api.Select) []string {
	f.t.Helper()
	reply, err := f.reg.Ls(context.Background(), api.LsArgs{Select: sel})
	if err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for _, c := range reply.Checkouts {
		names = append(names, c.Name)
	}
	return names
}

// TestSelect checks each criterion and that given together they all must hold.
func TestSelect(t *testing.T) {
	f, app := selectFixture(t)
	var s Session
	if err := f.reg.Enter("other", &s); err != nil {
		t.Fatal(err)
	}
	s.Touch()
	s.Done()
	for _, tt := range []struct {
		name string
		sel  api.Select
		want []string
	}{
		{"folder", api.Select{Dir: app}, []string{"app", "app-edit"}},
		{"folder inside the source", api.Select{Dir: filepath.Join(app, "sub")}, []string{"app", "app-edit"}},
		{"folder holding the sources", api.Select{Dir: filepath.Dir(app)}, []string{"app", "app-edit", "other"}},
		{"unrelated folder", api.Select{Dir: f.state}, nil},
		{"inside a checkout", api.Select{Dir: filepath.Join(f.mount, "app-edit", "sub")}, []string{"app", "app-edit"}},
		{"the mount", api.Select{Dir: f.mount}, []string{"app", "app-edit", "other"}},
		{"idle", api.Select{Idle: time.Hour.Seconds()}, []string{"app", "app-edit"}},
		{"no diff", api.Select{NoDiff: true}, []string{"app", "other"}},
		{"all criteria", api.Select{Dir: app, Idle: time.Hour.Seconds(), NoDiff: true}, []string{"app"}},
	} {
		if got := f.picked(tt.sel); !slices.Equal(got, tt.want) {
			t.Errorf("%s: picked %v, want %v", tt.name, got, tt.want)
		}
	}
	if _, err := f.reg.Ls(context.Background(), api.LsArgs{Select: api.Select{Dir: filepath.Join(f.mount, "gone")}}); !isCode(err, "not_found") {
		t.Errorf("folder of a missing checkout in the mount: %v", err)
	}
}

// TestPrune checks that prune refuses to pick everything, that a dry run removes
// nothing, and that a checkout touched after it was picked stays.
func TestPrune(t *testing.T) {
	ctx := context.Background()
	f, app := selectFixture(t)
	if _, err := f.reg.Prune(ctx, api.PruneArgs{}); !isCode(err, "invalid") {
		t.Fatalf("prune without criteria: %v", err)
	}
	dry, err := f.reg.Prune(ctx, api.PruneArgs{Select: api.Select{NoDiff: true}, DryRun: true})
	if err != nil || len(dry.Removed) != 2 || len(f.ls().Checkouts) != 3 {
		t.Fatalf("dry run = %+v, %v; %d checkouts left", dry, err, len(f.ls().Checkouts))
	}
	picked := f.ls().Checkouts[0]
	var s Session
	if err := f.reg.Enter(picked.Name, &s); err != nil {
		t.Fatal(err)
	}
	s.Touch()
	s.Done()
	if removed, err := f.reg.remove(ctx, api.RmArgs{Checkout: picked.Name}, picked.Touched); removed || err != nil {
		t.Fatalf("removing %s touched after it was picked = %v, %v", picked.Name, removed, err)
	}
	reply, err := f.reg.Prune(ctx, api.PruneArgs{Select: api.Select{Dir: app}})
	if err != nil || len(reply.Removed) != 2 {
		t.Fatalf("prune %s = %+v, %v", app, reply, err)
	}
	if got := f.picked(api.Select{}); !slices.Equal(got, []string{"other"}) {
		t.Fatalf("left %v, want other", got)
	}
}

// TestRemovalSnapshots checks both removal commands and their opt-in retention.
func TestRemovalSnapshots(t *testing.T) {
	for _, prune := range []bool{false, true} {
		for _, sticky := range []bool{false, true} {
			name := fmt.Sprintf("prune=%t/sticky=%t", prune, sticky)
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				f := newFixture(t)
				dir := folder(t, map[string]string{"a": "base"})
				f.newCheckout(dir, "app")
				f.put("app", "a", "changed")
				snap, err := f.reg.Snap(ctx, api.SnapArgs{Checkout: "app", Wait: true})
				if err != nil {
					t.Fatal(err)
				}
				f.reg.mu.RLock()
				layer := f.reg.snapshots[snap.Snapshot.ID].layer
				f.reg.mu.RUnlock()
				if prune {
					_, err = f.reg.Prune(ctx, api.PruneArgs{Select: api.Select{Dir: dir}, StickySnapshots: sticky})
				} else {
					err = f.reg.Rm(ctx, api.RmArgs{Checkout: "app", StickySnapshots: sticky})
				}
				if err != nil {
					t.Fatal(err)
				}
				f.reg.Close()
				f.open()
				snapshots := f.ls().Snapshots
				if sticky {
					if len(snapshots) != 1 || snapshots[0].ID != snap.Snapshot.ID {
						t.Fatalf("retained snapshots = %+v", snapshots)
					}
					if _, err := f.reg.New(ctx, api.NewArgs{Name: "restored", Snapshot: snap.Snapshot.ID}); err != nil {
						t.Fatal(err)
					}
					if got := f.read("restored", "a"); got != "changed" {
						t.Fatalf("restored content = %q", got)
					}
				} else {
					if len(snapshots) != 0 {
						t.Fatalf("snapshots after removal = %+v", snapshots)
					}
					eventually(t, "unreferenced layer remains on disk", func() bool {
						_, err := os.Stat(filepath.Join(f.state, "layers", layer))
						return os.IsNotExist(err)
					})
				}
			})
		}
	}
}

// TestRemovalKeepsSharedSnapshots preserves ancestry still used by another checkout.
func TestRemovalKeepsSharedSnapshots(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := folder(t, map[string]string{"a": "base"})
	f.newCheckout(dir, "parent")
	f.put("parent", "a", "shared")
	child, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "parent", Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Rm(ctx, api.RmArgs{Checkout: "parent"}); err != nil {
		t.Fatal(err)
	}
	f.reg.Close()
	f.open()
	if got := f.read("child", "a"); got != "shared" {
		t.Fatalf("child content after removal = %q", got)
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Name: "restored", Snapshot: child.Snapshot.ID}); err != nil {
		t.Fatalf("shared snapshot was removed: %v", err)
	}
}

func TestRemovalKeepsEarlierCheckoutSnapshots(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, prune := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy=%t/prune=%t", legacy, prune), func(t *testing.T) {
				ctx := t.Context()
				f := newFixture(t)
				dir := folder(t, map[string]string{"a": "base"})
				f.newCheckout(dir, "probe")
				f.put("probe", "a", "kept")
				kept, err := f.reg.Snap(ctx, api.SnapArgs{Checkout: "probe", Wait: true})
				if err != nil {
					t.Fatal(err)
				}
				id := kept.Snapshot.ID
				if err := f.reg.Rm(ctx, api.RmArgs{Checkout: "probe", StickySnapshots: true}); err != nil {
					t.Fatal(err)
				}
				if legacy {
					// Even a same-name inherited parent is not owned by this checkout.
					if _, err := f.reg.New(ctx, api.NewArgs{Name: "probe", Snapshot: id}); err != nil {
						t.Fatal(err)
					}
				} else {
					f.newCheckout(dir, "probe")
				}
				f.put("probe", "a", "discarded")
				current, err := f.reg.Snap(ctx, api.SnapArgs{Checkout: "probe", Wait: true})
				if err != nil {
					t.Fatal(err)
				}
				fork, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "probe", Name: "child", Label: "branch point"})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.reg.Rm(ctx, api.RmArgs{Checkout: "child"}); err != nil {
					t.Fatal(err)
				}
				if err := f.reg.Close(); err != nil {
					t.Fatal(err)
				}
				if legacy {
					d := readDisk(t, f.state)
					for i := range d.Snapshots {
						d.Snapshots[i].Life = 0
					}
					if err := writeState(f.state, d); err != nil {
						t.Fatal(err)
					}
				}
				f.open()
				selected, err := f.reg.Ls(ctx, api.LsArgs{Select: api.Select{Dir: dir}})
				if err != nil {
					t.Fatal(err)
				}
				snaps := selected.Snapshots
				if len(snaps) != 2 || snaps[0].ID != current.Snapshot.ID || snaps[1].ID != fork.Snapshot.ID || snaps[1].Label != "branch point" {
					t.Fatalf("selected snapshots = %+v, want %s and %s", snaps, current.Snapshot.ID, fork.Snapshot.ID)
				}
				if all := f.ls().Snapshots; len(all) != 3 || all[0].ID != id || !all[0].Orphan || all[1].Orphan || all[2].Orphan {
					t.Fatalf("all snapshots = %+v, want only %s orphaned", all, id)
				}

				if prune {
					_, err = f.reg.Prune(ctx, api.PruneArgs{Select: api.Select{Dir: dir}})
				} else {
					err = f.reg.Rm(ctx, api.RmArgs{Checkout: "probe"})
				}
				if err != nil {
					t.Fatal(err)
				}
				ls := f.ls()
				if len(ls.Checkouts) != 0 || len(ls.Snapshots) != 1 || ls.Snapshots[0].ID != id {
					t.Fatalf("checkouts = %+v, snapshots = %+v; want no checkouts and only %s", ls.Checkouts, ls.Snapshots, id)
				}
				if _, err := f.reg.New(ctx, api.NewArgs{Name: "restored", Snapshot: id}); err != nil {
					t.Fatal(err)
				}
				if got := f.read("restored", "a"); got != "kept" {
					t.Fatalf("restored content = %q", got)
				}
				if err := f.reg.Drop(ctx, api.DropArgs{Snapshot: id}); err != nil {
					t.Fatal(err)
				}
				if snapshots := f.ls().Snapshots; len(snapshots) != 0 {
					t.Fatalf("snapshots after explicit drop = %+v", snapshots)
				}
			})
		}
	}
}

// isCode reports whether err is a registry error with code.
func isCode(err error, code string) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Code == code
}
