// Command reach is milan's LAN liveness probe, ported from
// scripts/custom/reach.rb. milan serves it as /reach/<ip>.
//
// Two stages: ping, and if that stays silent, ARP — devices behind strict
// firewalls still give themselves away on layer 2 (Fedora VM, 2026-08-27:
// firewalld drops everything but Tailscale on the LAN interface, yet the MAC
// answers). The ping attempt is also what populates the ARP cache, so the
// order is not interchangeable.
//
// Why on the Mini rather than in the dylan container: dylan hangs off the same
// NIC as the NAS's VMM bridge as a macvlan child and cannot reach its VMs by
// construction (hairpin). The Mini, as an external host, can.
//
// Why compiled: /reach is by far milan's busiest endpoint — 970 calls in the
// last 4000 log lines, because monitor.sh asks for every host on its watch list
// every five minutes. The port was only worth doing AFTER the probe itself got
// cheap, and that sequence is the lesson: with ping's default one-second
// spacing the Ruby interpreter's 115 ms start was 10% of 1153 ms, which is
// noise. With -i 0.2 the run is ~350 ms and the same 115 ms is a third of it
// (measured 2026-09-27). Measure the work before switching the language.
//
// Behaviour is unchanged from the Ruby original, including its loose address
// guard: four groups of one to three digits, so "999.1.1.1" passes here and is
// rejected by ping instead. Tightening it to net.ParseIP would be a different
// program, and a port is not the place for that.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
)

// Absolute paths: milan's launchd environment carries a sparse PATH, and a
// probe that silently fails to find its own tools would report every host as
// down. The Ruby original relied on PATH and got away with it.
const (
	pingBin = "/sbin/ping"
	arpBin  = "/usr/sbin/arp"
)

// -W is in MILLISECONDS on macOS, not seconds — a Linux habit that sat in the
// Ruby version as "-W 1" (one millisecond) for months. It bounds the wait after
// the last probe is sent, so the interval is what really decides how long a
// reply may take. See the measurements in reach.rb: -i 0.2 alone narrowed that
// window from ~1000 ms to ~200 ms, which would call a slow device down.
var pingArgs = []string{"-c", "2", "-i", "0.2", "-W", "1000", "-q"}

var (
	ipRe  = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
	macRe = regexp.MustCompile(`(?i)at ([0-9a-f]{1,2}:){5}[0-9a-f]{1,2}`)
)

// hasMAC reports whether arp's output names a hardware address for the host.
// Split out so the parsing is testable without a network.
func hasMAC(arpOutput string) bool { return macRe.MatchString(arpOutput) }

func pingOK(ip string) bool {
	return exec.Command(pingBin, append(pingArgs, ip)...).Run() == nil
}

func arpOK(ip string) bool {
	// Errors are not distinguished from "no entry": arp exits non-zero for an
	// unknown host, and that is the same answer as empty output.
	out, _ := exec.Command(arpBin, "-n", ip).Output()
	return hasMAC(string(out))
}

func main() {
	if len(os.Args) < 2 || !ipRe.MatchString(os.Args[1]) {
		fmt.Fprintln(os.Stderr, "usage: reach IP")
		os.Exit(1)
	}
	ip := os.Args[1]

	state := "aus"
	if pingOK(ip) || arpOK(ip) {
		state = "ok"
	}
	fmt.Printf("%s %s\n", state, ip)
}
