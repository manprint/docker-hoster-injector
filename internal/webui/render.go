package webui

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"sync"
)

//go:embed templates/index.html
var indexTemplate string

//go:embed assets/style.css
var styleCSS string

// The page is rendered from a template that inlines the stylesheet and the
// initial snapshot. Compiling it once at startup and reusing the result would
// be faster but would bake in a stale snapshot, so it is parsed once and
// executed per request: parsing dominates only on the first call.
var (
	tmplOnce sync.Once
	tmpl     *template.Template
	tmplErr  error
)

// indexData is the template's input.
type indexData struct {
	Config       Config
	CSS          template.CSS
	Snapshot     Snapshot
	SnapshotJSON template.JS
}

// renderIndex produces the full HTML page.
func renderIndex(cfg Config, snap Snapshot) (string, error) {
	tmplOnce.Do(func() {
		tmpl, tmplErr = template.New("index").Parse(indexTemplate)
	})
	if tmplErr != nil {
		return "", fmt.Errorf("parse the page template: %w", tmplErr)
	}

	// The snapshot is embedded as JSON for the page's first paint, so the
	// table is populated without waiting for the event stream.
	raw, err := json.Marshal(snap)
	if err != nil {
		return "", fmt.Errorf("encode the snapshot: %w", err)
	}

	data := indexData{
		Config:       cfg,
		CSS:          template.CSS(styleCSS),
		Snapshot:     snap,
		SnapshotJSON: template.JS(raw),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute the page template: %w", err)
	}
	return buf.String(), nil
}
