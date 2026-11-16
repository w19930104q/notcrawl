// Package exportgen runs staged, manifest-verified export generations.
//
// An export generation renders every output from a single read-only SQLite
// snapshot into an isolated staging directory, records per-file source and
// result fingerprints, and only becomes visible after the whole generation
// succeeds. A failed generation never modifies the previously published
// output or unrelated user files; rerunning it reuses files whose rendered
// inputs are unchanged and reclaims orphans left by the failed attempt.
package exportgen

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openclaw/notcrawl/internal/store"
	sqlite "modernc.org/sqlite"
)

// StateDirName is the hidden directory under an export root that holds
// manifests, generation trees, and staging directories.
const StateDirName = ".notcrawl-export"

// KindMarkdown and KindDatabase select the manifest namespace for an export.
const (
	KindMarkdown = "markdown"
	KindDatabase = "database"
)

// Item describes one output file of a generation.
type Item struct {
	// ID is the stable identity of the rendered record (page id, database id,
	// or "index" for the export index).
	ID string
	// RelativePath is the output path relative to the export root.
	RelativePath string
	// SourceDigest fingerprints every source input that influences the
	// rendered bytes. Identical digest and path means a previous result can be
	// reused without rendering.
	SourceDigest string
}

// FileResult is what Render returns for one rendered item.
type FileResult struct {
	// Data holds the rendered file bytes. Ignored when Reused is true.
	Data []byte
	// Mode is the file mode; zero defaults to 0o644.
	Mode os.FileMode
	// Reused reports the item was rendered from a previous generation and the
	// bytes were already linked into the staging directory by Render.
	Reused bool
	// Metadata carries verifiable per-item details into the manifest (for
	// example incomplete-content counters). It is not part of the output
	// files.
	Metadata map[string]int
}

// Plan is the callback that enumerates output items from the frozen snapshot.
type Plan func(ctx context.Context, snapshot *store.Store) ([]Item, error)

// Render renders a single item into the staging directory. It must create the
// file at stagingPath (including parent directories) unless it returns a
// FileResult with Reused set, in which case the reused bytes must already
// exist at stagingPath.
type Render func(ctx context.Context, snapshot *store.Store, item Item, stagingPath string) (FileResult, error)

// FaultHooks are optional test hooks fired at generation lifecycle points.
// Returning an error simulates a failure at that point.
type FaultHooks struct {
	AfterSnapshot  func(snapshotPath string) error
	AfterPlan      func(items []Item) error
	AfterItem      func(item Item, attempt int) error
	AfterStaging   func(stagingDir string) error
	AfterPublished func(manifestPath string) error
}

// Options configures a generation run.
type Options struct {
	// Kind namespaces the manifest and generation state (KindMarkdown, ...).
	Kind string
	// DBPath is the live SQLite archive to snapshot.
	DBPath string
	// CacheDir receives the temporary SQLite snapshot.
	CacheDir string
	// OutputRoot is the published export directory (Markdown root, --all dir).
	OutputRoot string
	Plan       Plan
	Render     Render
	// Owned reports whether a previously managed file may be pruned when the
	// current generation no longer lists it. User files for which Owned is
	// false are never removed.
	Owned func(absolutePath string) bool
	// Hooks inject failures in tests.
	Hooks FaultHooks
	// Clock and IDReader are overridable in tests.
	Clock    func() time.Time
	IDReader io.Reader
}

// FileManifest records one published output and the source inputs that
// produced it.
type FileManifest struct {
	ID           string         `json:"id"`
	Path         string         `json:"path"`
	SourceDigest string         `json:"source_digest"`
	SHA256       string         `json:"sha256"`
	Size         int64          `json:"size"`
	Reused       bool           `json:"reused,omitempty"`
	Metadata     map[string]int `json:"metadata,omitempty"`
}

