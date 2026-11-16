package markdown

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnershipGatePreservesMarkdownWithoutGeneratedFrontMatter(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"generated", "---\ngenerated_by: \"notcrawl\"\nid: \"x\"\n---\n\nbody", true},
		{"quoted export", "# Export example\n\n```yaml\n---\ngenerated_by: \"notcrawl\"\n---\n```\n", false},
		{"marker in body", "---\ntitle: Notes\n---\ngenerated_by: \"notcrawl\"\n", false},
		{"unterminated header", "---\ngenerated_by: \"notcrawl\"\n", false},
		{"plain notes", "user notes", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "notes.md")
			if err := os.WriteFile(path, []byte(tc.text), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := isNotcrawlGeneratedMarkdown(path); got != tc.want {
				t.Fatalf("isNotcrawlGeneratedMarkdown = %v, want %v", got, tc.want)
			}
		})
	}
}
