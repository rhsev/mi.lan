// Command livesync is milan's health check for the obsidian-livesync daemon,
// ported from scripts/custom/livesync.rb.
//
// Why compiled: the check runs every 5 minutes, and 106 of the Ruby version's
// 185 ms were the interpreter starting up before a single line of the script
// ran. That is the one cost a compiled binary removes outright (measured
// 2026-07-30). Everything else about the check is unchanged, deliberately: the
// Ruby version stays next to it as the reference, and both must print the same
// diagnosis for the same machine state.
//
// The check is process-based rather than `launchctl list`, because the daemon
// runs as a system LaunchDaemon (/Library/LaunchDaemons, UserName=extern) and
// `launchctl list` in a user domain does not see it — it reported "not loaded"
// while the daemon was fine. Reading the process table sees it regardless of
// domain and works the same for a user agent on the MacBook.
//
// It also does not just check "process alive" — that produced false greens
// while the watcher silently degraded. It detects crash/crash-loop, a listener
// leak (a degraded watcher), a locked remote database, and high uptime.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	label = "net.vrtmrz.livesync-cli"
	// match is the daemon's process signature: node … dist/index.cjs <vault>
	match        = "dist/index.cjs"
	uptimeWarnH  = 36
	tailLines    = 80
	tailBytes    = 256 * 1024
	lstartLayout = "Mon Jan 2 15:04:05 2006"
	psTimeFields = 5 // lstart spans "Wed Jul 29 17:40:25 2026"
)

var (
	crashRe = regexp.MustCompile(`Node\.js v\d|TypeError|OpenError|cannot be initialised|Failed to initialize|cannot continue`)
	lockRe  = regexp.MustCompile(`Remote database is locked|Unlock the remote database|Reset Synchronisation on This Device`)
)

func errLogs() []string {
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, "Library/Logs/livesync-cli.err"), "/tmp/livesync-cli.err"}
}

func stamp() string { return time.Now().Format("15:04:05") }

func main() {
	fmt.Println("## LiveSync Agent")
	fmt.Println()

	pid, start, found := findDaemon()
	if !found {
		fmt.Printf("🔴 **Crash / kein Prozess** — Daemon läuft nicht. Fix: `sudo launchctl kickstart -k system/%s`\n", label)
		fmt.Printf("\n[%s] Check fertig.\n", stamp())
		return
	}

	upStr := "?"
	var uptime time.Duration
	haveUptime := !start.IsZero()
	if haveUptime {
		uptime = time.Since(start)
		upStr = fmt.Sprintf("%dh%dm", int(uptime.Hours()), int(uptime.Minutes())%60)
	}

	log, tail := readTail()

	// A marker only counts when it appears *after* the last "LiveSync active" —
	// otherwise the daemon recovered (a cold-start race, or a restart just now).
	crashIdx := lastIndexFunc(tail, crashRe.MatchString)
	lockIdx := lastIndexFunc(tail, lockRe.MatchString)
	activeIdx := lastIndexFunc(tail, func(l string) bool { return strings.Contains(l, "LiveSync active") })

	locked := lockIdx >= 0 && (activeIdx < 0 || lockIdx > activeIdx)
	// A crash only counts when there is no lock: the lock's "cannot continue" is
	// not a real crash.
	crash := ""
	if !locked && crashIdx >= 0 && (activeIdx < 0 || crashIdx > activeIdx) {
		crash = tail[crashIdx]
	}
	leak := lastIndexFunc(tail, func(l string) bool {
		return strings.Contains(l, "MaxListenersExceededWarning")
	}) >= 0
	active := activeIdx >= 0

	var issues []issue
	if locked {
		issues = append(issues, issue{"lock", "Remote-DB gesperrt (LOCK) — Unlock nötig (z. B. nach iPhone-Rebuild/Doctor). Headless löst das NICHT selbst, Neustart hilft NICHT → server-seitig `_local/obsydian_livesync_milestone` locked:false."})
	}
	if crash != "" {
		issues = append(issues, issue{"red", fmt.Sprintf("Crash im Log: `%s`", crash)})
	}
	if haveUptime && uptime > uptimeWarnH*time.Hour {
		issues = append(issues, issue{"orange", fmt.Sprintf("Uptime %s (>%dh) — Watcher-Degradation möglich; Auto-Neustart prüfen", upStr, uptimeWarnH)})
	}

	// The leak no longer colours the status (2026-07-30). Over the whole log:
	// 46 daemon instances, 8 of them warned, and every one of those warned
	// *exactly twice* — never once, never three times. Node emits
	// MaxListenersExceededWarning once per EventTarget when the 11th listener
	// is added and never again, so this is a startup artefact, not a trend. It
	// cannot escalate, and dylan's monitor.sh never acted on it anyway (it
	// restarts on 🔴 or uptime>=24h) — it just pinned /monitor at 🟠 for the
	// instance's whole life. Kept as a detail line.
	var infos []string
	if leak {
		infos = append(infos, "`MaxListenersExceededWarning` beim Start dieser Instanz — Startartefakt (feuert 2× oder nie), kein Verlaufssignal; gegen echte Watcher-Degradation greift der 24h-Neustart")
	}

	emoji := map[string]string{"lock": "🔒", "red": "🔴", "orange": "🟠"}
	status := "🟢"
	switch {
	case hasSev(issues, "lock"):
		status = "🔒"
	case hasSev(issues, "red"):
		status = "🔴"
	case len(issues) > 0:
		status = "🟠"
	}

	if status == "🟢" {
		activeMark := "—"
		if active {
			activeMark = "✅"
		}
		fmt.Printf("🟢 **gesund** — pid %s, Uptime %s, `LiveSync active`: %s\n", pid, upStr, activeMark)
	} else {
		fmt.Printf("%s **Achtung** — pid %s, Uptime %s\n", status, pid, upStr)
		for _, is := range issues {
			fmt.Printf("- %s %s\n", emoji[is.sev], is.msg)
		}
	}

	for _, msg := range infos {
		fmt.Printf("- ℹ️ %s\n", msg)
	}
	if log == "" {
		fmt.Println("- ℹ️ kein Logfile am Pfad (Prozess läuft) — Neustart legt es neu an.")
	}

	fmt.Println()
	fmt.Printf("[%s] Check fertig (echte Liveness: Datei im Vault anlegen → muss in Sek. in CouchDB sein).\n", stamp())
}