// SnapshotManifest fingerprints the SQLite snapshot backing a generation.
type SnapshotManifest struct {
	SourcePath string `json:"source_path"`
	Path       string `json:"snapshot_path"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
}

// Manifest is the verifiable record of a published generation.
type Manifest struct {
	Version    int              `json:"version"`
	Kind       string           `json:"kind"`
	Generation string           `json:"generation"`
	CreatedAt  string           `json:"created_at"`
	Snapshot   SnapshotManifest `json:"snapshot"`
	Files      []FileManifest   `json:"files"`
}

const manifestVersion = 1

type statePaths struct {
	root       string // OutputRoot
	state      string // OutputRoot/.notcrawl-export
	kind       string // .../<kind>
	manifest   string // .../<kind>/manifest.json
	generation string // .../<kind>/generations/<id>
	staging    string // .../<kind>/tmp-<id>
}

// Run executes one full generation and returns its manifest.
func Run(ctx context.Context, opts Options) (Manifest, error) {
	if err := validateOptions(opts); err != nil {
		return Manifest{}, err
	}
	paths, err := prepareState(opts)
	if err != nil {
		return Manifest{}, err
	}
	// Staging directories of earlier failed attempts are orphans; remove them
	// before starting so their files cannot be mistaken for live output.
	if err := removeStaleStaging(paths.kind); err != nil {
		return Manifest{}, err
	}

	snapshotPath, snapshotInfo, err := captureSnapshot(opts)
	if err != nil {
		return Manifest{}, err
	}
	snapshotCleaned := false
	cleanupSnapshot := func() {
		if snapshotCleaned {
			return
		}
		snapshotCleaned = true
		removeSQLiteBundle(snapshotPath)
	}
	defer cleanupSnapshot()
	if opts.Hooks.AfterSnapshot != nil {
		if err := opts.Hooks.AfterSnapshot(snapshotPath); err != nil {
			return Manifest{}, err
		}
	}

	snapshot, err := store.OpenReadOnly(snapshotPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("open export snapshot: %w", err)
	}

	items, err := opts.Plan(ctx, snapshot)
	if err != nil {
		_ = snapshot.Close()
		return Manifest{}, err
	}
	if opts.Hooks.AfterPlan != nil {
		if err := opts.Hooks.AfterPlan(items); err != nil {
			_ = snapshot.Close()
			return Manifest{}, err
		}
	}

	previous, previousErr := readManifest(paths.manifest)
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		_ = snapshot.Close()
		return Manifest{}, previousErr
	}
	hasPrevious := previousErr == nil
	previousFiles := map[string]FileManifest{}
	if hasPrevious {
		for _, file := range previous.Files {
			previousFiles[file.Path] = file
		}
	}

	generationID, err := newGenerationID(opts)
	if err != nil {
		_ = snapshot.Close()
		return Manifest{}, err
	}
	paths.staging = filepath.Join(paths.kind, "tmp-"+generationID)
	paths.generation = filepath.Join(paths.kind, "generations", generationID)
	if err := os.MkdirAll(paths.staging, 0o700); err != nil {
		_ = snapshot.Close()
		return Manifest{}, fmt.Errorf("create export staging: %w", err)
	}
	stagingCommitted := false
	defer func() {
		if !stagingCommitted {
			_ = os.RemoveAll(paths.staging)
		}
	}()

	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	manifest := Manifest{
		Version:    manifestVersion,
		Kind:       opts.Kind,
		Generation: generationID,
		CreatedAt:  clock().UTC().Format(time.RFC3339Nano),
		Snapshot:   snapshotInfo,
	}

	seen := map[string]bool{}
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			_ = snapshot.Close()
			return Manifest{}, err
		}
		rel := filepath.Clean(item.RelativePath)
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			_ = snapshot.Close()
			return Manifest{}, fmt.Errorf("export item path escapes output root: %q", item.RelativePath)
		}
		if seen[rel] {
			_ = snapshot.Close()
			return Manifest{}, fmt.Errorf("duplicate export item path: %q", rel)
		}
		seen[rel] = true

		stagingPath := filepath.Join(paths.staging, rel)
		result, reused, err := renderItem(ctx, opts, snapshot, item, rel, stagingPath, previousFiles, hasPrevious)
		if err != nil {
			_ = snapshot.Close()
			return Manifest{}, fmt.Errorf("render %s: %w", item.ID, err)
		}
		if opts.Hooks.AfterItem != nil {
			if err := opts.Hooks.AfterItem(item, index); err != nil {
				_ = snapshot.Close()
				return Manifest{}, err
			}
		}
		hash, size, err := fingerprintFile(stagingPath)
		if err != nil {
			_ = snapshot.Close()
			return Manifest{}, err
		}
		manifest.Files = append(manifest.Files, FileManifest{
			ID:           item.ID,
			Path:         filepath.ToSlash(rel),
			SourceDigest: item.SourceDigest,
			SHA256:       hash,
			Size:         size,
			Reused:       reused,
			Metadata:     result.Metadata,
		})
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	if err := snapshot.Close(); err != nil {
		return Manifest{}, err
	}

	if opts.Hooks.AfterStaging != nil {
		if err := opts.Hooks.AfterStaging(paths.staging); err != nil {
			return Manifest{}, err
		}
	}

	if err := publish(opts, paths, manifest, hasPrevious, previousFiles); err != nil {
		return Manifest{}, err
	}
	stagingCommitted = true
	if opts.Hooks.AfterPublished != nil {
		if err := opts.Hooks.AfterPublished(paths.manifest); err != nil {
			return Manifest{}, err
		}
	}
	if err := os.RemoveAll(paths.staging); err != nil {
		return Manifest{}, fmt.Errorf("clean export staging: %w", err)
	}
	if err := pruneGenerations(paths, manifest.Generation); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func renderItem(ctx context.Context, opts Options, snapshot *store.Store, item Item, rel, stagingPath string, previous map[string]FileManifest, hasPrevious bool) (FileResult, bool, error) {
	if hasPrevious {
		if old, ok := previous[filepath.ToSlash(rel)]; ok && old.SourceDigest == item.SourceDigest && old.SHA256 != "" {
			previousPath := filepath.Join(opts.OutputRoot, rel)
			if hash, _, err := fingerprintFile(previousPath); err == nil && hash == old.SHA256 {
				if err := linkOrCopyFile(previousPath, stagingPath, 0o644); err == nil {
					return FileResult{Reused: true, Metadata: old.Metadata}, true, nil
				}
			}
		}
	}
	result, err := opts.Render(ctx, snapshot, item, stagingPath)
	if err != nil {
		return FileResult{}, false, err
	}
	if result.Reused {
		if _, _, err := fingerprintFile(stagingPath); err != nil {
			return FileResult{}, false, fmt.Errorf("renderer claimed reuse without staging file: %w", err)
		}
		return result, false, nil
	}
	if len(result.Data) == 0 {
		return FileResult{}, false, fmt.Errorf("render produced no bytes")
	}
	mode := result.Mode
	if mode == 0 {
		mode = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(stagingPath), 0o755); err != nil {
		return FileResult{}, false, err
	}
	if err := os.WriteFile(stagingPath, result.Data, mode); err != nil {
		return FileResult{}, false, err
	}
	return result, false, nil
}

func validateOptions(opts Options) error {
	if opts.Kind == "" {
		return errors.New("export generation kind is required")
	}
	if opts.DBPath == "" {
		return errors.New("export database path is required")
	}
	if opts.CacheDir == "" {
		return errors.New("export cache dir is required")
	}
	if opts.OutputRoot == "" {
		return errors.New("export output root is required")
	}
	if opts.Plan == nil || opts.Render == nil {
		return errors.New("export plan and render are required")
	}
	return nil
}

func prepareState(opts Options) (statePaths, error) {
	root, err := filepath.Abs(opts.OutputRoot)
	if err != nil {
		return statePaths{}, err
	}
	state := filepath.Join(root, StateDirName)
	kind := filepath.Join(state, opts.Kind)
	if err := os.MkdirAll(filepath.Join(kind, "generations"), 0o700); err != nil {
		return statePaths{}, fmt.Errorf("create export state: %w", err)
	}
	return statePaths{
		root:     root,
		state:    state,
		kind:     kind,
		manifest: filepath.Join(kind, "manifest.json"),
	}, nil
}

func newGenerationID(opts Options) (string, error) {
	reader := opts.IDReader
	if reader == nil {
		reader = rand.Reader
	}
	var raw [8]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return "", err
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return clock().UTC().Format("20060102T150405.000000000") + "-" + hex.EncodeToString(raw[:6]), nil
}

// captureSnapshot creates a consistent, self-contained copy of the live
// archive in a uniquely named temporary SQLite file under the cache
// directory. The SQLite online backup API folds committed WAL frames into
// the copy even while the live archive has an open connection, so the export
// always renders one frozen point in time; the live database is opened
// read-only for the backup.
func captureSnapshot(opts Options) (string, SnapshotManifest, error) {
	snapshotDir := filepath.Join(opts.CacheDir, "export-snapshots")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return "", SnapshotManifest{}, fmt.Errorf("create snapshot dir: %w", err)
	}
	name := fmt.Sprintf("export-%s-%d.db", opts.Kind, time.Now().UnixNano())
	snapshotPath := filepath.Join(snapshotDir, name)
	if err := backupSQLite(opts.DBPath, snapshotPath); err != nil {
		removeSQLiteBundle(snapshotPath)
		return "", SnapshotManifest{}, fmt.Errorf("snapshot export database: %w", err)
	}
	// Verify the copy opens and fold any residual WAL state into the main file.
	checkpointDB, err := sql.Open("sqlite", snapshotPath)
	if err != nil {
		removeSQLiteBundle(snapshotPath)
		return "", SnapshotManifest{}, fmt.Errorf("verify export snapshot: %w", err)
	}
	if _, err := checkpointDB.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = checkpointDB.Close()
		removeSQLiteBundle(snapshotPath)
		return "", SnapshotManifest{}, fmt.Errorf("checkpoint export snapshot: %w", err)
	}
	if err := checkpointDB.Close(); err != nil {
		removeSQLiteBundle(snapshotPath)
		return "", SnapshotManifest{}, err
	}
	if err := os.Remove(snapshotPath + "-wal"); err != nil && !errors.Is(err, os.ErrNotExist) {
		removeSQLiteBundle(snapshotPath)
		return "", SnapshotManifest{}, err
	}
	if err := os.Remove(snapshotPath + "-shm"); err != nil && !errors.Is(err, os.ErrNotExist) {
		removeSQLiteBundle(snapshotPath)
		return "", SnapshotManifest{}, err
	}
	hash, size, err := fingerprintFile(snapshotPath)
	if err != nil {
		removeSQLiteBundle(snapshotPath)
		return "", SnapshotManifest{}, err
	}
	return snapshotPath, SnapshotManifest{
		SourcePath: opts.DBPath,
		Path:       snapshotPath,
		SHA256:     hash,
		Size:       size,
	}, nil
}

// backupSQLite performs an online SQLite backup from source into target. The
// source is opened immutable/read-only so the live archive cannot be
// modified; backup still observes all committed WAL frames.
func backupSQLite(source, target string) error {
	sourceURI := "file:" + sqliteURIPath(source) + "?mode=ro&_pragma=busy_timeout(5000)"
	sourceDB, err := sql.Open("sqlite", sourceURI)
	if err != nil {
		return err
	}
	defer sourceDB.Close()
	conn, err := sourceDB.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	targetURI := "file:" + sqliteURIPath(target) + "?_pragma=journal_mode(DELETE)"
	return conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(interface {
			NewBackup(dstURI string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("sqlite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(targetURI)
		if err != nil {
			return err
		}
		for {
			more, err := backup.Step(-1)
			if err != nil {
				_ = backup.Finish()
				return err
			}
			if !more {
				return backup.Finish()
			}
		}
	})
}

// sqliteURIPath percent-encodes a filesystem path for use in a SQLite file:
// URI so that spaces, question marks, and non-ASCII names cannot break URI
// parameter parsing.
func sqliteURIPath(path string) string {
	var b strings.Builder
	for _, r := range path {
		switch r {
		case '?', '#', '%':
			b.WriteString(url.QueryEscape(string(r)))
		default:
			if r < 0x20 {
				b.WriteString(url.QueryEscape(string(r)))
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func removeSQLiteBundle(path string) {
	for _, target := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
}

// publish links every staged file into the output tree (replacing prior
// generated files one at a time), prunes managed files that the new
// generation drops, persists the generation tree, and only then switches the
// manifest atomically.
func publish(opts Options, paths statePaths, manifest Manifest, hasPrevious bool, previous map[string]FileManifest) error {
	keep := map[string]bool{}
	for _, file := range manifest.Files {
		keep[filepath.Clean(filepath.Join(paths.root, filepath.FromSlash(file.Path)))] = true
	}
	if err := os.MkdirAll(paths.generation, 0o700); err != nil {
		return err
	}
	for _, file := range manifest.Files {
		rel := filepath.FromSlash(file.Path)
		stagingPath := filepath.Join(paths.staging, rel)
		targetPath := filepath.Join(paths.root, rel)
		generationPath := filepath.Join(paths.generation, rel)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(generationPath), 0o700); err != nil {
			return err
		}
		// The generation tree keeps an independent copy of exactly what this
		// generation published, so a later edit of the visible file cannot
		// corrupt the reuse source.
		if err := linkOrCopyFile(stagingPath, generationPath, 0o600); err != nil {
			return err
		}
		if err := replaceFile(stagingPath, targetPath, 0o644); err != nil {
			return err
		}
	}
	if err := pruneStale(opts, paths.root, keep, hasPrevious, previous); err != nil {
		return err
	}
	if err := writeManifestAtomic(paths.manifest, manifest); err != nil {
		return err
	}
	return nil
}

// pruneStale removes managed files that the previous manifest recorded but
// the current generation drops. On the first generation no manifest exists,
// so the Owned gate alone decides which legacy generated files to reclaim;
// unmanaged user files are never touched.
func pruneStale(opts Options, root string, keep map[string]bool, hasPrevious bool, previous map[string]FileManifest) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		path = filepath.Clean(path)
		if d.IsDir() {
			if path != filepath.Clean(root) && !isInside(path, filepath.Join(root, StateDirName)) {
				dirs = append(dirs, path)
			}
			return nil
		}
		if keep[path] || isInside(path, filepath.Join(root, StateDirName)) {
			return nil
		}
		if hasPrevious {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if _, managed := previous[filepath.ToSlash(rel)]; !managed {
				return nil
			}
		}
		if opts.Owned != nil && !opts.Owned(path) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}

func pruneGenerations(paths statePaths, current string) error {
	root := filepath.Join(paths.kind, "generations")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == current {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func removeStaleStaging(kindDir string) error {
	entries, err := os.ReadDir(kindDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "tmp-") {
			if err := os.RemoveAll(filepath.Join(kindDir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeManifestAtomic(path string, manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func readManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("read export manifest %s: %w", path, err)
	}
	if manifest.Version != manifestVersion {
		return Manifest{}, fmt.Errorf("unsupported export manifest version %d", manifest.Version)
	}
	return manifest, nil
}

func fingerprintFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// replaceFile atomically swaps source into target within the same directory
// tree. Hard links keep the operation cheap and copy-on-write safe; a cross
// device fallback copies bytes.
func replaceFile(source, target string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.Link(source, target); err == nil {
		if err := os.Chmod(target, mode); err != nil {
			return err
		}
		return nil
	}
	// An existing target cannot be hard-linked over; move it aside first.
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return copyFile(source, target, mode)
	}
	if err := os.Link(source, target); err == nil {
		return os.Chmod(target, mode)
	}
	return copyFile(source, target, mode)
}

func linkOrCopyFile(source, target string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.Link(source, target); err == nil {
		return os.Chmod(target, mode)
	}
	return copyFile(source, target, mode)
}

func copyFile(source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return err
	}
	committed = true
	return nil
}

func isInside(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// DigestHash returns the hex SHA256 of the concatenated labeled inputs. It is
// a convenience for renderers building per-item source digests.
func DigestHash(label string, inputs ...string) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%d\x00", label, len(inputs))
	for _, input := range inputs {
		hash.Write([]byte(input))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
