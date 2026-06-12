package main

// Milan — Minimalist Script Executor for macOS
//
// Single binary: control commands + embedded HTTP server.
//
//	milan start [--standalone]   Start the server daemon
//	milan stop                   Stop the server
//	milan restart [--standalone] Restart
//	milan status                 Show status
//	milan log                    Tail the log
//	milan whoami                 Check identity with Dylan
//
// URL schema: http://mi.lan/<script>/<arg>
//             http://mi.lan/stream/<script>/<arg>  (SSE)

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const version = "2.0.1"

var (
	scriptExtensions = []string{".rb", ".sh", ".py"}
	scriptSubdirs    = []string{"custom", ""}
)

// ─── Paths ────────────────────────────────────────────────────────────────────

// selfDir returns the directory of the real binary (symlinks resolved).
// Falls back to CWD when run via "go run".
func selfDir() string {
	exe, err := os.Executable()
	if err != nil {
		cwd, _ := os.Getwd()
		return cwd
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		real = exe
	}
	return filepath.Dir(real)
}

var (
	base    = selfDir()
	pidFile = filepath.Join(base, "milan.pid")
	logPath = filepath.Join(base, "milan.log")
	jobsDir = filepath.Join(base, "data", "jobs")
)

// ─── Config ───────────────────────────────────────────────────────────────────

type NoteSource struct {
	ID   string `yaml:"id"`
	Path string `yaml:"path"`
}

type MilanConfig struct {
	Port         int          `yaml:"port"`
	AllowedIPs   []string     `yaml:"allowed_ips"`
	ScriptsDir   string       `yaml:"scripts_dir"`
	CheatersDir  string       `yaml:"cheaters_dir"`
	Notes        []NoteSource `yaml:"notes"`
	CronInterval int          `yaml:"cron_interval"`
}

type Config struct {
	Milan MilanConfig `yaml:"milan"`
}

func loadConfig() (*Config, error) {
	path := filepath.Join(base, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config not found: %s", path)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config parse error: %w", err)
	}
	m := &cfg.Milan
	if m.Port == 0 {
		m.Port = 8080
	}
	if m.ScriptsDir == "" {
		m.ScriptsDir = "./scripts"
	}
	if m.CronInterval == 0 {
		m.CronInterval = 300
	}
	if !filepath.IsAbs(m.ScriptsDir) {
		m.ScriptsDir = filepath.Join(base, m.ScriptsDir)
	}
	os.MkdirAll(m.ScriptsDir, 0o755)
	return &cfg, nil
}

func (c *Config) allowed(ip string) bool {
	if ip == "127.0.0.1" || ip == "::1" {
		return true
	}
	for _, pattern := range c.Milan.AllowedIPs {
		if strings.Contains(pattern, "*") {
			re := `^` + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, `\d+`) + `$`
			if ok, _ := regexp.MatchString(re, ip); ok {
				return true
			}
		} else if pattern == ip {
			return true
		}
	}
	return false
}

// ─── Ruby discovery ───────────────────────────────────────────────────────────

var rubyBin = func() string {
	// If PATH ruby is already >= 3, use it as-is.
	if out, err := exec.Command("ruby", "--version").Output(); err == nil {
		s := string(out)
		if strings.Contains(s, "ruby 3") || strings.Contains(s, "ruby 4") {
			return "ruby"
		}
	}
	home, _ := os.UserHomeDir()
	rbenvRoot := os.Getenv("RBENV_ROOT")
	if rbenvRoot == "" {
		rbenvRoot = filepath.Join(home, ".rbenv")
	}
	candidates := []string{
		filepath.Join(rbenvRoot, "shims", "ruby"),
		filepath.Join(home, ".asdf", "shims", "ruby"),
		filepath.Join(home, ".local", "share", "mise", "shims", "ruby"),
		filepath.Join(home, ".mise", "shims", "ruby"),
		"/opt/homebrew/bin/ruby",
		"/usr/local/bin/ruby",
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		out, err := exec.Command(path, "--version").Output()
		if err != nil {
			continue
		}
		s := string(out)
		if strings.Contains(s, "ruby 3") || strings.Contains(s, "ruby 4") {
			return path
		}
	}
	return "ruby"
}()