type issue struct{ sev, msg string }

func hasSev(issues []issue, sev string) bool {
	for _, is := range issues {
		if is.sev == sev {
			return true
		}
	}
	return false
}

// findDaemon reads the process table once for pid, start time and command line.
//
// Deliberately not `pgrep -f dist/index.cjs`: that matches *any* process whose
// command line contains the string — an editor, a grep, a shell — and taking
// the first match takes the lowest pid, not the daemon. Observed 2026-07-30:
// the Ruby version reported pid 6767 (a shell) while the daemon ran as 29024.
// So the executable must actually be named `node`.
func findDaemon() (pid string, start time.Time, found bool) {
	out, err := exec.Command("ps", "-Ao", "pid=,lstart=,args=").Output()
	if err != nil {
		return "", time.Time{}, false
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // command lines can be long
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.Contains(line, match) {
			continue
		}
		f := strings.Fields(line)
		if len(f) <= 1+psTimeFields {
			continue
		}
		if filepath.Base(f[1+psTimeFields]) != "node" {
			continue
		}
		t, err := time.ParseInLocation(lstartLayout, strings.Join(f[1:1+psTimeFields], " "), time.Local)
		if err != nil {
			t = time.Time{}
		}
		return f[0], t, true
	}
	return "", time.Time{}, false
}

// readTail returns the first existing error log and its last tailLines lines.
//
// Only the end of the file is read. The Ruby version used to File.read the
// whole thing (14.4 MB on 2026-07-30) and split it into a full line array to
// keep 80 lines — about 54 ms, growing with the log. Fixed there too.
func readTail() (string, []string) {
	for _, path := range errLogs() {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			return path, nil
		}
		size := info.Size()
		from := size - tailBytes
		truncated := from > 0
		if !truncated {
			from = 0
		}
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return path, nil
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return path, nil
		}

		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		// the first line is cut mid-way when we seeked into the file
		if truncated && len(lines) > 1 {
			lines = lines[1:]
		}
		if len(lines) > tailLines {
			lines = lines[len(lines)-tailLines:]
		}
		return path, lines
	}
	return "", nil
}

func lastIndexFunc(lines []string, pred func(string) bool) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if pred(lines[i]) {
			return i
		}
	}
	return -1
}
