package exportgen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/openclaw/notcrawl/internal/store"
)

func seedArchive(t *testing.T, pages int) string {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "notcrawl.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := store.NowMS()
	for i := 0; i < pages; i++ {
		id := fmt.Sprintf("page-%d", i)
		if err := st.UpsertPage(ctx, store.Page{ID: id, Title: "Page " + id, Alive: true, Source: "test", SyncedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertBlock(ctx, store.Block{
			ID: id + "-block", PageID: id, ParentID: id, Type: "paragraph",
			Text: "body " + id, Alive: true, Source: "test", SyncedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func testOptions(t *testing.T, dbPath, root string, render func(context.Context, *store.Store, Item, string) (FileResult, error)) Options {
	t.Helper()
	return Options{
		Kind:       KindMarkdown,
		DBPath:     dbPath,
		CacheDir:   filepath.Join(t.TempDir(), "cache"),
		OutputRoot: root,
		Plan: func(ctx context.Context, snapshot *store.Store) ([]Item, error) {
			pages, err := snapshot.Pages(ctx)
			if err != nil {
				return nil, err
			}
			items := make([]Item, 0, len(pages))
			for _, page := range pages {
				items = append(items, Item{
					ID:           page.ID,
					RelativePath: page.ID + ".txt",
					SourceDigest: DigestHash("test", page.ID, page.Title),
				})
			}
			return items, nil
		},
		Render: render,
		Owned:  func(string) bool { return true },
	}
}

func renderFromTitle(suffix string) func(context.Context, *store.Store, Item, string) (FileResult, error) {
	return func(ctx context.Context, snapshot *store.Store, item Item, stagingPath string) (FileResult, error) {
		return FileResult{Data: []byte(item.ID + " " + suffix + "\n")}, nil
	}
}

func TestRunPublishesManifestAndVerifiableFiles(t *testing.T) {
	ctx := context.Background()
	dbPath := seedArchive(t, 3)
	root := filepath.Join(t.TempDir(), "out")
	renderCount := 0
	opts := testOptions(t, dbPath, root, func(ctx context.Context, st *store.Store, item Item, stagingPath string) (FileResult, error) {
		renderCount++
		return renderFromTitle("v1")(ctx, st, item, stagingPath)
	})
	manifest, err := Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 3 {
		t.Fatalf("manifest files = %d, want 3", len(manifest.Files))
	}
	if manifest.Snapshot.SHA256 == "" || manifest.Snapshot.Size == 0 {
		t.Fatalf("snapshot not fingerprinted: %+v", manifest.Snapshot)
	}
	if _, err := os.Stat(manifest.Snapshot.Path); !os.IsNotExist(err) {
		t.Fatalf("temporary snapshot was not removed: %s", manifest.Snapshot.Path)
	}
	for _, file := range manifest.Files {
		published := filepath.Join(root, filepath.FromSlash(file.Path))
		hash, size, err := sha256FileAt(published)
		if err != nil {
			t.Fatal(err)
		}
		if hash != file.SHA256 || size != file.Size {
			t.Fatalf("published file %s does not match manifest", file.Path)
		}
		if file.SourceDigest == "" {
			t.Fatalf("file %s missing source digest", file.Path)
		}
	}
	persisted, err := readManifestAt(root, KindMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Generation != manifest.Generation {
		t.Fatalf("manifest generation mismatch: %s vs %s", persisted.Generation, manifest.Generation)
	}
	if renderCount != 3 {
		t.Fatalf("render count = %d, want 3", renderCount)
	}
	// No staging directories survive a successful run.
	entries, _ := os.ReadDir(filepath.Join(root, StateDirName, KindMarkdown))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "tmp-") {
			t.Fatalf("staging directory survived: %s", entry.Name())
		}
	}
}

func TestRunRerunReusesUnchangedItems(t *testing.T) {
	ctx := context.Background()
	dbPath := seedArchive(t, 2)
	root := filepath.Join(t.TempDir(), "out")
	rendered := map[string]int{}
	render := func(ctx context.Context, st *store.Store, item Item, stagingPath string) (FileResult, error) {
		rendered[item.ID]++
		return FileResult{Data: []byte(item.ID + "\n")}, nil
	}
	opts := testOptions(t, dbPath, root, render)
	first, err := Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation == second.Generation {
		t.Fatal("rerun did not create a new generation")
	}
	for id, count := range rendered {
		if count != 1 {
			t.Fatalf("item %s rendered %d times, want 1 (reuse)", id, count)
		}
	}
	reused := 0
	for _, file := range second.Files {
		if !file.Reused {
			t.Fatalf("file %s was not reused on unchanged rerun", file.Path)
		}
		reused++
	}
	if reused != 2 {
		t.Fatalf("reused = %d, want 2", reused)
	}
}

func TestRunChangedDigestReRendersItem(t *testing.T) {
	ctx := context.Background()
	dbPath := seedArchive(t, 2)
	root := filepath.Join(t.TempDir(), "out")
	version := "v1"
	render := func(ctx context.Context, st *store.Store, item Item, stagingPath string) (FileResult, error) {
		return FileResult{Data: []byte(item.ID + "-" + version + "\n")}, nil
	}
	opts := testOptions(t, dbPath, root, render)
	if _, err := Run(ctx, opts); err != nil {
		t.Fatal(err)
	}
	// Change the source digest of one item while keeping rendered bytes stable
	// for the other.
	basePlan := opts.Plan
	opts.Plan = func(ctx context.Context, snapshot *store.Store) ([]Item, error) {
		items, err := basePlan(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		for i := range items {
			if items[i].ID == "page-0" {
				items[i].SourceDigest = items[i].SourceDigest + "-changed"
			}
		}
		return items, nil
	}
	version = "v2"
	manifest, err := Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]FileManifest{}
	for _, file := range manifest.Files {
		byID[file.ID] = file
	}
	if byID["page-0"].Reused {
		t.Fatal("changed item was reused instead of re-rendered")
	}
	if !byID["page-1"].Reused {
		t.Fatal("unchanged item was re-rendered")
	}
}

func TestRunFailureKeepsPreviousGeneration(t *testing.T) {
	ctx := context.Background()
	dbPath := seedArchive(t, 3)
	root := filepath.Join(t.TempDir(), "out")
	opts := testOptions(t, dbPath, root, renderFromTitle("v1"))
	first, err := Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	previousBytes := map[string][]byte{}
	for _, file := range first.Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		previousBytes[file.Path] = data
	}

	// Fail the second generation after two items have staged successfully.
	injected := errors.New("simulated render failure")
	failIndex := 1
	opts.Hooks = FaultHooks{
		AfterItem: func(item Item, attempt int) error {
			if attempt == failIndex {
				return injected
			}
			return nil
		},
	}
	if _, err := Run(ctx, opts); !errors.Is(err, injected) {
		t.Fatalf("want injected failure, got %v", err)
	}

	// The visible output and manifest remain exactly the first generation.
	persisted, err := readManifestAt(root, KindMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Generation != first.Generation {
		t.Fatal("manifest switched despite failed generation")
	}
	for path, want := range previousBytes {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("published file %s changed after failed generation", path)
		}
	}
}

func TestRunFailureCleansOrphansOnRerun(t *testing.T) {
	ctx := context.Background()
	dbPath := seedArchive(t, 2)
	root := filepath.Join(t.TempDir(), "out")
	opts := testOptions(t, dbPath, root, renderFromTitle("v1"))
	first, err := Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}

	// Plant an orphan staging directory like a crashed generation would leave.
	kindDir := filepath.Join(root, StateDirName, KindMarkdown)

	// A failing run removes its own staging via its cleanup defer.
	opts.Hooks = FaultHooks{
		AfterStaging: func(string) error { return errors.New("boom") },
	}
	if _, err := Run(ctx, opts); err == nil {
		t.Fatal("expected injected staging failure")
	}
	entries, _ := os.ReadDir(kindDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "tmp-") {
			t.Fatalf("failed run left its own staging behind: %s", entry.Name())
		}
	}

	// Simulate a hard crash whose cleanup defer never ran.
	orphan := filepath.Join(kindDir, "tmp-orphan123")
	if err := os.MkdirAll(filepath.Join(orphan, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "nested", "ghost.txt"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Successful rerun reclaims every stale staging directory, including the
	// crash orphan that no cleanup defer ever touched.
	opts.Hooks = FaultHooks{}
	second, err := Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation == first.Generation {
		t.Fatal("rerun reused the old generation id")
	}
	entries, _ = os.ReadDir(kindDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "tmp-") {
			t.Fatalf("orphan staging survived successful rerun: %s", entry.Name())
		}
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("planted orphan was not reclaimed")
	}
}

func TestRunNeverDeletesUnmanagedUserFiles(t *testing.T) {
	ctx := context.Background()
	dbPath := seedArchive(t, 1)
	root := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	userNote := filepath.Join(root, "notes.md")
	userData := []byte("my own notes, leave me alone\n")
	if err := os.WriteFile(userNote, userData, 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(root, "data.json")
	if err := os.WriteFile(sidecar, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(t, dbPath, root, renderFromTitle("v1"))
	// Only .txt files are considered owned by this exporter.
	opts.Owned = func(path string) bool { return filepath.Ext(path) == ".txt" }
	if _, err := Run(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(userNote); err != nil || string(got) != string(userData) {
		t.Fatalf("user markdown removed or changed: %v", err)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar file removed: %v", err)
	}
}

func TestRunPrunesDroppedManagedFiles(t *testing.T) {
	ctx := context.Background()
	dbPath := seedArchive(t, 2)
	root := filepath.Join(t.TempDir(), "out")
	opts := testOptions(t, dbPath, root, renderFromTitle("v1"))
	first, err := Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	dropped := first.Files[0].Path
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dropped))); err != nil {
		t.Fatal(err)
	}
	// Second generation only emits page-1.
	basePlan := opts.Plan
	opts.Plan = func(ctx context.Context, snapshot *store.Store) ([]Item, error) {
		items, err := basePlan(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		filtered := items[:0]
		for _, item := range items {
			if item.ID != first.Files[0].ID {
				filtered = append(filtered, item)
			}
		}
		return filtered, nil
	}
	if _, err := Run(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dropped))); !os.IsNotExist(err) {
		t.Fatalf("dropped managed file was not pruned: %s", dropped)
	}
}

func TestRunSnapshotIsolationSeesFrozenContent(t *testing.T) {
	ctx := context.Background()
	dbPath := seedArchive(t, 1)
	root := filepath.Join(t.TempDir(), "out")
	opts := testOptions(t, dbPath, root, renderFromTitle("v1"))
	if _, err := Run(ctx, opts); err != nil {
		t.Fatal(err)
	}
	// Mutate the live archive while the next run holds its snapshot; the
	// snapshot must still expose the original rows. The hook fires after the
	// backup has completed, so the mutation cannot enter the snapshot.
	verifyFrozen := false
	opts.Hooks = FaultHooks{
		AfterSnapshot: func(snapshotPath string) error {
			live, err := store.Open(dbPath)
			if err != nil {
				return err
			}
			defer live.Close()
			if err := live.UpsertPage(ctx, store.Page{ID: "page-0", Title: "MUTATED TITLE", Alive: true, Source: "test", SyncedAt: store.NowMS()}); err != nil {
				return err
			}
			return live.UpsertPage(ctx, store.Page{ID: "page-extra", Title: "Late arrival", Alive: true, Source: "test", SyncedAt: store.NowMS()})
		},
	}
	basePlan := opts.Plan
	opts.Plan = func(ctx context.Context, snapshot *store.Store) ([]Item, error) {
		pages, err := snapshot.Pages(ctx)
		if err != nil {
			return nil, err
		}
		if verifyFrozen {
			ids := make([]string, 0, len(pages))
			for _, page := range pages {
				ids = append(ids, page.ID)
				if page.Title == "MUTATED TITLE" {
					t.Fatal("snapshot observed a mutation made after capture")
				}
			}
			sort.Strings(ids)
			if strings.Join(ids, ",") != "page-0" {
				t.Fatalf("snapshot saw rows added after capture: %v", ids)
			}
		}
		return basePlan(ctx, snapshot)
	}
	verifyFrozen = true
	manifest, err := Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 {
		t.Fatalf("frozen snapshot produced %d files, want 1", len(manifest.Files))
	}
	// A subsequent run without the mutation hook observes the new live state.
	opts.Hooks = FaultHooks{}
	verifyFrozen = false
	manifest, err = Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 2 {
		t.Fatalf("later generation should see the added page, got %d files", len(manifest.Files))
	}
}