// ─── Script helpers ───────────────────────────────────────────────────────────

var scriptNameRE = regexp.MustCompile(`\A[\w-]+\z`)

func buildCmd(scriptPath, argument string) []string {
	var args []string
	switch strings.ToLower(filepath.Ext(scriptPath)) {
	case ".rb":
		args = []string{rubyBin, scriptPath}
	case ".sh":
		args = []string{"sh", scriptPath}
	case ".py":
		args = []string{"python3", scriptPath}
	default:
		args = []string{scriptPath}
	}
	if argument != "" {
		args = append(args, argument)
	}
	return args
}

// ─── Server ───────────────────────────────────────────────────────────────────

type Server struct {
	config    *Config
	requests  atomic.Int64
	scripts   atomic.Int64
	startedAt time.Time
	jobsMu    sync.Mutex
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	ip := extractIP(r)
	if !s.config.allowed(ip) {
		s.logf("warn", "Blocked: %s -> %s", ip, r.URL.Path)
		http.Error(w, "Access denied", http.StatusForbidden)
		return
	}
	s.route(w, r, ip)
}

func extractIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, ip string) {
	p := r.URL.Path
	switch {
	case p == "/" || p == "/status":
		s.handleStatus(w)
	case p == "/health":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "OK")
	case p == "/list":
		writeJSON(w, map[string]any{"scripts": s.listScripts()})
	case p == "/jobs/pending":
		writeJSON(w, s.pendingJobs())
	case p == "/jobs" || p == "/jobs/all":
		writeJSON(w, s.allJobs())
	case strings.HasPrefix(p, "/jobs/ack/"):
		jobID, _ := url.PathUnescape(strings.TrimPrefix(p, "/jobs/ack/"))
		s.acknowledgeJob(jobID)
		fmt.Fprintf(w, "acknowledged: %s", jobID)
	case p == "/notes":
		writeJSON(w, s.listNoteSources())
	case strings.HasPrefix(p, "/notes/"):
		s.handleNotes(w, r, p)
	case strings.HasPrefix(p, "/stream/"):
		rest := strings.TrimPrefix(p, "/stream/")
		scriptName, arg := splitScriptPath(rest)
		s.streamScript(w, r, scriptName, arg, ip)
	default:
		rest := strings.TrimPrefix(p, "/")
		scriptName, arg := splitScriptPath(rest)
		if scriptName == "" {
			http.NotFound(w, r)
			return
		}
		s.executeScript(w, r, scriptName, arg, ip)
	}
}

func splitScriptPath(rest string) (script, arg string) {
	parts := strings.SplitN(rest, "/", 2)
	script = parts[0]
	if len(parts) > 1 {
		arg, _ = url.PathUnescape(parts[1])
	}
	return
}

func (s *Server) handleStatus(w http.ResponseWriter) {
	uptime := time.Since(s.startedAt).Round(time.Second)
	writeJSON(w, map[string]any{
		"service":           "milan",
		"version":           version,
		"uptime_seconds":    int(uptime.Seconds()),
		"requests":          s.requests.Load(),
		"scripts_run":       s.scripts.Load(),
		"available_scripts": s.listScripts(),
		"scripts_dir":       s.config.Milan.ScriptsDir,
	})
}

// ─── Script lookup ────────────────────────────────────────────────────────────

