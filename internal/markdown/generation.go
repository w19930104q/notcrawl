package markdown

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openclaw/notcrawl/internal/exportgen"
	"github.com/openclaw/notcrawl/internal/notiontext"
	"github.com/openclaw/notcrawl/internal/store"
)

type Exporter struct {
	Store *store.Store
	Dir   string
	// CacheDir stores the temporary SQLite snapshot backing an export. When
	// empty, the snapshot is written under the export state directory.
	CacheDir string
	// Hooks inject generation failures in tests.
	Hooks exportgen.FaultHooks
}

type Summary struct {
	Pages                  int
	IncompletePages        int
	MissingBlockReferences int
	Files                  []string
	// Reused counts pages whose bytes were carried over from the previous
	// generation without re-rendering.
	Reused int
	// Generation identifies the published export generation.
	Generation string
}

type pageData struct {
	page      store.Page
	blocks    []store.Block
	comments  []store.Comment
	coverage  store.BlockCoverage
	spaceName string
	teamID    string
	teamName  string
	path      string
	apiSynced bool
	mcpSynced bool
}

func (e Exporter) Export(ctx context.Context) (Summary, error) {
	if e.Store == nil {
		return Summary{}, fmt.Errorf("missing store")
	}
	if e.Dir == "" {
		return Summary{}, fmt.Errorf("missing markdown dir")
	}
	if err := os.MkdirAll(e.Dir, 0o755); err != nil {
		return Summary{}, err
	}
	cacheDir := e.CacheDir
	if cacheDir == "" {
		cacheDir = filepath.Join(e.Dir, exportgen.StateDirName, "cache")
	}
	manifest, err := exportgen.Run(ctx, exportgen.Options{
		Kind:       exportgen.KindMarkdown,
		DBPath:     e.Store.Path(),
		CacheDir:   cacheDir,
		OutputRoot: e.Dir,
		Hooks:      e.Hooks,
		Plan:       e.planPages,
		Render:     e.renderPageItem,
		Owned:      isNotcrawlGeneratedMarkdown,
	})
	if err != nil {
		return Summary{}, err
	}
	return e.summarize(manifest), nil
}

func (e Exporter) summarize(manifest exportgen.Manifest) Summary {
	var s Summary
	s.Generation = manifest.Generation
	s.Pages = len(manifest.Files)
	for _, file := range manifest.Files {
		s.Files = append(s.Files, filepath.Join(e.Dir, filepath.FromSlash(file.Path)))
		if file.Reused {
			s.Reused++
		}
	}
	s.IncompletePages = incompleteCount(manifest)
	s.MissingBlockReferences = missingReferenceCount(manifest)
	return s
}

const (
	metadataIncompletePages        = "incomplete_pages"
	metadataMissingBlockReferences = "missing_block_references"
)

func incompleteCount(manifest exportgen.Manifest) int {
	total := 0
	for _, file := range manifest.Files {
		total += file.Metadata[metadataIncompletePages]
	}
	return total
}

func missingReferenceCount(manifest exportgen.Manifest) int {
	total := 0
	for _, file := range manifest.Files {
		total += file.Metadata[metadataMissingBlockReferences]
	}
	return total
}

