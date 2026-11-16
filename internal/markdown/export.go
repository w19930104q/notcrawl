package markdown

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/notcrawl/internal/store"
)

func writeFrontMatter(b *strings.Builder, page store.Page, spaceName, teamID, teamName string, coverage store.BlockCoverage) {
	b.WriteString("---\n")
	writeKV(b, "generated_by", "notcrawl")
	writeKV(b, "id", page.ID)
	writeKV(b, "space_id", page.SpaceID)
	writeKV(b, "space", spaceName)
	writeKV(b, "team_id", teamID)
	writeKV(b, "team", teamName)
	writeKV(b, "title", page.Title)
	writeKV(b, "source", page.Source)
	writeKV(b, "notion_url", page.URL)
	writeKV(b, "created_time", formatMS(page.CreatedTime))
	writeKV(b, "last_edited_time", formatMS(page.LastEditedTime))
	if coverage.Missing > 0 {
		b.WriteString("content_complete: false\n")
		b.WriteString("missing_block_references: ")
		b.WriteString(strconv.Itoa(coverage.Missing))
		b.WriteString("\n")
	}
	b.WriteString("---\n\n")
}

func writeCoverageWarning(b *strings.Builder, coverage store.BlockCoverage) {
	if coverage.Missing == 0 {
		return
	}
	noun := "blocks were"
	if coverage.Missing == 1 {
		noun = "block was"
	}
	b.WriteString("> [!WARNING]\n> Incomplete Desktop cache snapshot: ")
	b.WriteString(strconv.Itoa(coverage.Missing))
	b.WriteString(" referenced " + noun + " not available locally. API sync can retrieve content shared with a Notion integration.\n\n")
}

func shouldWriteEmptyDesktopNotice(page store.Page, comments []store.Comment, wroteProperties, wroteBlocks bool) bool {
	return strings.Contains(page.Source, "desktop") && !wroteProperties && !wroteBlocks && len(comments) == 0
}

func writeKV(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	value = strings.ReplaceAll(value, "\n", " ")
	// %q applies Go (YAML-compatible) escaping for backslashes, quotes, and
	// control or line-separator characters.
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(strconv.Quote(value))
	b.WriteString("\n")
}

// isNotcrawlGeneratedMarkdown reports whether path begins with notcrawl's
// generated front matter with a closing delimiter. Quoting the ownership
// marker in a document body does not make that file generated. It is the
// ownership gate used before pruning stale export output.
func isNotcrawlGeneratedMarkdown(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	text := string(data)
	return strings.HasPrefix(text, "---\ngenerated_by: \"notcrawl\"\n") && strings.Contains(text, "\n---\n")
}

func formatMS(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func maxSlug(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := 0
	for i := range s {
		if i > max {
			break
		}
		cut = i
	}
	s = strings.TrimRight(s[:cut], "-")
	if s == "" {
		return "untitled"
	}
	return s
}
