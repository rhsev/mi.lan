package main

// Widget inbox — passive state tiles.
//
// One target = one state, not a log: a push overwrites the target's file.
// `cp` rewrites itself once a second; nobody wants 400 progress lines.
//
//	Script → POST /widget/<target> → data/widgets/<target>.json
//	                               ├→ ticker (only when urgent)
//	Dylan  ← GET  /widgets  (all at once) ←┘
//
// Events (ticker) fire on arrival, states are read on collection. Keep the
// two apart.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var widgetsDir = filepath.Join(base, "data", "widgets")

// A target is a path segment on disk, so it is validated strictly rather than
// sanitised: anything else is a 400.
var widgetTargetRE = regexp.MustCompile(`\A[a-z0-9_-]{1,32}\z`)

// Widget is the stored state of one target. Every field except target is
// optional; the server only ever fills in UpdatedAt.
//
// Text may contain ANSI — Milan stores it raw, rendering is Dylan's business.
type Widget struct {
	Target    string `json:"target"`
	Title     string `json:"title,omitempty"`
	Text      string `json:"text,omitempty"`
	Progress  *int   `json:"progress,omitempty"` // pointer: 0% is not "absent"
	Icon      string `json:"icon,omitempty"`
	Color     string `json:"color,omitempty"`
	Urgent    bool   `json:"urgent,omitempty"`
	TTL       int    `json:"ttl,omitempty"`  // seconds; after that the tile is stale
	Sort      int    `json:"sort,omitempty"` // display order, lowest first
	UpdatedAt int64  `json:"updated_at"`
}

func widgetPath(target string) string {
	return filepath.Join(widgetsDir, target+".json")
}

// ─── Routes ──────────────────────────────────────────────────────────────────

// handleWidget serves everything under /widget/: the push itself and the two
// spellings of "remove this tile".
func (s *Server) handleWidget(w http.ResponseWriter, r *http.Request, rest string) {
	target := rest
	clear := false
	if t, ok := strings.CutSuffix(rest, "/clear"); ok {
		target, clear = t, true
	}
	if !widgetTargetRE.MatchString(target) {
		http.Error(w, "Invalid target", http.StatusBadRequest)
		return
	}

	switch {
	case clear || r.Method == http.MethodDelete:
		os.Remove(widgetPath(target))
		fmt.Fprintf(w, "cleared: %s", target)
	case r.Method == http.MethodPost || r.Method == http.MethodPut:
		s.receiveWidget(w, r, target)
	default:
		http.Error(w, "Use POST to push, DELETE or /clear to remove", http.StatusMethodNotAllowed)
	}
}

