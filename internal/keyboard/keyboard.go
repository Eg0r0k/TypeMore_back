// Package keyboard owns the layouts DATA asset (layouts/*.json — see the
// README there) and the two small things every consumer needs from it: the
// char → physical-key mapping the replay worker's projection folds through,
// and the public HTTP surface that hands the layout geometry to the frontend's
// heatmap. The asset is data, not code: this package parses it, it does not
// define it.
package keyboard

import (
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

//go:embed layouts/*.json
var layoutFS embed.FS

// OtherKey is the reserved bucket for characters no layout maps: aggregates
// must stay complete, so an unmapped symbol is counted there, never dropped.
const OtherKey = "other"

// Key is one physical key of a layout, as the asset states it.
type Key struct {
	ID     string   `json:"id"`
	Row    int      `json:"row"`
	Col    int      `json:"col"`
	Finger string   `json:"finger"`
	Hand   string   `json:"hand"`
	Chars  []string `json:"chars"`
	Width  int      `json:"width,omitempty"`
}

// Layout is one parsed layout document.
type Layout struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Keys  []Key  `json:"keys"`
}

// Layouts is the parsed asset: every layout, plus the char→key index each one
// implies. Built once at startup; a malformed asset is a startup error — a
// projection quietly mapping everything to 'other' must not be deployable.
type Layouts struct {
	all    []Layout
	raw    json.RawMessage // the /api/v1/layouts response body, marshalled once
	byName map[string]map[string]string
}

// Load parses the embedded asset.
func Load() (*Layouts, error) {
	entries, err := layoutFS.ReadDir("layouts")
	if err != nil {
		return nil, fmt.Errorf("keyboard: read layouts dir: %w", err)
	}
	l := &Layouts{byName: make(map[string]map[string]string)}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := layoutFS.ReadFile("layouts/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("keyboard: read %s: %w", e.Name(), err)
		}
		var layout Layout
		if err := json.Unmarshal(body, &layout); err != nil {
			return nil, fmt.Errorf("keyboard: parse %s: %w", e.Name(), err)
		}
		if layout.Name == "" || len(layout.Keys) == 0 {
			return nil, fmt.Errorf("keyboard: %s: a layout needs a name and keys", e.Name())
		}
		index := make(map[string]string)
		for _, k := range layout.Keys {
			for _, ch := range k.Chars {
				if prev, dup := index[ch]; dup && prev != k.ID {
					return nil, fmt.Errorf("keyboard: %s: %q maps to both %s and %s",
						layout.Name, ch, prev, k.ID)
				}
				index[ch] = k.ID
			}
		}
		l.all = append(l.all, layout)
		l.byName[layout.Name] = index
	}
	if _, ok := l.byName["qwerty"]; !ok {
		return nil, fmt.Errorf("keyboard: the qwerty layout is missing")
	}
	if _, ok := l.byName["jcuken"]; !ok {
		return nil, fmt.Errorf("keyboard: the jcuken layout is missing")
	}
	raw, err := json.Marshal(struct {
		Layouts []Layout `json:"layouts"`
	}{Layouts: l.all})
	if err != nil {
		return nil, fmt.Errorf("keyboard: marshal layouts: %w", err)
	}
	l.raw = raw
	return l, nil
}

// LayoutFor names the layout a run's dictionary language maps through: the
// one place the language → layout heuristic lives (see the asset README for
// why it is a heuristic and what replaces it).
func (l *Layouts) LayoutFor(lang string) string {
	lower := strings.ToLower(lang)
	if strings.HasPrefix(lower, "ru") || strings.Contains(lower, "russian") {
		return "jcuken"
	}
	return "qwerty"
}

// KeyOf maps one typed character onto its physical key for a run of the given
// language. Unmapped characters bucket to OtherKey, never disappear.
func (l *Layouts) KeyOf(lang, char string) string {
	if id, ok := l.byName[l.LayoutFor(lang)][char]; ok {
		return id
	}
	return OtherKey
}

// All returns the parsed layouts (the heatmap test surface).
func (l *Layouts) All() []Layout { return l.all }

// Routes serves GET /api/v1/layouts: the asset, verbatim, public and
// immutable-cacheable for a day — a guest picking a layout to look at has no
// account, exactly like the dictionaries.
func (l *Layouts) Routes(log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		if _, err := w.Write(l.raw); err != nil {
			log.Error("write layouts", "err", err)
		}
	})
}