func (s *Server) findScript(name string) string {
	dir := s.config.Milan.ScriptsDir
	for _, sub := range scriptSubdirs {
		for _, ext := range scriptExtensions {
			var path string
			if sub == "" {
				path = filepath.Join(dir, name+ext)
			} else {
				path = filepath.Join(dir, sub, name+ext)
			}
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
	}
	return ""
}

func (s *Server) listScripts() []string {
	dir := s.config.Milan.ScriptsDir
	seen := map[string]bool{}
	for _, sub := range scriptSubdirs {
		var d string
		if sub == "" {
			d = dir
		} else {
			d = filepath.Join(dir, sub)
		}
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			for _, ext := range scriptExtensions {
				if strings.HasSuffix(name, ext) {
					seen[strings.TrimSuffix(name, ext)] = true
					break
				}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// ─── Sync execution ───────────────────────────────────────────────────────────

func (s *Server) executeScript(w http.ResponseWriter, r *http.Request, scriptName, argument, clientIP string) {
	if !scriptNameRE.MatchString(scriptName) {
		s.logf("warn", "Invalid script name: %s", scriptName)
		http.Error(w, "Invalid script name", http.StatusForbidden)
		return
	}
	scriptPath := s.findScript(scriptName)
	if scriptPath == "" {
		s.logf("warn", "Script not found: %s", scriptName)
		http.NotFound(w, r)
		return
	}
	s.logf("info", "%s -> %s(%s)", clientIP, scriptName, argument)

	start := time.Now()
	output, ok, timedOut := runScript(scriptPath, argument)
	dur := time.Since(start)
	s.scripts.Add(1)

	switch {
	case timedOut:
		s.logf("warn", "%s timed out", scriptName)
		http.Error(w, "Timeout (>5s)", http.StatusUnprocessableEntity)
	case ok:
		s.logf("info", "%s completed (%dms)", scriptName, dur.Milliseconds())
		if isHTMLOutput(output) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		fmt.Fprint(w, output)
	default:
		s.logf("warn", "%s failed: %s", scriptName, output)
		http.Error(w, output, http.StatusUnprocessableEntity)
	}
}

// Scripts may return full HTML pages (e.g. the markbinder album script);
// sniff the prefix so browsers render them instead of showing source.
func isHTMLOutput(out string) bool {
	t := strings.TrimSpace(out)
	return strings.HasPrefix(t, "<!DOCTYPE") || strings.HasPrefix(t, "<html")
}

func runScript(scriptPath, argument string) (output string, ok bool, timedOut bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	args := buildCmd(scriptPath, argument)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	// Forked Hintergrundprozesse erben stdout — ohne WaitDelay würde
	// CombinedOutput trotz Timeout auf Pipe-EOF warten.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	output = strings.TrimSpace(string(out))

	if ctx.Err() == context.DeadlineExceeded {
		return "Timeout (>5s)", false, true
	}
	return output, err == nil, false
}

// ─── SSE streaming ────────────────────────────────────────────────────────────

func (s *Server) streamScript(w http.ResponseWriter, r *http.Request, scriptName, argument, clientIP string) {
	if !scriptNameRE.MatchString(scriptName) {
		s.logf("warn", "Invalid script name: %s", scriptName)
		http.Error(w, "Invalid script name", http.StatusForbidden)
		return
	}
	scriptPath := s.findScript(scriptName)
	if scriptPath == "" {
		s.logf("warn", "Stream: script not found: %s", scriptName)
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	s.logf("info", "%s ~> %s(%s) [stream]", clientIP, scriptName, argument)
	s.scripts.Add(1)

	w.Header().Set("Content-Type", "text/event-stream; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	pr, pw, err := os.Pipe()
	if err != nil {
		s.logf("error", "Pipe error: %v", err)
		return
	}

	args := buildCmd(scriptPath, argument)
	// Bewusst kein kurzes Timeout — Streams dürfen als Hintergrund-Job
	// weiterlaufen. Die 1h-Obergrenze fängt nur echt hängende Skripte ab,
	// deren Goroutine sonst für immer in Wait() stecken bliebe.
	runCtx, cancelRun := context.WithTimeout(context.Background(), time.Hour)
	defer cancelRun()
	cmd := exec.CommandContext(runCtx, args[0], args[1:]...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		s.logf("error", "Start error: %v", err)
		return
	}
	pw.Close()

	ctx := r.Context()
	silent := false
	jobID := fmt.Sprintf("%s_%s", scriptName, time.Now().Format("20060102_150405"))
	jobLogPath := filepath.Join(jobsDir, jobID+".log")

	// Hintergrund-Ausgabe streamt direkt ins Job-Log auf Platte — ein
	// unbegrenzter In-Memory-Puffer wäre bei dauerschreibenden Skripten
	// ein OOM-Risiko.
	var jobLog *os.File
	goSilent := func() {
		if silent {
			return
		}
		silent = true
		os.MkdirAll(jobsDir, 0o755)
		f, err := os.Create(jobLogPath)
		if err != nil {
			s.logf("error", "%s job log: %v", scriptName, err)
		}
		jobLog = f
		s.logf("info", "%s → background (%s)", scriptName, jobID)
	}

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		// Detect client disconnect
		select {
		case <-ctx.Done():
			goSilent()
		default:
		}

		if silent {
			if jobLog != nil {
				fmt.Fprintln(jobLog, line)
			}
			continue
		}

		var writeErr error
		if strings.HasPrefix(line, "MILAN_PROMPT ") {
			payload := strings.TrimPrefix(line, "MILAN_PROMPT ")
			_, writeErr = fmt.Fprintf(w, "event: input_request\ndata: %s\n\n", payload)
		} else {
			_, writeErr = fmt.Fprintf(w, "data: %s\n\n", line)
		}
		if writeErr != nil {
			goSilent()
			if jobLog != nil {
				fmt.Fprintln(jobLog, line)
			}
			continue
		}
		flusher.Flush()
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		// z.B. Zeile > 1MB: Stream abbrechen, aber sauber melden statt
		// still als "done" zu enden. pr.Close() lässt das Skript per EPIPE sterben.
		s.logf("error", "%s stream read: %v", scriptName, scanErr)
	}
	pr.Close()
	cmd.Wait()

	exitOK := cmd.ProcessState != nil && cmd.ProcessState.Success()
	if silent {
		if jobLog != nil {
			jobLog.Close()
		}
		s.recordJob(jobID, scriptName, exitOK, jobLogPath)
		s.logf("info", "%s background %s → %s", scriptName, boolStr(exitOK, "ok", "failed"), jobLogPath)
	} else {
		if scanErr != nil {
			fmt.Fprintf(w, "event: stream_error\ndata: read error: %v\n\n", scanErr)
		} else if !exitOK && cmd.ProcessState != nil {
			fmt.Fprintf(w, "event: stream_error\ndata: exit %d\n\n", cmd.ProcessState.ExitCode())
		}
		fmt.Fprintf(w, "event: done\ndata: \n\n")
		flusher.Flush()
		s.logf("info", "%s stream completed", scriptName)
	}
}

// ─── Job tracking ─────────────────────────────────────────────────────────────

type Job struct {
	ID           string `json:"id"`
	Script       string `json:"script"`
	ExitOK       bool   `json:"exit_ok"`
	Log          string `json:"log"`
	TS           string `json:"ts"`
	Acknowledged bool   `json:"acknowledged"`
}

func (s *Server) recordJob(jobID, scriptName string, exitOK bool, logFilePath string) {
	statusFile := filepath.Join(jobsDir, "status.json")
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	var jobs []Job
	if data, err := os.ReadFile(statusFile); err == nil {
		json.Unmarshal(data, &jobs)
	}
	jobs = append(jobs, Job{
		ID:     jobID,
		Script: scriptName,
		ExitOK: exitOK,
		Log:    logFilePath,
		TS:     time.Now().Format(time.RFC3339),
	})
	if len(jobs) > 100 {
		jobs = jobs[len(jobs)-100:]
	}
	data, _ := json.Marshal(jobs)
	atomicWrite(statusFile, data)
}

func (s *Server) allJobs() []Job {
	statusFile := filepath.Join(jobsDir, "status.json")
	data, err := os.ReadFile(statusFile)
	if err != nil {
		return []Job{}
	}
	var jobs []Job
	if json.Unmarshal(data, &jobs) != nil {
		return []Job{}
	}
	return jobs
}

func (s *Server) pendingJobs() []Job {
	result := []Job{}
	for _, j := range s.allJobs() {
		if !j.Acknowledged {
			result = append(result, j)
		}
	}
	return result
}

func (s *Server) acknowledgeJob(jobID string) {
	statusFile := filepath.Join(jobsDir, "status.json")
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	data, err := os.ReadFile(statusFile)
	if err != nil {
		return
	}
	var jobs []Job
	json.Unmarshal(data, &jobs)
	for i := range jobs {
		if jobs[i].ID == jobID {
			jobs[i].Acknowledged = true
		}
	}
	updated, _ := json.Marshal(jobs)
	atomicWrite(statusFile, updated)
}

func atomicWrite(path string, data []byte) {
	tmp := path + ".tmp"
	os.WriteFile(tmp, data, 0o644)
	os.Rename(tmp, path)
}

// ─── Notes ────────────────────────────────────────────────────────────────────

func (s *Server) noteSources() map[string]string {
	m := map[string]string{}
	for _, ns := range s.config.Milan.Notes {
		m[ns.ID] = ns.Path
	}
	return m
}

func (s *Server) listNoteSources() []map[string]string {
	var result []map[string]string
	for _, ns := range s.config.Milan.Notes {
		result = append(result, map[string]string{"id": ns.ID, "path": ns.Path})
	}
	return result
}

func (s *Server) handleNotes(w http.ResponseWriter, r *http.Request, p string) {
	trimmed := strings.TrimPrefix(p, "/notes/")
	parts := strings.SplitN(trimmed, "/", 3)
	sourceID, _ := url.PathUnescape(parts[0])
	sources := s.noteSources()
	dir, exists := sources[sourceID]

	// /notes/<source>  — list notes
	if len(parts) == 1 || (len(parts) == 2 && parts[1] == "") {
		if !exists {
			http.Error(w, "Source not found", http.StatusNotFound)
			return
		}
		writeJSON(w, s.listNotes(dir))
		return
	}

	// /notes/<source>/assets/<path>
	if parts[1] == "assets" && len(parts) == 3 {
		assetPath, _ := url.PathUnescape(parts[2])
		s.serveNoteAsset(w, dir, assetPath, exists)
		return
	}

	// /notes/<source>/<file>
	filename, _ := url.PathUnescape(parts[1])
	s.serveNote(w, dir, filename, exists)
}

func (s *Server) listNotes(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var files []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".md", ".html":
			files = append(files, e.Name())
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i]) < strings.ToLower(files[j])
	})
	return files
}

var (
	safeFilenameRE = regexp.MustCompile(`\A[\w. -]+\z`)
	safeAssetRE    = regexp.MustCompile(`\A(images|css)/[\w. -]+\z`)
)

func (s *Server) serveNote(w http.ResponseWriter, dir, filename string, sourceExists bool) {
	if !sourceExists {
		http.Error(w, "Source not found", http.StatusNotFound)
		return
	}
	name := filepath.Base(filename)
	if !safeFilenameRE.MatchString(name) {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}
	path := filepath.Join(dir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "Note not found", http.StatusNotFound)
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	var html string
	switch ext {
	case ".md":
		md := string(raw)
		if idx := strings.Index(md, "%%%END"); idx >= 0 {
			md = strings.TrimSpace(md[idx+6:])
		}
		cmd := exec.Command("apex", "--mode", "gfm")
		cmd.Stdin = strings.NewReader(md)
		out, err := cmd.Output()
		if err != nil {
			http.Error(w, "apex not found — gem install apex", http.StatusServiceUnavailable)
			return
		}
		html = string(out)
	case ".html":
		html = string(raw)
	default:
		http.Error(w, "Unsupported format", http.StatusUnsupportedMediaType)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, html)
}

func (s *Server) serveNoteAsset(w http.ResponseWriter, dir, assetPath string, sourceExists bool) {
	if !sourceExists {
		http.Error(w, "Source not found", http.StatusNotFound)
		return
	}
	if !safeAssetRE.MatchString(assetPath) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, assetPath))
	if err != nil {
		http.Error(w, "Asset not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", mimeForExt(filepath.Ext(assetPath)))
	w.Write(data)
}

func mimeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".webp":
		return "image/webp"
	case ".css":
		return "text/css"
	default:
		return "application/octet-stream"
	}
}

// ─── Cron ─────────────────────────────────────────────────────────────────────

func (s *Server) startCron() {
	var cronScript string
	for _, ext := range scriptExtensions {
		path := filepath.Join(s.config.Milan.ScriptsDir, "cron-runner"+ext)
		if _, err := os.Stat(path); err == nil {
			cronScript = path
			break
		}
	}
	if cronScript == "" {
		return
	}
	interval := time.Duration(s.config.Milan.CronInterval) * time.Second
	s.logf("info", "Cron: every %v via %s", interval, filepath.Base(cronScript))
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			args := buildCmd(cronScript, "")
			// Timeout = Intervall: ein hängender Runner blockiert sonst
			// alle folgenden Ticks für immer.
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			cmd := exec.CommandContext(ctx, args[0], args[1:]...)
			cmd.WaitDelay = 5 * time.Second
			if err := cmd.Run(); ctx.Err() == context.DeadlineExceeded {
				s.logf("warn", "cron run timed out after %v", interval)
			} else if err != nil {
				s.logf("warn", "cron run failed: %v", err)
			}
			cancel()
		}
	}()
}

// ─── Logging ─────────────────────────────────────────────────────────────────