func (s *Server) receiveWidget(w http.ResponseWriter, r *http.Request, target string) {
	widget, err := parseWidget(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	widget.Target = target
	widget.UpdatedAt = time.Now().Unix()

	data, err := json.Marshal(widget)
	if err != nil {
		http.Error(w, "Encode error", http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(widgetsDir, 0o755); err != nil {
		http.Error(w, "Storage error", http.StatusInternalServerError)
		return
	}
	atomicWrite(widgetPath(target), data)

	if widget.Urgent && strings.TrimSpace(widget.Text) != "" {
		s.tickerSend(widget.Text)
	}
	fmt.Fprintf(w, "ok: %s", target)
}

// handleWidgetList answers one Dylan refresh with every target at once — the
// board polls this route, so it must never be N requests.
func (s *Server) handleWidgetList(w http.ResponseWriter) {
	writeJSON(w, s.listWidgets())
}

func (s *Server) listWidgets() []Widget {
	entries, _ := os.ReadDir(widgetsDir)
	widgets := []Widget{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(widgetsDir, e.Name()))
		if err != nil {
			continue
		}
		var widget Widget
		if json.Unmarshal(data, &widget) != nil {
			continue // a half-written or hand-edited file must not kill the list
		}
		if widget.Target == "" {
			widget.Target = strings.TrimSuffix(e.Name(), ".json")
		}
		widgets = append(widgets, widget)
	}
	sort.Slice(widgets, func(i, j int) bool {
		if widgets[i].Sort != widgets[j].Sort {
			return widgets[i].Sort < widgets[j].Sort
		}
		return widgets[i].Target < widgets[j].Target
	})
	return widgets
}

// ─── Payload ─────────────────────────────────────────────────────────────────

const maxWidgetBody = 64 << 10

// parseWidget reads either a JSON body or a text/plain body plus query
// parameters. The plain branch is the one shell scripts actually use:
//
//	curl -d "large-archive.zip → Backup" 'http://127.0.0.1:8080/widget/copy?progress=42'
func parseWidget(r *http.Request) (Widget, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, maxWidgetBody+1))
		if len(body) > maxWidgetBody {
			return Widget{}, fmt.Errorf("body too large (max %d bytes)", maxWidgetBody)
		}
	}

	var widget Widget
	if isJSONWidget(r, body) {
		if err := json.Unmarshal(body, &widget); err != nil {
			return Widget{}, fmt.Errorf("invalid JSON: %v", err)
		}
	} else {
		widget.Text = strings.TrimRight(string(body), "\n")
	}
	applyWidgetQuery(&widget, r)

	if widget.Progress != nil {
		p := min(max(*widget.Progress, 0), 100)
		widget.Progress = &p
	}
	if widget.TTL < 0 {
		widget.TTL = 0
	}
	return widget, nil
}

// isJSONWidget decides by content-type, falling back to the shape of the body
// so that a `curl -d @tile.json` without an explicit header still works.
func isJSONWidget(r *http.Request, body []byte) bool {
	if strings.Contains(r.Header.Get("Content-Type"), "json") {
		return true
	}
	trimmed := strings.TrimSpace(string(body))
	return strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")
}

// applyWidgetQuery lets query parameters fill in the fields the plain body
// cannot carry. Present-but-unparseable values are ignored rather than fatal —
// a typo in a progress number should not lose the tile.
func applyWidgetQuery(widget *Widget, r *http.Request) {
	q := r.URL.Query()
	str := func(key string, dst *string) {
		if v := q.Get(key); v != "" {
			*dst = v
		}
	}
	str("title", &widget.Title)
	str("icon", &widget.Icon)
	str("color", &widget.Color)
	if v := q.Get("text"); v != "" {
		widget.Text = v
	}
	if v := q.Get("progress"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			widget.Progress = &n
		}
	}
	for _, key := range []string{"ttl", "sort"} {
		v := q.Get(key)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		if key == "ttl" {
			widget.TTL = n
		} else {
			widget.Sort = n
		}
	}
	if v := q.Get("urgent"); v != "" {
		widget.Urgent = v != "0" && v != "false"
	}
}

// ─── Ticker fan-out ──────────────────────────────────────────────────────────

// tickerBin resolves the ticker CLI once. A LaunchAgent gets a minimal PATH,
// so the usual install locations are probed explicitly.
var tickerBin = func() string {
	if path, err := exec.LookPath("ticker"); err == nil {
		return path
	}
	home, _ := os.UserHomeDir()
	for _, path := range []string{
		filepath.Join(home, "bin", "ticker"),
		"/opt/homebrew/bin/ticker",
		"/usr/local/bin/ticker",
	} {
		if isScriptFile(path, "") {
			return path
		}
	}
	return ""
}()

// tickerSend fires the event once per urgent push, on arrival. Not
// configurable in phase 1 — first find out whether it gets annoying.
func (s *Server) tickerSend(text string) {
	if tickerBin == "" {
		s.logf("warn", "urgent widget but no ticker binary found")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := exec.CommandContext(ctx, tickerBin, "--send", text).Run(); err != nil {
			s.logf("warn", "ticker --send failed: %v", err)
		}
	}()
}
