package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

// With bind set, the machine's own address is the only door the control
// commands have — it must be trusted like loopback, and only exactly.
func TestConfigAllowedBindSelf(t *testing.T) {
	cfg := &Config{Milan: MilanConfig{Bind: "100.86.161.44"}}
	if !cfg.allowed("100.86.161.44") {
		t.Error("the bind address itself must be allowed")
	}
	if cfg.allowed("100.86.161.45") {
		t.Error("a neighbouring address must not ride along")
	}
	if (&Config{}).allowed("") {
		t.Error("empty bind must not admit an empty source address")
	}
}

func TestListenAddrAndControlURL(t *testing.T) {
	cases := []struct {
		bind, listen, control string
	}{
		{"", ":8080", "http://localhost:8080/health"},
		{"0.0.0.0", "0.0.0.0:8080", "http://localhost:8080/health"},
		{"::", "[::]:8080", "http://localhost:8080/health"},
		{"100.86.161.44", "100.86.161.44:8080", "http://100.86.161.44:8080/health"},
	}
	for _, c := range cases {
		cfg := &Config{Milan: MilanConfig{Port: 8080, Bind: c.bind}}
		if got := cfg.listenAddr(); got != c.listen {
			t.Errorf("listenAddr(bind=%q) = %q, want %q", c.bind, got, c.listen)
		}
		if got := cfg.controlURL("/health"); got != c.control {
			t.Errorf("controlURL(bind=%q) = %q, want %q", c.bind, got, c.control)
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

// findScript's precedence is what lets a ported script replace its original
// without the endpoint changing, so pin it — including the guards that the
// empty extension makes necessary.
func TestFindScriptPrecedence(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(custom, "both.rb"), 0o644)
	write(filepath.Join(custom, "both"), 0o755)      // compiled: must win
	write(filepath.Join(custom, "onlyrb.rb"), 0o644) // no binary: script
	write(filepath.Join(custom, "plain"), 0o644)     // not executable: ignored
	if err := os.MkdirAll(filepath.Join(custom, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &Server{config: &Config{Milan: MilanConfig{ScriptsDir: dir}}}

	cases := []struct{ name, want string }{
		{"both", filepath.Join(custom, "both")},
		{"onlyrb", filepath.Join(custom, "onlyrb.rb")},
		{"plain", ""}, // a regular file without the executable bit is not a script
		{"adir", ""},  // a directory must never resolve
		{"nope", ""},
	}
	for _, c := range cases {
		if got := s.findScript(c.name); got != c.want {
			t.Errorf("findScript(%q) = %q, want %q", c.name, got, c.want)
		}
	}

	// buildCmd runs a binary directly, a script through its interpreter
	if got := buildCmd(filepath.Join(custom, "both"), "")[0]; got != filepath.Join(custom, "both") {
		t.Errorf("buildCmd(binary)[0] = %q, want the binary itself", got)
	}
	if got := buildCmd(filepath.Join(custom, "onlyrb.rb"), "")[0]; got != rubyBin {
		t.Errorf("buildCmd(.rb)[0] = %q, want %q", got, rubyBin)
	}

	list := s.listScripts()
	joined := strings.Join(list, ",")
	for _, want := range []string{"both", "onlyrb"} {
		if !strings.Contains(joined, want) {
			t.Errorf("listScripts() = %v, missing %q", list, want)
		}
	}
	for _, unwanted := range []string{"plain", "adir"} {
		for _, got := range list {
			if got == unwanted {
				t.Errorf("listScripts() = %v, should not contain %q", list, unwanted)
			}
		}
	}
}

// The vectors are openssl's, not this package's own hmac output — a test that
// computes its expectation with the code under test would pass forever.
//
//	printf '%s' 'hello stdin' | openssl dgst -sha256 -hmac 's3cr3t'
const (
	sigHelloStdin = "sha256=2af4e91bd5a3a6a4270dd7662b36bed6e411d05c6d12644c42b541c878b7cd40"
	sigEmptyBody  = "sha256=3c81cc9496e1c25250f6ccb85f697c1bb623e3480d6538ad8cb6a6648142777d"
)

func TestValidSignature(t *testing.T) {
	body := []byte("hello stdin")
	if !validSignature("s3cr3t", body, sigHelloStdin) {
		t.Error("the openssl vector must verify")
	}
	if !validSignature("s3cr3t", nil, sigEmptyBody) {
		t.Error("an empty body has a signature too")
	}
	for name, header := range map[string]string{
		"missing":      "",
		"no prefix":    strings.TrimPrefix(sigHelloStdin, "sha256="),
		"wrong scheme": "sha1=" + strings.TrimPrefix(sigHelloStdin, "sha256="),
		"bad hex":      "sha256=zz",
		"wrong body":   sigEmptyBody,
	} {
		if validSignature("s3cr3t", body, header) {
			t.Errorf("%s signature must not verify", name)
		}
	}
	if validSignature("other", body, sigHelloStdin) {
		t.Error("a different secret must not verify")
	}
}

// scriptServer builds a scripts dir with one `cat` endpoint and allows the
// address httptest stamps on requests, so tests can go through ServeHTTP —
// the IP gate and payload handling included.
func scriptServer(t *testing.T, secrets map[string]string) *Server {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat\n"
	if err := os.WriteFile(filepath.Join(dir, "cat.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Server{config: &Config{Milan: MilanConfig{
		AllowedIPs: []string{"192.0.2.1"},
		ScriptsDir: dir,
		Secrets:    secrets,
	}}}
}

func TestScriptPayloadOnStdin(t *testing.T) {
	s := scriptServer(t, nil)

	r := httptest.NewRequest(http.MethodPost, "/cat", strings.NewReader("hello stdin"))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "hello stdin" {
		t.Errorf("POST body did not reach stdin: %d %q", w.Code, w.Body.String())
	}

	// GET has no body; the script must see EOF, not block on a missing pipe.
	r = httptest.NewRequest(http.MethodGet, "/cat", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "" {
		t.Errorf("GET = %d %q, want 200 with empty body", w.Code, w.Body.String())
	}
}

func TestScriptPayloadStreamsToStdin(t *testing.T) {
	s := scriptServer(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/stream/cat", strings.NewReader("hello stdin"))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "data: hello stdin") {
		t.Errorf("stream = %d %q, want the payload as an SSE data event", w.Code, w.Body.String())
	}
}

func TestScriptSecretGate(t *testing.T) {
	s := scriptServer(t, map[string]string{"cat": "s3cr3t"})

	cases := []struct {
		name     string
		method   string
		body     string
		sig      string
		wantCode int
	}{
		{"signed POST", http.MethodPost, "hello stdin", sigHelloStdin, 200},
		{"signed GET", http.MethodGet, "", sigEmptyBody, 200},
		{"unsigned POST", http.MethodPost, "hello stdin", "", 403},
		{"unsigned GET", http.MethodGet, "", "", 403},
		{"tampered body", http.MethodPost, "hello stdim", sigHelloStdin, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "/cat", strings.NewReader(c.body))
			if c.sig != "" {
				r.Header.Set("X-Hub-Signature-256", c.sig)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != c.wantCode {
				t.Errorf("status = %d, want %d (body: %q)", w.Code, c.wantCode, w.Body.String())
			}
		})
	}

	// A script without an entry in secrets: stays open — opt-in, not global.
	dir := s.config.Milan.ScriptsDir
	if err := os.WriteFile(filepath.Join(dir, "open.sh"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/open", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("unlisted script = %d, want 200", w.Code)
	}
}

// A missing type means the browser downloads the file instead of showing it —
// the case that matters for album attachments, so the mapping is pinned.
func TestMimeForExt(t *testing.T) {
	cases := map[string]string{
		".png": "image/png",
		".JPG": "image/jpeg",
		".pdf": "application/pdf",
		".md":  "text/plain; charset=utf-8",
		".mov": "video/quicktime",
		".zip": "application/octet-stream",
		"":     "application/octet-stream",
	}
	for ext, want := range cases {
		if got := mimeForExt(ext); got != want {
			t.Errorf("mimeForExt(%q) = %q, want %q", ext, got, want)
		}
	}
}

// The bug this pins: `milan stop` under launchd killed its process, launchd
// brought a successor up within the second, the successor wrote its own pid —
// and stop's unconditional cleanup then deleted that file. Milan was left
// serving happily while status said "not running" and start said "port in use".
func TestRemovePidFileKeepsASuccessorsFile(t *testing.T) {
	dir := t.TempDir()
	old := pidFile
	pidFile = filepath.Join(dir, "milan.pid")
	defer func() { pidFile = old }()

	if err := os.WriteFile(pidFile, []byte("20235\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removePidFileIf(16962) // the process we killed, not the one in the file

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("successor's pid file was deleted: %v", err)
	}
	if strings.TrimSpace(string(data)) != "20235" {
		t.Errorf("pid file = %q, want 20235", strings.TrimSpace(string(data)))
	}
}

func TestRemovePidFileDropsOurOwn(t *testing.T) {
	dir := t.TempDir()
	old := pidFile
	pidFile = filepath.Join(dir, "milan.pid")
	defer func() { pidFile = old }()

	if err := os.WriteFile(pidFile, []byte("16962"), 0o644); err != nil {
		t.Fatal(err)
	}

	removePidFileIf(16962)

	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Errorf("own pid file survived: %v", err)
	}
}

// A file we cannot read is stale by definition — clearing it is the point.
func TestRemovePidFileDropsGarbage(t *testing.T) {
	dir := t.TempDir()
	old := pidFile
	pidFile = filepath.Join(dir, "milan.pid")
	defer func() { pidFile = old }()

	if err := os.WriteFile(pidFile, []byte("not a pid"), 0o644); err != nil {
		t.Fatal(err)
	}

	removePidFileIf(16962)

	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Errorf("garbage pid file survived: %v", err)
	}
}

// `go test` links an ad-hoc signed binary on macOS — exactly the build the
// warning exists for — so the test binary itself must trip it.
func TestSignatureWarningOnAdHocBuild(t *testing.T) {
	if runtime.GOOS != "darwin" {
		if w := signatureWarning(); w != "" {
			t.Fatalf("no signature check off macOS, got %q", w)
		}
		return
	}
	if w := signatureWarning(); !strings.Contains(w, "ad-hoc") {
		t.Fatalf("ad-hoc test binary not flagged, got %q", w)
	}
}

// launchctl print output as macOS 15 writes it, trimmed. The nested
// `program` line stands for any deeper dictionary that happens to use the key.
const launchctlPrintSample = "gui/501/rhsev.milan = {\n" +
	"\tactive count = 1\n" +
	"\tpath = /Library/LaunchAgents/rhsev.milan.plist\n" +
	"\tstate = running\n" +
	"\n" +
	"\tprogram = %s\n" +
	"\targuments = {\n" +
	"\t\t%s\n" +
	"\t\tserve\n" +
	"\t}\n" +
	"\tevent triggers = {\n" +
	"\t\tprogram = /usr/libexec/elsewhere\n" +
	"\t}\n" +
	"}\n"

func TestLaunchdProgram(t *testing.T) {
	out := strings.ReplaceAll(launchctlPrintSample, "%s", "/opt/milan/milan")
	if got := launchdProgram(out); got != "/opt/milan/milan" {
		t.Errorf("launchdProgram = %q, want /opt/milan/milan", got)
	}
	if got := launchdProgram("gui/501/x = {\n\t\tprogram = /deep\n}\n"); got != "" {
		t.Errorf("nested program line taken: %q", got)
	}
}

// The case that went wrong: a second milan on the same machine claimed the
// live agent, so its restart kickstarted the wrong process.
func TestIsLaunchdProgram(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live", "milan")
	other := filepath.Join(dir, "probe", "milan")
	link := filepath.Join(dir, "bin-milan")
	for _, f := range []string{live, other} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("bin"), 0o755)
	}
	os.Symlink(live, link)
	out := strings.ReplaceAll(launchctlPrintSample, "%s", live)

	cases := []struct {
		name, out, self string
		want            bool
	}{
		{"the agent's own binary", out, live, true},
		{"through a symlink", out, link, true},
		{"another copy", out, other, false},
		{"agent's binary gone", strings.ReplaceAll(launchctlPrintSample, "%s", filepath.Join(dir, "gone")), live, false},
		{"output without program line", "gui/501/rhsev.milan = {\n}\n", other, true},
		{"own path unknown", out, "", true},
	}
	for _, c := range cases {
		if got := isLaunchdProgram(c.out, c.self); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// whoamiFixture points checkIdentity at a stand-in Dylan and a config dir of
// its own, with scutil answering host ("" makes it fail). reply sees the name
// milan sent and returns the status and body Dylan answers with.
func whoamiFixture(t *testing.T, config, host string, reply func(name string) (int, string)) {
	t.Helper()
	dir := t.TempDir()
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, body := reply(r.URL.Query().Get("name"))
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	oldBase, oldURL, oldHost := base, dylanURL, localHostName
	base, dylanURL = dir, srv.URL+"/whoami"
	localHostName = func() (string, error) {
		if host == "" {
			return "", errors.New("scutil: not found")
		}
		return host, nil
	}
	t.Cleanup(func() { base, dylanURL, localHostName = oldBase, oldURL, oldHost })
}

// captureStdout returns what fn printed — checkIdentity reports on stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// A name in config.yaml wins over the hostname, so a machine whose Bonjour
// name differs from its agent name can still say who it is.
func TestCheckIdentityNameFromConfig(t *testing.T) {
	var sent string
	whoamiFixture(t, "milan:\n  name: book\n", "Mini", func(name string) (int, string) {
		sent = name
		return http.StatusOK, name + " (192.168.1.187)"
	})

	var got string
	var ok bool
	out := captureStdout(t, func() { got, ok = checkIdentity() })
	if sent != "book" {
		t.Errorf("sent name %q, want book", sent)
	}
	if !ok || got != "book" {
		t.Errorf("checkIdentity() = %q, %v, want book, true", got, ok)
	}
	if !strings.Contains(out, "OK - I am book (192.168.1.187)") {
		t.Errorf("output = %q", out)
	}
}

// Without a name in config, LocalHostName is used — lower-cased, since macOS
// reports "Mini" where Dylan's agent is keyed "mini".
func TestCheckIdentityNameFromHostname(t *testing.T) {
	var sent string
	whoamiFixture(t, "", "Mini", func(name string) (int, string) {
		sent = name
		return http.StatusOK, name + " (192.168.1.118)"
	})

	var got string
	var ok bool
	captureStdout(t, func() { got, ok = checkIdentity() })
	if sent != "mini" {
		t.Errorf("sent name %q, want mini", sent)
	}
	if !ok || got != "mini" {
		t.Errorf("checkIdentity() = %q, %v, want mini, true", got, ok)
	}
}

// No name anywhere (Linux has no scutil): refuse instead of asking Dylan
// without a claim.
func TestCheckIdentityWithoutName(t *testing.T) {
	asked := false
	whoamiFixture(t, "", "", func(string) (int, string) {
		asked = true
		return http.StatusOK, "mini (192.168.1.118)"
	})

	var ok bool
	out := captureStdout(t, func() { _, ok = checkIdentity() })
	if ok || asked {
		t.Errorf("ok = %v, asked Dylan = %v, want false, false", ok, asked)
	}
	if !strings.Contains(out, "FAILED (cannot determine own name") {
		t.Errorf("output = %q", out)
	}
}

// The bug this pins: the MacBook bridged over the mini arrives NATed as
// 192.168.1.118, Dylan answered by address alone, and milan took "mini" at
// its word. An answer for another agent must stop it even when it is a 200.
func TestCheckIdentityMismatch(t *testing.T) {
	whoamiFixture(t, "milan:\n  name: book\n", "", func(string) (int, string) {
		return http.StatusOK, "mini (192.168.1.118)"
	})

	var got string
	var ok bool
	out := captureStdout(t, func() { got, ok = checkIdentity() })
	if ok || got != "" {
		t.Errorf("checkIdentity() = %q, %v, want \"\", false", got, ok)
	}
	if want := "MISMATCH - Dylan identifies this host as mini, expected book"; !strings.Contains(out, want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
	}
}

// A current Dylan refuses the claim itself; that stays a plain rejection.
func TestCheckIdentityRejectedByDylan(t *testing.T) {
	whoamiFixture(t, "milan:\n  name: book\n", "", func(string) (int, string) {
		return http.StatusConflict, "book is registered at 192.168.1.187 but called from 192.168.1.118 (registered as mini)"
	})

	var ok bool
	out := captureStdout(t, func() { _, ok = checkIdentity() })
	if ok {
		t.Error("checkIdentity() accepted a 409")
	}
	if !strings.Contains(out, "REJECTED (HTTP 409)") {
		t.Errorf("output = %q", out)
	}
}

// DYLAN_URL can point anywhere, query included; the name is encoded into it.
func TestWhoamiURL(t *testing.T) {
	cases := []struct {
		raw, name, want string
	}{
		{"http://dy.lan/whoami", "book", "http://dy.lan/whoami?name=book"},
		{"http://192.168.1.33:8080/whoami", "mac mini&x", "http://192.168.1.33:8080/whoami?name=mac+mini%26x"},
		{"http://dy.lan/whoami?debug=1", "book", "http://dy.lan/whoami?debug=1&name=book"},
		{"http://dy.lan/whoami?name=mini", "book", "http://dy.lan/whoami?name=book"},
	}
	for _, c := range cases {
		got, err := whoamiURL(c.raw, c.name)
		if err != nil || got != c.want {
			t.Errorf("whoamiURL(%q, %q) = %q, %v, want %q", c.raw, c.name, got, err, c.want)
		}
	}
	if _, err := whoamiURL("http://dy.lan/%zz", "book"); err == nil {
		t.Error("malformed URL accepted")
	}
}