func (s *Server) logf(level, format string, args ...any) {
	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("[%s] %-5s %s\n", ts, strings.ToUpper(level), msg)
}

// ─── HTTP helper ─────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// ─── serve (internal) ────────────────────────────────────────────────────────

func serve() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Fatal:", err)
		os.Exit(1)
	}

	srv := &Server{config: cfg, startedAt: time.Now()}

	fmt.Printf("\033[36m\n")
	fmt.Printf("╔═══════════════════════════════════════╗\n")
	fmt.Printf("║          Milan v%-22s║\n", version)
	fmt.Printf("║    Script Executor for macOS          ║\n")
	fmt.Printf("╚═══════════════════════════════════════╝\n")
	fmt.Printf("\033[0m\n")
	fmt.Printf("Port:        %d\n", cfg.Milan.Port)
	fmt.Printf("Scripts:     %s\n", cfg.Milan.ScriptsDir)
	fmt.Printf("Allowed IPs: %s\n", strings.Join(cfg.Milan.AllowedIPs, ", "))
	fmt.Printf("Ruby:        %s\n", rubyBin)
	fmt.Println(strings.Repeat("─", 40))

	srv.startCron()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		fmt.Println("\nMilan stopped.")
		os.Exit(0)
	}()

	addr := fmt.Sprintf(":%d", cfg.Milan.Port)
	if err := http.ListenAndServe(addr, srv); err != nil {
		fmt.Fprintln(os.Stderr, "Server error:", err)
		os.Exit(1)
	}
}

