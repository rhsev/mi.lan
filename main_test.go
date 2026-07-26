package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two boundaries worth pinning are the IP gate and the notes path handling;
// the rest here is cheap regression cover for the small parsing helpers.

func TestConfigAllowed(t *testing.T) {
	cfg := &Config{Milan: MilanConfig{AllowedIPs: []string{"192.168.1.*", "10.0.0.5"}}}

	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true}, // localhost is always in, regardless of config
		{"::1", true},
		{"192.168.1.1", true},
		{"192.168.1.195", true},
		{"192.168.11.5", false}, // the wildcard is one octet, not a prefix match
		{"192.168.1.5.6", false},
		{"192.168.1.", false},
		{"10.0.0.5", true},
		{"10.0.0.50", false}, // exact patterns stay exact
		{"", false},
	}
	for _, c := range cases {
		if got := cfg.allowed(c.ip); got != c.want {
			t.Errorf("allowed(%q) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestConfigAllowedWithoutPatterns(t *testing.T) {
	cfg := &Config{}
	if cfg.allowed("192.168.1.1") {
		t.Error("empty allowed_ips must not admit a LAN address")
	}
	if !cfg.allowed("127.0.0.1") {
		t.Error("localhost must stay reachable with an empty config")
	}
}

// notesServer builds a source dir with one note and one asset, plus a secret
// file one level up — the thing a traversal attempt would be reaching for.
func notesServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "secret.html"), "TOP SECRET")
	write(filepath.Join(dir, "note.html"), "<h1>note</h1>")
	write(filepath.Join(dir, "images", "logo.png"), "PNG")

	return &Server{config: &Config{Milan: MilanConfig{
		Notes: []NoteSource{{ID: "src", Path: dir}},
	}}}, root
}

func TestHandleNotes(t *testing.T) {
	s, _ := notesServer(t)

	cases := []struct {
		name     string
		path     string
		wantCode int
		wantBody string
	}{
		{"note", "/notes/src/note.html", 200, "<h1>note</h1>"},
		{"asset", "/notes/src/assets/images/logo.png", 200, "PNG"},
		{"listing", "/notes/src", 200, "note.html"},
		{"unknown source", "/notes/nope/note.html", 404, ""},
		{"missing note", "/notes/src/gone.html", 404, ""},

		// Traversal: the path segments are percent-decoded after the split, so
		// an encoded slash is the interesting case — it cannot reappear as a
		// separator, and neither variant may reach secret.html.
		{"encoded traversal", "/notes/src/..%2F..%2Fsecret.html", 404, ""},
		// ".." alone passes safeFilenameRE (dots are in the class); what stops
		// it is that reading the resulting directory fails. Worth knowing if
		// the filename check is ever rewritten.
		{"literal traversal", "/notes/src/../secret.html", 404, ""},
		{"asset encoded traversal", "/notes/src/assets/images%2F..%2F..%2Fsecret.html", 403, ""},
		{"asset literal traversal", "/notes/src/assets/../secret.html", 403, ""},
		{"asset outside images", "/notes/src/assets/secret.html", 403, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, c.path, nil)
			w := httptest.NewRecorder()
			s.handleNotes(w, r, r.URL.EscapedPath())

			if w.Code != c.wantCode {
				t.Errorf("status = %d, want %d (body: %q)", w.Code, c.wantCode, w.Body.String())
			}
			if c.wantBody != "" && !strings.Contains(w.Body.String(), c.wantBody) {
				t.Errorf("body = %q, want it to contain %q", w.Body.String(), c.wantBody)
			}
			if strings.Contains(w.Body.String(), "TOP SECRET") {
				t.Fatalf("served a file from outside the source dir: %s", c.path)
			}
		})
	}
}

func TestSplitScriptPath(t *testing.T) {
	cases := []struct{ rest, script, arg string }{
		{"cl", "cl", ""},
		{"cl/file/664435710", "cl", "file/664435710"},
		{"album/safari/fresh", "album", "safari/fresh"},
		{"cl/files/Mein%20Binder", "cl", "files/Mein Binder"}, // argument is decoded
		{"", "", ""},
	}
	for _, c := range cases {
		script, arg := splitScriptPath(c.rest)
		if script != c.script || arg != c.arg {
			t.Errorf("splitScriptPath(%q) = (%q, %q), want (%q, %q)", c.rest, script, arg, c.script, c.arg)
		}
	}
}

func TestScriptNameRE(t *testing.T) {
	for _, ok := range []string{"cl", "album", "livesync-restart", "a_b"} {
		if !scriptNameRE.MatchString(ok) {
			t.Errorf("%q should be a valid script name", ok)
		}
	}
	for _, bad := range []string{"../cl", "cl.rb", "cl/file", "", "cl name"} {
		if scriptNameRE.MatchString(bad) {
			t.Errorf("%q must not pass as a script name", bad)
		}
	}
}

func TestBuildCmd(t *testing.T) {
	// The argument becomes its own argv entry — no shell is involved, so it
	// cannot be reinterpreted as a command here.
	got := buildCmd("/s/cl.rb", "file/x; rm -rf /")
	want := []string{rubyBin, "/s/cl.rb", "file/x; rm -rf /"}
	if len(got) != len(want) {
		t.Fatalf("buildCmd = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("buildCmd = %v, want %v", got, want)
		}
	}

	if got := buildCmd("/s/a.sh", ""); len(got) != 2 || got[0] != "sh" {
		t.Errorf("sh script = %v", got)
	}
	if got := buildCmd("/s/a.py", ""); len(got) != 2 || got[0] != "python3" {
		t.Errorf("py script = %v", got)
	}
	if got := buildCmd("/s/a", ""); len(got) != 1 || got[0] != "/s/a" {
		t.Errorf("extensionless script = %v", got)
	}
}

func TestIsHTMLOutput(t *testing.T) {
	yes := []string{"<!DOCTYPE html><html>…", "\n  <html lang=\"de\">", "<htmlx"}
	no := []string{"plain text", "<h1>fragment</h1>", "", "{\"json\": true}"}
	for _, s := range yes {
		if !isHTMLOutput(s) {
			t.Errorf("%q should be served as HTML", s)
		}
	}
	for _, s := range no {
		if isHTMLOutput(s) {
			t.Errorf("%q should not be served as HTML", s)
		}
	}
}
