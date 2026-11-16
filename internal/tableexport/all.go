package tableexport

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openclaw/notcrawl/internal/exportgen"
	"github.com/openclaw/notcrawl/internal/notiontext"
	"github.com/openclaw/notcrawl/internal/store"
)

// AllSummary reports the outcome of an --all generation.
type AllSummary struct {
	Databases  int
	Rows       int
	Files      []string
	Reused     int
	Generation string
}

// AllExporter runs the export-db --all output through staged generations.
type AllExporter struct {
	Store    *store.Store
	Dir      string
	Format   Format
	CacheDir string
	Hooks    exportgen.FaultHooks
}

const indexFileName = "index.tsv"

// Export renders every collection plus the index file as one verifiable
// generation: all files land in staging first, and the manifest switches only
// after every database renders successfully.
func (e AllExporter) Export(ctx context.Context) (AllSummary, error) {
	if e.Store == nil {
		return AllSummary{}, fmt.Errorf("missing store")
	}
	if e.Dir == "" {
		return AllSummary{}, fmt.Errorf("missing export dir")
	}
	if err := os.MkdirAll(e.Dir, 0o755); err != nil {
		return AllSummary{}, err
	}
	cacheDir := e.CacheDir
	if cacheDir == "" {
		cacheDir = filepath.Join(e.Dir, exportgen.StateDirName, "cache")
	}
	manifest, err := exportgen.Run(ctx, exportgen.Options{
		Kind:       exportgen.KindDatabase,
		DBPath:     e.Store.Path(),
		CacheDir:   cacheDir,
		OutputRoot: e.Dir,
		Hooks:      e.Hooks,
		Plan:       e.plan,
		Render:     e.render,
		// Every file under the root is generated table output; the state
		// directory itself is excluded by the walk.
		Owned: func(string) bool { return true },
	})
	if err != nil {
		return AllSummary{}, err
	}
	summary := AllSummary{Generation: manifest.Generation}
	for _, file := range manifest.Files {
		summary.Files = append(summary.Files, filepath.Join(e.Dir, filepath.FromSlash(file.Path)))
		if file.Reused {
			summary.Reused++
		}
		if file.Path != indexFileName {
			summary.Databases++
			summary.Rows += file.Metadata[metadataRows]
		}
	}
	return summary, nil
}

type databasePlan struct {
	collection store.Collection
	filename   string
	rows       int
	columns    int
}

func (e AllExporter) plan(ctx context.Context, snapshot *store.Store) ([]exportgen.Item, error) {
	if err := ValidateFormat(e.Format); err != nil {
		return nil, err
	}
	collections, err := snapshot.Collections(ctx)
	if err != nil {
		return nil, err
	}
	ext, err := extension(e.Format)
	if err != nil {
		return nil, err
	}
	items := make([]exportgen.Item, 0, len(collections)+1)
	used := map[string]bool{}
	var indexRows [][]string
	for _, collection := range collections {
		pages, err := snapshot.CollectionPages(ctx, collection.ID)
		if err != nil {
			return nil, err
		}
		refs, err := referenceLabelsFor(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		columns := propertyColumns(collection, pages)
		name := databaseFilename(collection, ext, used)
		digest := collectionDigest(collection, pages, refs, columns, e.Format)
		items = append(items, exportgen.Item{
			ID:           collection.ID,
			RelativePath: name,
			SourceDigest: digest,
		})
		indexRows = append(indexRows, []string{
			collection.ID, collection.Name, collection.Source,
			fmt.Sprint(len(pages)), fmt.Sprint(len(columns) + len(fixedHeaders())), name,
		})
	}
	items = append(items, exportgen.Item{
		ID:           "index",
		RelativePath: indexFileName,
		SourceDigest: exportgen.DigestHash("database-index", encodeIndexRows(indexRows), string(e.Format)),
	})
	return items, nil
}

const (
	metadataRows    = "rows"
	metadataColumns = "columns"
)

func (e AllExporter) render(ctx context.Context, snapshot *store.Store, item exportgen.Item, stagingPath string) (exportgen.FileResult, error) {
	if item.ID == "index" {
		collections, err := snapshot.Collections(ctx)
		if err != nil {
			return exportgen.FileResult{}, err
		}
		ext, err := extension(e.Format)
		if err != nil {
			return exportgen.FileResult{}, err
		}
		used := map[string]bool{}
		var rows [][]string
		for _, collection := range collections {
			pages, err := snapshot.CollectionPages(ctx, collection.ID)
			if err != nil {
				return exportgen.FileResult{}, err
			}
			columns := propertyColumns(collection, pages)
			name := databaseFilename(collection, ext, used)
			rows = append(rows, []string{
				collection.ID, collection.Name, collection.Source,
				fmt.Sprint(len(pages)), fmt.Sprint(len(columns) + len(fixedHeaders())), name,
			})
		}
		data, err := encodeIndex(rows)
		if err != nil {
			return exportgen.FileResult{}, err
		}
		return exportgen.FileResult{Data: data}, nil
	}

	var buffer bytes.Buffer
	summary, err := Exporter{Store: snapshot}.Export(ctx, item.ID, e.Format, &buffer)
	if err != nil {
		return exportgen.FileResult{}, err
	}
	return exportgen.FileResult{
		Data: buffer.Bytes(),
		Metadata: map[string]int{
			metadataRows:    summary.Rows,
			metadataColumns: summary.Columns,
		},
	}, nil
}

func fixedHeaders() []string {
	return []string{"page_id", "page_title", "url"}
}

func extension(format Format) (string, error) {
	switch format {
	case "", FormatCSV:
		return "csv", nil
	case FormatTSV:
		return "tsv", nil
	default:
		return "", fmt.Errorf("unsupported format %q", format)
	}
}

func databaseFilename(collection store.Collection, ext string, used map[string]bool) string {
	baseName := collection.Name
	if strings.TrimSpace(baseName) == "" {
		baseName = collection.ID
	}
	base := notiontext.Slug(baseName) + "-" + notiontext.ShortID(collection.ID)
	name := base + "." + ext
	for i := 2; used[name]; i++ {
		name = fmt.Sprintf("%s-%d.%s", base, i, ext)
	}
	used[name] = true
	return name
}

func encodeIndex(rows [][]string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	writer.Comma = '\t'
	if err := writer.Write([]string{"id", "name", "source", "rows", "columns", "file"}); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := writer.Write(row); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func encodeIndexRows(rows [][]string) string {
	var b strings.Builder
	for _, row := range rows {
		b.WriteString(strings.Join(row, "\x1f"))
		b.WriteByte(0)
	}
	return b.String()
}

// collectionDigest fingerprints every source input that changes the rendered
// delimited output: collection schema, every row's properties, and the user
// and page reference labels used for relation rendering.
func collectionDigest(collection store.Collection, pages []store.Page, refs ReferenceLabels, columns []exportColumn, format Format) string {
	encode := func(v any) string {
		var b strings.Builder
		if err := json.NewEncoder(&b).Encode(v); err != nil {
			return fmt.Sprintf("%v", v)
		}
		return b.String()
	}
	parts := []string{
		"v1",
		string(format),
		encode(collection),
		encode(pages),
		encode(refs),
		encode(columns),
	}
	return exportgen.DigestHash("database-collection", parts...)
}
