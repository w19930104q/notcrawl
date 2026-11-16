package tableexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/notcrawl/internal/exportgen"
	"github.com/openclaw/notcrawl/internal/store"
)

func readDatabaseManifest(t *testing.T, dir string) exportgen.Manifest {
	t.Helper()
	path := filepath.Join(dir, exportgen.StateDirName, exportgen.KindDatabase, "manifest.json")
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

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func seedAllExportStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "notcrawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := store.NowMS()
	for _, collection := range []store.Collection{
		{ID: "db1", Name: "Roadmap", Source: "test", SyncedAt: now, SchemaJSON: `{"Name":{"type":"title"},"status_id":{"name":"Status","type":"select"}}`},
		{ID: "db2", Name: "Contacts", Source: "test", SyncedAt: now, SchemaJSON: `{"Name":{"type":"title"},"email_id":{"name":"Email","type":"email"}}`},
	} {
		if err := st.UpsertCollection(ctx, collection); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertPage(ctx, store.Page{
		ID: "page1", CollectionID: "db1", Title: "Ship", URL: "https://example.com/ship", Alive: true, Source: "test", SyncedAt: now,
		PropertiesJSON: `{"Name":{"type":"title","title":[{"plain_text":"Ship"}]},"status_id":{"type":"select","select":{"name":"Done"}}}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPage(ctx, store.Page{
		ID: "page2", CollectionID: "db2", Title: "Ada", URL: "https://example.com/ada", Alive: true, Source: "test", SyncedAt: now,
		PropertiesJSON: `{"Name":{"type":"title","title":[{"plain_text":"Ada"}]},"email_id":{"type":"email","email":"ada@example.com"}}`,
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAllExporterPublishesStagedGeneration(t *testing.T) {
	ctx := context.Background()
	st := seedAllExportStore(t)
	defer st.Close()
	dir := t.TempDir()
	exporter := AllExporter{Store: st, Dir: dir, Format: FormatCSV, CacheDir: filepath.Join(t.TempDir(), "cache")}

	summary, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Databases != 2 || summary.Rows != 2 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	for _, name := range []string{"roadmap-db1.csv", "contacts-db2.csv", indexFileName} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	manifest := readDatabaseManifest(t, dir)
	if len(manifest.Files) != 3 {
		t.Fatalf("manifest files = %d, want 3", len(manifest.Files))
	}
	for _, file := range manifest.Files {
		hash := sha256OfFile(t, filepath.Join(dir, filepath.FromSlash(file.Path)))
		if hash != file.SHA256 {
			t.Fatalf("file %s hash does not match manifest", file.Path)
		}
		if file.SourceDigest == "" {
			t.Fatalf("file %s missing source digest", file.Path)
		}
	}
}

func TestAllExporterRerunReusesUnchangedFiles(t *testing.T) {
	ctx := context.Background()
	st := seedAllExportStore(t)
	defer st.Close()
	dir := t.TempDir()
	exporter := AllExporter{Store: st, Dir: dir, Format: FormatTSV, CacheDir: filepath.Join(t.TempDir(), "cache")}
	first, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reused != 0 {
		t.Fatalf("first run reused files: %+v", first)
	}
	second, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation == first.Generation {
		t.Fatal("rerun reused the generation id")
	}
	if second.Reused != 3 {
		t.Fatalf("unchanged rerun reused %d files, want 3", second.Reused)
	}
}

func TestAllExporterFailureKeepsPreviousFilesAndCleansOrphans(t *testing.T) {
	ctx := context.Background()
	st := seedAllExportStore(t)
	defer st.Close()
	dir := t.TempDir()
	exporter := AllExporter{Store: st, Dir: dir, Format: FormatCSV, CacheDir: filepath.Join(t.TempDir(), "cache")}
	first, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	previous := map[string]string{}
	for _, path := range first.Files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		previous[path] = string(data)
	}

	injected := errors.New("index render boom")
	exporter.Hooks = exportgen.FaultHooks{
		AfterItem: func(item exportgen.Item, attempt int) error {
			if item.ID == "index" {
				return injected
			}
			return nil
		},
	}
	if _, err := exporter.Export(ctx); !errors.Is(err, injected) {
		t.Fatalf("want injected failure, got %v", err)
	}
	for path, want := range previous {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("previous export changed or vanished after failure: %v", err)
		}
	}
	persisted := readDatabaseManifest(t, dir)
	if persisted.Generation != first.Generation {
		t.Fatal("manifest switched despite failed generation")
	}

	// Simulate a hard-crash orphan that no cleanup defer ever removed.
	kindDir := filepath.Join(dir, exportgen.StateDirName, exportgen.KindDatabase)
	orphan := filepath.Join(kindDir, "tmp-crashed")
	if err := os.MkdirAll(filepath.Join(orphan, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "nested", "ghost.csv"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Successful rerun publishes again and sweeps every staging directory.
	exporter.Hooks = exportgen.FaultHooks{}
	second, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Databases != 2 {
		t.Fatalf("rerun summary: %+v", second)
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
			t.Fatalf("orphan staging survived: %s", entry.Name())
		}
	}
}

func TestAllExporterRerendersChangedCollection(t *testing.T) {
	ctx := context.Background()
	st := seedAllExportStore(t)
	defer st.Close()
	dir := t.TempDir()
	exporter := AllExporter{Store: st, Dir: dir, Format: FormatCSV, CacheDir: filepath.Join(t.TempDir(), "cache")}
	if _, err := exporter.Export(ctx); err != nil {
		t.Fatal(err)
	}
	// Change one row in db1 only.
	if err := st.UpsertPage(ctx, store.Page{
		ID: "page1", CollectionID: "db1", Title: "Ship", URL: "https://example.com/ship", Alive: true, Source: "test", SyncedAt: store.NowMS() + 1,
		PropertiesJSON: `{"Name":{"type":"title","title":[{"plain_text":"Ship v2"}]},"status_id":{"type":"select","select":{"name":"Shipped"}}}`,
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := exporter.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// db1 re-renders; db2 and the index are unchanged and reused.
	if summary.Reused != 2 {
		t.Fatalf("expected 2 reused files, got %d (%+v)", summary.Reused, summary.Files)
	}
	data, err := os.ReadFile(filepath.Join(dir, "roadmap-db1.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Ship v2") {
		t.Fatalf("changed collection was not re-rendered:\n%s", data)
	}
}