func (e Exporter) planPages(ctx context.Context, snapshot *store.Store) ([]exportgen.Item, error) {
	pages, err := snapshot.Pages(ctx)
	if err != nil {
		return nil, err
	}
	resolver, err := newPathResolver(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	items := make([]exportgen.Item, 0, len(pages))
	for _, page := range pages {
		data, err := e.loadPageData(ctx, snapshot, resolver, page)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(e.Dir, data.path)
		if err != nil {
			return nil, err
		}
		items = append(items, exportgen.Item{
			ID:           page.ID,
			RelativePath: rel,
			SourceDigest: data.sourceDigest(),
		})
	}
	return items, nil
}

func (e Exporter) renderPageItem(ctx context.Context, snapshot *store.Store, item exportgen.Item, stagingPath string) (exportgen.FileResult, error) {
	pages, err := snapshot.Pages(ctx)
	if err != nil {
		return exportgen.FileResult{}, err
	}
	var page store.Page
	for _, candidate := range pages {
		if candidate.ID == item.ID {
			page = candidate
			break
		}
	}
	if page.ID == "" {
		return exportgen.FileResult{}, fmt.Errorf("page %s vanished from snapshot", item.ID)
	}
	resolver, err := newPathResolver(ctx, snapshot)
	if err != nil {
		return exportgen.FileResult{}, err
	}
	data, err := e.loadPageData(ctx, snapshot, resolver, page)
	if err != nil {
		return exportgen.FileResult{}, err
	}
	out, err := renderPage(data, resolver)
	if err != nil {
		return exportgen.FileResult{}, err
	}
	result := exportgen.FileResult{Data: []byte(out)}
	if data.coverage.Missing > 0 {
		result.Metadata = map[string]int{
			metadataIncompletePages:        1,
			metadataMissingBlockReferences: data.coverage.Missing,
		}
	}
	return result, nil
}

func (e Exporter) loadPageData(ctx context.Context, st *store.Store, paths pathResolver, page store.Page) (pageData, error) {
	spaceName := paths.spaceName(page.SpaceID)
	teamID := paths.pageTeamID(page)
	teamName := paths.teamName(teamID)
	blocks, err := st.PageBlocks(ctx, page.ID)
	if err != nil {
		return pageData{}, err
	}
	var coverage store.BlockCoverage
	apiBlocksSynced, err := st.HasSyncState(ctx, "api", "page_blocks", page.ID)
	if err != nil {
		return pageData{}, err
	}
	mcpContentSynced, err := st.HasSyncState(ctx, store.SourceNotionMCP, "page_content", page.ID)
	if err != nil {
		return pageData{}, err
	}
	desktopBacked, err := st.RecordHasLiveSource(ctx, "page", page.ID, "desktop")
	if err != nil {
		return pageData{}, err
	}
	if desktopBacked && !apiBlocksSynced && !mcpContentSynced {
		coverage, err = st.PageBlockCoverage(ctx, page.ID)
		if err != nil {
			return pageData{}, err
		}
	}
	comments, err := st.PageComments(ctx, page.ID)
	if err != nil {
		return pageData{}, err
	}
	projected, err := notiontext.ExportFields(map[string]string{
		"source": page.Source,
		"url":    page.URL, "title": page.Title, "icon": page.Icon, "cover": page.Cover,
		"properties_json": page.PropertiesJSON, "raw_json": page.RawJSON,
	})
	if err != nil {
		return pageData{}, err
	}
	page.URL, page.Title = projected["url"], projected["title"]
	page.Icon, page.Cover = projected["icon"], projected["cover"]
	page.PropertiesJSON, page.RawJSON = projected["properties_json"], projected["raw_json"]
	for i := range blocks {
		// Desktop link destinations live under source, outside the generic URL fields.
		target, _ := linkedBlockContent(blocks[i])
		projected, err := notiontext.ExportFields(map[string]string{
			"source": blocks[i].Source,
			"url":    target,
			"text":   blocks[i].Text, "properties_json": blocks[i].PropertiesJSON,
			"raw_json": blocks[i].RawJSON, "content_json": blocks[i].ContentJSON,
			"format_json": blocks[i].FormatJSON,
		})
		if err != nil {
			return pageData{}, err
		}
		blocks[i].Text, blocks[i].PropertiesJSON = projected["text"], projected["properties_json"]
	}
	for i := range comments {
		projected, err := notiontext.ExportFields(map[string]string{"source": comments[i].Source, "text": comments[i].Text, "raw_json": comments[i].RawJSON})
		if err != nil {
			return pageData{}, err
		}
		comments[i].Text = projected["text"]
	}
	spaceSlug := notiontext.Slug(spaceName)
	titleSlug := maxSlug(notiontext.Slug(page.Title), 96)
	name := fmt.Sprintf("%s-%s.md", titleSlug, notiontext.ShortID(page.ID))
	parts := []string{e.Dir, spaceSlug}
	if teamName != "" {
		parts = append(parts, notiontext.Slug(teamName))
	}
	parts = append(parts, name)
	return pageData{
		page:      page,
		blocks:    blocks,
		comments:  comments,
		coverage:  coverage,
		spaceName: spaceName,
		teamID:    teamID,
		teamName:  teamName,
		path:      filepath.Join(parts...),
		apiSynced: apiBlocksSynced,
		mcpSynced: mcpContentSynced,
	}, nil
}

func renderPage(data pageData, paths pathResolver) (string, error) {
	page := data.page
	var b strings.Builder
	writeFrontMatter(&b, page, data.spaceName, data.teamID, data.teamName, data.coverage)
	if page.Title != "" {
		fmt.Fprintf(&b, "# %s\n\n", notiontext.MarkdownEscape(page.Title))
	}
	writeCoverageWarning(&b, data.coverage)
	wroteProperties := writeProperties(&b, paths, page)
	beforeBlocks := b.Len()
	renderBlocks(&b, page.ID, store.PreferredPageContentBlocks(data.blocks, data.apiSynced))
	wroteBlocks := b.Len() > beforeBlocks
	if shouldWriteEmptyDesktopNotice(page, data.comments, wroteProperties, wroteBlocks) {
		b.WriteString("> [!NOTE]\n> No body blocks or non-title properties were present in the Desktop cache. The page may be empty or its body may not have been cached.\n\n")
	}
	if len(data.comments) > 0 {
		if !strings.HasSuffix(b.String(), "\n\n") {
			b.WriteString("\n")
		}
		b.WriteString("## Comments\n\n")
		for _, c := range data.comments {
			text := notiontext.MarkdownEscape(c.Text)
			if text == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s\n", text)
		}
	}
	return strings.TrimRight(b.String(), " \n") + "\n", nil
}

// sourceDigest fingerprints every source input that can change the rendered
// Markdown, including the resolver context (space/team names, parents,
// reference labels) that shapes paths and property rendering.
func (d pageData) sourceDigest() string {
	encode := func(v any) string {
		var b strings.Builder
		if err := json.NewEncoder(&b).Encode(v); err != nil {
			// All values here are plain structs/slices of strings and ints.
			return fmt.Sprintf("%v", v)
		}
		return b.String()
	}
	parts := []string{
		"v2",
		encode(d.page),
		encode(d.blocks),
		encode(d.comments),
		encode(d.coverage),
		d.spaceName, d.teamID, d.teamName,
		fmt.Sprintf("%t/%t", d.apiSynced, d.mcpSynced),
	}
	return exportgen.DigestHash("markdown-page", parts...)
}
