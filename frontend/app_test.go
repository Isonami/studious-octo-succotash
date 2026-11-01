package frontend

import (
	"strings"
	"testing"

	"github.com/maxence-charriere/go-app/v10/pkg/app"
)

func TestInitialDirectoryLoading(t *testing.T) {
	d := &dashboard{}
	html := app.HTMLString(d.renderDirsTable())
	if !strings.Contains(html, ">Loading<") || !strings.Contains(html, `role="status"`) {
		t.Fatalf("initial loading indicator missing: %s", html)
	}
	if strings.Contains(html, "No remote directories") {
		t.Fatal("empty state shown before initial load completes")
	}
	d.dirsLoadComplete = true
	html = app.HTMLString(d.renderDirsTable())
	if strings.Contains(html, ">Loading<") || !strings.Contains(html, "No remote directories found.") {
		t.Fatalf("incorrect loaded empty state: %s", html)
	}
}

func TestLoadedDirectoryFiltering(t *testing.T) {
	d := &dashboard{dirsLoadComplete: true, dirs: []dir{{Path: "/shows/example"}}}
	html := app.HTMLString(d.renderDirsTable())
	if !strings.Contains(html, "/shows/example") || strings.Contains(html, ">Loading<") {
		t.Fatalf("missing loaded directory: %s", html)
	}
	d.dirFilter = "missing"
	html = app.HTMLString(d.renderDirsTable())
	if !strings.Contains(html, "No remote directories match this filter.") {
		t.Fatalf("incorrect filter empty state: %s", html)
	}
}

func TestSyncResponsiveMarkup(t *testing.T) {
	d := &dashboard{syncs: []syncItem{{Path: "/shows/example", Progress: 42}}}
	html := app.HTMLString(d.renderSyncsTable())
	for _, want := range []string{"sync-table", "sync-row", "sync-path-cell", "sync-progress-cell", "sync-actions-cell", `data-label="Time Left"`, `data-label="Transferred"`, `data-label="Speed"`, "/shows/example", "42%", "Cancel"} {
		if !strings.Contains(html, want) {
			t.Errorf("sync markup missing %q: %s", want, html)
		}
	}
}
