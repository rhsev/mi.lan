package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// widgetServer points the store at a temp dir for the duration of one test.
func widgetServer(t *testing.T) *Server {
	t.Helper()
	old := widgetsDir
	widgetsDir = t.TempDir()
	t.Cleanup(func() { widgetsDir = old })
	return &Server{config: &Config{}}
}

func pushWidget(t *testing.T, s *Server, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	s.handleWidget(w, r, strings.TrimPrefix(r.URL.Path, "/widget/"))
	return w
}

func readWidgets(t *testing.T, s *Server) []Widget {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleWidgetList(w)
	var widgets []Widget
	if err := json.Unmarshal(w.Body.Bytes(), &widgets); err != nil {
		t.Fatalf("GET /widgets returned %q: %v", w.Body.String(), err)
	}
	return widgets
}

// The target is a path segment on disk, so its validation is the boundary
// worth pinning hardest.
func TestWidgetTargetValidation(t *testing.T) {
	for _, ok := range []string{"copy", "na", "a", "build-2", "some_target", strings.Repeat("a", 32)} {
		if !widgetTargetRE.MatchString(ok) {
			t.Errorf("%q should be a valid target", ok)
		}
	}
	for _, bad := range []string{"", "Copy", "../etc", "a/b", "a.json", "ä", strings.Repeat("a", 33), "with space"} {
		if widgetTargetRE.MatchString(bad) {
			t.Errorf("%q must not pass as a target", bad)
		}
	}

	s := widgetServer(t)
	for _, path := range []string{"/widget/..%2F..%2Fescape", "/widget/UPPER", "/widget/two/segments"} {
		if w := pushWidget(t, s, path, "text/plain", "x"); w.Code != http.StatusBadRequest {
			t.Errorf("push to %s = %d, want 400", path, w.Code)
		}
	}
	if entries, _ := os.ReadDir(widgetsDir); len(entries) != 0 {
		t.Errorf("a rejected target still wrote a file: %v", entries)
	}
}

// The plain branch is the one shell scripts use: body is the text, everything
// else comes from the query string.
func TestWidgetPlainPush(t *testing.T) {
	s := widgetServer(t)
	pushWidget(t, s, "/widget/copy?progress=42&icon=download&ttl=120&title=Backup", "text/plain",
		"large-archive.zip → Backup\n")

	widgets := readWidgets(t, s)
	if len(widgets) != 1 {
		t.Fatalf("got %d widgets, want 1", len(widgets))
	}
	got := widgets[0]
	if got.Target != "copy" || got.Title != "Backup" || got.Text != "large-archive.zip → Backup" {
		t.Errorf("plain push = %+v", got)
	}
	if got.Progress == nil || *got.Progress != 42 || got.Icon != "download" || got.TTL != 120 {
		t.Errorf("query fields not applied: %+v", got)
	}
	if got.UpdatedAt == 0 {
		t.Error("server must stamp updated_at")
	}
}

func TestWidgetJSONPush(t *testing.T) {
	s := widgetServer(t)
	pushWidget(t, s, "/widget/copy", "application/json",
		`{"title":"Backup","text":"\u001b[32mdone\u001b[0m","progress":0,"color":"#34c759","ttl":120}`)

	got := readWidgets(t, s)[0]
	if got.Progress == nil || *got.Progress != 0 {
		t.Errorf("progress 0 must survive as 0%%, not as absent: %+v", got.Progress)
	}
	if !strings.Contains(got.Text, "\x1b[32m") {
		t.Errorf("ANSI must be stored raw, got %q", got.Text)
	}

	// A JSON body without the header is still JSON — `curl -d @tile.json`.
	pushWidget(t, s, "/widget/na", "", `{"text":"3 tasks"}`)
	for _, widget := range readWidgets(t, s) {
		if widget.Target == "na" && widget.Text != "3 tasks" {
			t.Errorf("headerless JSON body = %q", widget.Text)
		}
	}
}

func TestWidgetPushOverwrites(t *testing.T) {
	s := widgetServer(t)
	for _, pct := range []string{"10", "40", "99"} {
		pushWidget(t, s, "/widget/copy?progress="+pct, "text/plain", "running")
	}
	widgets := readWidgets(t, s)
	if len(widgets) != 1 {
		t.Fatalf("a target is a state, not a log: got %d entries", len(widgets))
	}
	if *widgets[0].Progress != 99 {
		t.Errorf("progress = %d, want the last push", *widgets[0].Progress)
	}
}