// ─── Control commands ────────────────────────────────────────────────────────

func isRunning() (int, bool) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return 0, false
	}
	return pid, true
}

func portInUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

var dylanURL = func() string {
	if u := os.Getenv("DYLAN_URL"); u != "" {
		return u
	}
	return "http://dy.lan/whoami"
}()

func checkIdentity() (string, bool) {
	if dylanURL == "" {
		return "standalone", true
	}
	fmt.Print("Checking identity with Dylan... ")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(dylanURL)
	if err != nil {
		fmt.Printf("FAILED (%v)\n", err)
		return "", false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	identity := strings.TrimSpace(string(body))
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("REJECTED (HTTP %d)\nDylan says: %s\n", resp.StatusCode, identity)
		return "", false
	}
	fields := strings.Fields(identity)
	if len(fields) == 0 {
		fmt.Println("FAILED (empty response from Dylan)")
		return "", false
	}
	fmt.Printf("OK - I am %s\n", identity)
	return fields[0], true
}

func start(standalone bool) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	port := cfg.Milan.Port

	if pid, ok := isRunning(); ok {
		fmt.Printf("Milan already running (PID %d)\n", pid)
		return
	}
	if portInUse(port) {
		fmt.Printf("Port %d in use — cannot start\n", port)
		return
	}

	var identity string
	if standalone {
		identity = "standalone"
	} else {
		var ok bool
		if identity, ok = checkIdentity(); !ok {
			return
		}
	}

	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)

	// Einfache Rotation: ab 10MB zur Seite legen (eine Generation genügt)
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 10<<20 {
		os.Rename(logPath, logPath+".old")
	}

	logF, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Println("Cannot open log file:", err)
		return
	}
	defer logF.Close()

	cmd := exec.Command(exe, "serve")
	cmd.Dir = base
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.Env = append(os.Environ(), "MILAN_IDENTITY="+identity)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		fmt.Println("Failed to start:", err)
		return
	}

	pid := cmd.Process.Pid
	os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644)

	// Poll /health
	client := &http.Client{Timeout: time.Second}
	healthURL := fmt.Sprintf("http://localhost:%d/health", port)
	for i := 0; i < 8; i++ {
		time.Sleep(500 * time.Millisecond)
		if proc, err := os.FindProcess(pid); err == nil {
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				fmt.Printf("Failed to start Milan — process exited early, check %s\n", logPath)
				os.Remove(pidFile)
				return
			}
		}
		if resp, err := client.Get(healthURL); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				fmt.Printf("Milan started (PID %d)\n", pid)
				return
			}
		}
	}

	if _, ok := isRunning(); ok {
		fmt.Printf("Milan started (PID %d) — port not yet responding\n", pid)
	} else {
		fmt.Printf("Failed to start Milan — check %s\n", logPath)
		os.Remove(pidFile)
	}
}

