package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/openclaw/notcrawl/internal/config"
	"github.com/openclaw/notcrawl/internal/markdown"
	"github.com/openclaw/notcrawl/internal/store"
	"github.com/openclaw/notcrawl/internal/tableexport"
)

func runExportMarkdown(ctx context.Context, stdout io.Writer, cfg config.Config) error {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := markdown.Exporter{Store: st, Dir: cfg.MarkdownDir, CacheDir: cfg.CacheDir}.Export(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "exported %d pages to %s", s.Pages, cfg.MarkdownDir)
	if s.IncompletePages > 0 {
		fmt.Fprintf(stdout, " (%d incomplete; %d missing block references)", s.IncompletePages, s.MissingBlockReferences)
	}
	fmt.Fprintln(stdout)
	return nil
}

func runDatabases(ctx context.Context, stdout io.Writer, cfg config.Config) error {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	collections, err := st.Collections(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "id\tname\tsource")
	for _, collection := range collections {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", collection.ID, collection.Name, collection.Source)
	}
	return nil
}

func runExportDatabase(ctx context.Context, stdout io.Writer, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("export-db", flag.ContinueOnError)
	databaseID := fs.String("database", "", "database id to export")
	all := fs.Bool("all", false, "export every crawled database")
	dir := fs.String("dir", "", "directory for --all exports")
	format := fs.String("format", "csv", "output format: csv or tsv")
	output := fs.String("output", "", "output file path, defaults to stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all {
		if *databaseID != "" {
			return fmt.Errorf("export-db cannot combine --all and --database")
		}
		if *output != "" {
			return fmt.Errorf("export-db cannot combine --all and --output")
		}
		if *dir == "" {
			return fmt.Errorf("export-db --all requires --dir")
		}
		return runExportAllDatabases(ctx, stdout, cfg, tableexport.Format(*format), *dir)
	}
	if *databaseID == "" {
		return fmt.Errorf("export-db requires --database")
	}
	formatValue := tableexport.Format(*format)
	if err := tableexport.ValidateFormat(formatValue); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	var out io.Writer = stdout
	var file io.WriteCloser
	outputName := ""
	tmpPath := ""
	committed := false
	if *output != "" {
		outputPath, err := config.ExpandPath(*output)
		if err != nil {
			return err
		}
		// Render into a sibling temporary file first so a render or close
		// failure cannot truncate an existing output; swap it in only after
		// the whole export succeeds.
		tmp, err := os.CreateTemp(filepath.Dir(outputPath), "."+filepath.Base(outputPath)+".tmp-*")
		if err != nil {
			return err
		}
		tmpPath = tmp.Name()
		defer func() {
			if !committed {
				_ = os.Remove(tmpPath)
			}
		}()
		file = tmp
		out = tmp
		outputName = outputPath
	}
	s, exportErr := tableexport.Exporter{Store: st}.Export(ctx, *databaseID, formatValue, out)
	if file != nil {
		closeErr := file.Close()
		if exportErr != nil {
			return exportErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := os.Rename(tmpPath, outputName); err != nil {
			return err
		}
		committed = true
		fmt.Fprintf(stdout, "exported %d rows and %d columns from %s to %s\n", s.Rows, s.Columns, s.Database, outputName)
		return nil
	}
	return exportErr
}

func runExportAllDatabases(ctx context.Context, stdout io.Writer, cfg config.Config, format tableexport.Format, dir string) error {
	if err := tableexport.ValidateFormat(format); err != nil {
		return err
	}
	dir, err := config.ExpandPath(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := tableexport.AllExporter{
		Store:    st,
		Dir:      dir,
		Format:   format,
		CacheDir: cfg.CacheDir,
	}.Export(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "exported %d databases and %d rows to %s\n", s.Databases, s.Rows, dir)
	return nil
}