func TestWidgetProgressClamped(t *testing.T) {
	s := widgetServer(t)
	pushWidget(t, s, "/widget/low?progress=-5", "text/plain", "x")
	pushWidget(t, s, "/widget/high?progress=250", "text/plain", "x")
	pushWidget(t, s, "/widget/junk?progress=nope&ttl=nope", "text/plain", "x")

	for _, widget := range readWidgets(t, s) {
		switch widget.Target {
		case "low":
			if *widget.Progress != 0 {
				t.Errorf("low = %d, want 0", *widget.Progress)
			}
		case "high":
			if *widget.Progress != 100 {
				t.Errorf("high = %d, want 100", *widget.Progress)
			}
		case "junk":
			// An unparseable number loses the field, never the tile.
			if widget.Progress != nil || widget.TTL != 0 {
				t.Errorf("junk = %+v", widget)
			}
		}
	}
}

func TestWidgetListSorted(t *testing.T) {
	s := widgetServer(t)
	pushWidget(t, s, "/widget/zulu", "text/plain", "x")
	pushWidget(t, s, "/widget/alpha", "text/plain", "x")
	pushWidget(t, s, "/widget/first?sort=-1", "text/plain", "x")

	var order []string
	for _, widget := range readWidgets(t, s) {
		order = append(order, widget.Target)
	}
	want := []string{"first", "alpha", "zulu"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// A half-written or hand-edited file must not take the whole list down —
// the board would go blank on a single bad tile.
func TestWidgetListSkipsUnreadable(t *testing.T) {
	s := widgetServer(t)
	pushWidget(t, s, "/widget/good", "text/plain", "x")
	os.WriteFile(widgetsDir+"/broken.json", []byte("{not json"), 0o644)
	os.WriteFile(widgetsDir+"/notes.txt", []byte("ignored"), 0o644)

	widgets := readWidgets(t, s)
	if len(widgets) != 1 || widgets[0].Target != "good" {
		t.Errorf("list = %+v, want only the good tile", widgets)
	}
}

func TestWidgetClear(t *testing.T) {
	s := widgetServer(t)
	pushWidget(t, s, "/widget/copy", "text/plain", "x")

	// GET .../clear — the spelling a browser or a shell without -X can reach
	r := httptest.NewRequest(http.MethodGet, "/widget/copy/clear", nil)
	w := httptest.NewRecorder()
	s.handleWidget(w, r, "copy/clear")
	if w.Code != http.StatusOK {
		t.Errorf("clear = %d", w.Code)
	}
	if len(readWidgets(t, s)) != 0 {
		t.Error("tile survived /clear")
	}

	// DELETE on a target that is already gone stays a success — clearing is
	// idempotent, scripts call it in a trap.
	pushWidget(t, s, "/widget/copy", "text/plain", "x")
	r = httptest.NewRequest(http.MethodDelete, "/widget/copy", nil)
	w = httptest.NewRecorder()
	s.handleWidget(w, r, "copy")
	s.handleWidget(httptest.NewRecorder(), r, "copy")
	if w.Code != http.StatusOK || len(readWidgets(t, s)) != 0 {
		t.Errorf("DELETE = %d, %d tiles left", w.Code, len(readWidgets(t, s)))
	}
}

func TestWidgetMethodGuard(t *testing.T) {
	s := widgetServer(t)
	r := httptest.NewRequest(http.MethodGet, "/widget/copy", nil)
	w := httptest.NewRecorder()
	s.handleWidget(w, r, "copy")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /widget/copy = %d, want 405", w.Code)
	}
}

// The ticker fan-out fires on arrival, once per urgent push, and only then.
func TestWidgetUrgentFiresTicker(t *testing.T) {
	s := widgetServer(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "sent")
	stub := filepath.Join(dir, "ticker")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := tickerBin
	tickerBin = stub
	t.Cleanup(func() { tickerBin = old })

	pushWidget(t, s, "/widget/quiet", "text/plain", "nothing to see")
	pushWidget(t, s, "/widget/loud?urgent=1", "text/plain", "disk almost full")

	var sent []byte
	for range 50 { // the send is a goroutine; give it a moment
		sent, _ = os.ReadFile(log)
		if len(sent) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := strings.TrimSpace(string(sent))
	if got != "--send disk almost full" {
		t.Errorf("ticker got %q", got)
	}
	if strings.Contains(got, "nothing to see") {
		t.Error("a non-urgent push must not reach the ticker")
	}
}

// An empty store answers with [] rather than null: the board's JSON parse
// should not have to special-case "no tiles yet".
func TestWidgetListEmpty(t *testing.T) {
	s := widgetServer(t)
	w := httptest.NewRecorder()
	s.handleWidgetList(w)
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("empty list = %q, want []", got)
	}
}