func stop() {
	pid, ok := isRunning()
	if !ok {
		fmt.Println("Milan not running")
		os.Remove(pidFile)
		return
	}
	fmt.Printf("Stopping Milan (PID %d)...\n", pid)
	proc, _ := os.FindProcess(pid)
	proc.Signal(syscall.SIGTERM)
	stopped := false
	for i := 0; i < 5; i++ {
		time.Sleep(time.Second)
		if _, ok := isRunning(); !ok {
			stopped = true
			break
		}
	}
	if !stopped {
		proc.Signal(syscall.SIGKILL)
	}
	os.Remove(pidFile)
	fmt.Println("Stopped.")
}

func status() {
	pid, ok := isRunning()
	if !ok {
		fmt.Println("Milan not running")
		return
	}
	fmt.Printf("Milan running (PID %d)\n", pid)

	cfg, err := loadConfig()
	if err != nil {
		return
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/", cfg.Milan.Port))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var data map[string]any
	if json.NewDecoder(resp.Body).Decode(&data) != nil {
		return
	}
	fmt.Printf("Uptime: %vs | Requests: %v | Scripts: %v\n",
		data["uptime_seconds"], data["requests"], data["scripts_run"])
}

// ─── Utility ─────────────────────────────────────────────────────────────────

func boolStr(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

// ─── Main ────────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s {start|stop|restart|status|log|whoami} [--standalone]\n", os.Args[0])
		os.Exit(1)
	}

	cmd := os.Args[1]
	standalone := len(os.Args) > 2 && os.Args[2] == "--standalone"

	switch cmd {
	case "start":
		start(standalone)
	case "stop":
		stop()
	case "restart":
		stop()
		start(standalone)
	case "status":
		status()
	case "log":
		exec.Command("tail", "-f", logPath).Run()
	case "whoami":
		if _, ok := checkIdentity(); !ok {
			os.Exit(1)
		}
	case "serve":
		serve()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		os.Exit(1)
	}
}
