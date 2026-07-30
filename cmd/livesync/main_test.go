package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These are the twelve machine states the Go check was diffed against its Ruby
// original on 2026-07-30, before it replaced it. That comparison lived in a
// throwaway harness under /tmp; this is the durable version. Every rule below
// was hand-tuned against real logs, so it needs pinning: the failure mode is
// not a crash, it is a health check that quietly says the wrong thing.

const nodeLine = "29024 Wed Jul 29 17:40:25 2026 /opt/homebrew/bin/node /x/dist/index.cjs /vault"

// A shell whose command line merely *contains* the signature. `pgrep -f` used
// to match this and take the lowest pid — on 2026-07-30 the check reported a
// shell's pid while the daemon ran elsewhere. With the daemon dead and any such
// process around, it would have reported "running": the exact false green this
// check exists to prevent.
const decoyLine = "777 Wed Jul 29 17:40:25 2026 sh -c sleep 15 dist/index.cjs"

func TestPickDaemon(t *testing.T) {
	cases := []struct {
		name    string
		ps      string
		wantPID string
	}{
		{"kein Prozess", "", ""},
		{"nur Fremdprozesse", "1 Wed Jul 29 17:40:25 2026 /bin/zsh\n", ""},
		{"Daemon", nodeLine + "\n", "29024"},
		{"Decoy allein wird verworfen", decoyLine + "\n", ""},
		{"Decoy vor Daemon", decoyLine + "\n" + nodeLine + "\n", "29024"},
		{"Signatur ohne node", "5 Wed Jul 29 17:40:25 2026 /usr/bin/vim /x/dist/index.cjs\n", ""},
	}
	for _, c := range cases {
		pid, start, found := pickDaemon(c.ps)
		if c.wantPID == "" {
			if found {
				t.Errorf("%s: fand pid %q, erwartet: keinen Daemon", c.name, pid)
			}
			continue
		}
		if !found || pid != c.wantPID {
			t.Errorf("%s: pid = %q (found=%v), erwartet %q", c.name, pid, found, c.wantPID)
		}
		if start.IsZero() {
			t.Errorf("%s: Startzeit nicht geparst", c.name)
		} else if got := start.Format("2006-01-02 15:04"); got != "2026-07-29 17:40" {
			t.Errorf("%s: Startzeit = %s", c.name, got)
		}
	}
}

func TestDecide(t *testing.T) {
	const (
		active = "[Daemon] LiveSync active"
		crash  = "TypeError: boom"
		lock   = "Remote database is locked"
		leak   = "(node:1) MaxListenersExceededWarning: 11 listeners"
		young  = 8 * time.Hour
		old    = 40 * time.Hour
	)

	cases := []struct {
		name       string
		tail       []string
		uptime     time.Duration
		wantStatus string
		wantInfos  int
		wantActive bool
	}{
		{"gesund mit active", []string{active}, young, "🟢", 0, true},
		{"gesund ohne active", []string{"nur geplauder"}, young, "🟢", 0, false},
		{"leeres Log", nil, young, "🟢", 0, false},

		// Der Leak färbt den Status nicht (mehr) — er ist ein Startartefakt.
		{"Leak bleibt grün", []string{active, leak}, young, "🟢", 1, true},

		{"Crash nach active", []string{active, crash}, young, "🔴", 0, true},
		{"Crash vor active = erholt", []string{crash, active}, young, "🟢", 0, true},
		{"Crash ohne active", []string{crash}, young, "🔴", 0, false},

		{"Lock nach active", []string{active, lock}, young, "🔒", 0, true},
		{"Lock vor active = erholt", []string{lock, active}, young, "🟢", 0, true},
		// Der lock-bedingte "cannot continue" ist kein echter Crash.
		{"Lock schlägt Crash", []string{active, "cannot continue", lock}, young, "🔒", 0, true},

		{"Uptime über Schwelle", []string{active}, old, "🟠", 0, true},
		{"Uptime und Leak", []string{active, leak}, old, "🟠", 1, true},
		{"Uptime schlägt nicht Crash", []string{active, crash}, old, "🔴", 0, true},
	}

	for _, c := range cases {
		v := decide(c.tail, c.uptime, true, "UP")
		if v.status != c.wantStatus {
			t.Errorf("%s: status = %s, erwartet %s (issues=%v)", c.name, v.status, c.wantStatus, v.issues)
		}
		if len(v.infos) != c.wantInfos {
			t.Errorf("%s: %d Infozeilen, erwartet %d", c.name, len(v.infos), c.wantInfos)
		}
		if v.active != c.wantActive {
			t.Errorf("%s: active = %v, erwartet %v", c.name, v.active, c.wantActive)
		}
	}

	// Ohne bekannte Uptime darf die Schwelle nie greifen.
	if v := decide([]string{active}, old, false, "?"); v.status != "🟢" {
		t.Errorf("unbekannte Uptime: status = %s, erwartet 🟢", v.status)
	}
}

func TestReadTailFrom(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nicht-da.err")

	if path, tail := readTailFrom([]string{missing}); path != "" || tail != nil {
		t.Errorf("fehlende Datei: path=%q tail=%v, erwartet leer", path, tail)
	}

	// Erste existierende Datei gewinnt.
	small := filepath.Join(dir, "small.err")
	if err := os.WriteFile(small, []byte("eins\nzwei\ndrei\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, tail := readTailFrom([]string{missing, small})
	if path != small {
		t.Errorf("path = %q, erwartet %q", path, small)
	}
	if len(tail) != 3 || tail[2] != "drei" {
		t.Errorf("tail = %v, erwartet drei Zeilen endend auf \"drei\"", tail)
	}

	// Nur die letzten tailLines Zeilen.
	many := filepath.Join(dir, "many.err")
	var b strings.Builder
	for i := 0; i < tailLines*3; i++ {
		b.WriteString("zeile\n")
	}
	if err := os.WriteFile(many, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, tail := readTailFrom([]string{many}); len(tail) != tailLines {
		t.Errorf("%d Zeilen, erwartet %d", len(tail), tailLines)
	}

	// Über tailBytes hinaus wird vom Ende gelesen und die angeschnittene erste
	// Zeile verworfen — das ist der Fix gegen das Einlesen der ganzen 14-MB-Datei.
	big := filepath.Join(dir, "big.err")
	var c strings.Builder
	c.WriteString(strings.Repeat("A", tailBytes)) // eine überlange erste Zeile
	c.WriteString("\nletzte-zeile\n")
	if err := os.WriteFile(big, []byte(c.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	_, tail = readTailFrom([]string{big})
	if len(tail) != 1 || tail[0] != "letzte-zeile" {
		t.Errorf("tail = %v, erwartet nur [letzte-zeile] (angeschnittene Zeile verworfen)", tail)
	}
}
