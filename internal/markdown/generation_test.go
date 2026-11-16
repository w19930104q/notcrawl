package markdown

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/notcrawl/internal/exportgen"
	"github.com/openclaw/notcrawl/internal/store"
)

func readExportManifest(t *testing.T, dir string) exportgen.Manifest {
	t.Helper()
	path := filepath.Join(dir, exportgen.StateDirName, exportgen.KindMarkdown, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest exportgen.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func seedGenerationStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "notcrawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := store.NowMS()
	if err := st.UpsertSpace(ctx, store.Space{ID: "space1", Name: "Engineering", Source: "test", SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct{ id, title, body string }{
		{"page1", "Launch Plan", "ship it"},
		{"page2", "Retro", "what we learned"},
		{"page3", "Hiring", "open roles"},
	} {
		if err := st.UpsertPage(ctx, store.Page{ID: page.id, SpaceID: "space1", Title: page.title, Alive: true, Source: "test", SyncedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertBlock(ctx, store.Block{ID: page.id + "-b", PageID: page.id, ParentID: page.id, Type: "paragraph", Text: page.body, Alive: true, Source: "test", SyncedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestExportGenerationReusesUnchangedPages(t *testing.T) {
	ctx := context.Background()
	st := seedGenerationStore(t)
	defer st.Close()
	dir := t.TempDir()
	exporter := Exporter{Store: st, Dir: dir, CacheDir: filepath.Join(t.TempDir(), "cache")}

	first, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Pages != 3 || first.Reused != 0 {
		t.Fatalf("first export: %+v", first)
	}
	firstBytes := map[string]string{}
	for _, path := range first.Files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		firstBytes[path] = string(data)
	}

	second, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation == first.Generation {
		t.Fatal("rerun did not start a new generation")
	}
	if second.Reused != 3 {
		t.Fatalf("unchanged rerun reused %d pages, want 3", second.Reused)
	}
	for path, want := range firstBytes {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("page %s changed despite unchanged source", path)
		}
	}

	manifest := readExportManifest(t, dir)
	if manifest.Generation != second.Generation || len(manifest.Files) != 3 {
		t.Fatalf("persisted manifest mismatch: %+v", manifest)
	}
	if manifest.Snapshot.SHA256 == "" {
		t.Fatal("manifest lacks snapshot fingerprint")
	}
	if _, err := os.Stat(manifest.Snapshot.Path); !os.IsNotExist(err) {
		t.Fatal("temporary snapshot file was not removed")
	}
}

func TestExportGenerationFailureKeepsPreviousOutput(t *testing.T) {
	ctx := context.Background()
	st := seedGenerationStore(t)
	defer st.Close()
	dir := t.TempDir()
	exporter := Exporter{Store: st, Dir: dir, CacheDir: filepath.Join(t.TempDir(), "cache")}
	first, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstManifest := readExportManifest(t, dir)
	previous := map[string]string{}
	for _, path := range first.Files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		previous[path] = string(data)
	}

	injected := errors.New("render boom")
	exporter.Hooks = exportgen.FaultHooks{
		AfterItem: func(item exportgen.Item, attempt int) error {
			if attempt == 1 {
				return injected
			}
			return nil
		},
	}
	if _, err := exporter.Export(ctx); !errors.Is(err, injected) {
		t.Fatalf("want injected failure, got %v", err)
	}

	// Previous generation remains published byte-for-byte.
	for path, want := range previous {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("previous output lost after failure: %v", err)
		}
		if string(got) != want {
			t.Fatalf("previous output changed after failed generation")
		}
	}
	persisted := readExportManifest(t, dir)
	if persisted.Generation != firstManifest.Generation {
		t.Fatal("manifest switched despite failed generation")
	}

	// A failed generation's staging is removed by its cleanup defer. A hard
	// crash would leave a tmp- directory behind; plant one to prove the
	// successful rerun sweeps such orphans before starting.
	kindDir := filepath.Join(dir, exportgen.StateDirName, exportgen.KindMarkdown)
	orphan := filepath.Join(kindDir, "tmp-crashed")
	if err := os.MkdirAll(filepath.Join(orphan, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "nested", "ghost.md"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	exporter.Hooks = exportgen.FaultHooks{}
	second, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Pages != 3 {
		t.Fatalf("rerun after failure: %+v", second)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("crash orphan staging was not reclaimed")
	}
	entries, err := os.ReadDir(kindDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "tmp-") {
			t.Fatalf("orphan staging survived rerun: %s", entry.Name())
		}
	}
}

func TestExportGenerationReRendersChangedPage(t *testing.T) {
	ctx := context.Background()
	st := seedGenerationStore(t)
	defer st.Close()
	dir := t.TempDir()
	exporter := Exporter{Store: st, Dir: dir, CacheDir: filepath.Join(t.TempDir(), "cache")}
	if _, err := exporter.Export(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBlock(ctx, store.Block{ID: "page1-b", PageID: "page1", ParentID: "page1", Type: "paragraph", Text: "updated body", Alive: true, Source: "test", SyncedAt: store.NowMS() + 1}); err != nil {
		t.Fatal(err)
	}
	summary, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Reused != 2 {
		t.Fatalf("expected 2 reused pages, got %d", summary.Reused)
	}
	var updated string
	for _, path := range summary.Files {
		if strings.HasPrefix(filepath.Base(path), "launch-plan-") {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			updated = string(data)
		}
	}
	if !strings.Contains(updated, "updated body") {
		t.Fatalf("changed page was not re-rendered:\n%s", updated)
	}
}

func TestExportGenerationPrunesDroppedPageButKeepsUserFiles(t *testing.T) {
	ctx := context.Background()
	st := seedGenerationStore(t)
	defer st.Close()
	dir := t.TempDir()
	exporter := Exporter{Store: st, Dir: dir, CacheDir: filepath.Join(t.TempDir(), "cache")}
	first, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A user-owned Markdown file without notcrawl front matter must survive.
	userNote := filepath.Join(dir, "engineering", "manual-notes.md")
	if err := os.MkdirAll(filepath.Dir(userNote), 0o755); err != nil {
		t.Fatal(err)
	}
	userContent := []byte("# My own notes\nnot generated\n")
	if err := os.WriteFile(userNote, userContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// Retire one page so the next generation drops its file.
	if _, err := st.DB().ExecContext(ctx, `update pages set alive = 0 where id = 'page3'`); err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.Export(ctx); err != nil {
		t.Fatal(err)
	}
	var dropped string
	for _, path := range first.Files {
		if strings.HasPrefix(filepath.Base(path), "hiring-") {
			dropped = path
		}
	}
	if dropped == "" {
		t.Fatal("test fixture did not produce the hiring page")
	}
	if _, err := os.Stat(dropped); !os.IsNotExist(err) {
		t.Fatal("dropped generated page was not pruned")
	}
	got, err := os.ReadFile(userNote)
	if err != nil || string(got) != string(userContent) {
		t.Fatalf("user Markdown was modified or removed: %v", err)
	}
}
