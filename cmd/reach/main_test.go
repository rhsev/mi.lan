package main

import "testing"

func TestHasMAC(t *testing.T) {
	cases := map[string]bool{
		// Real macOS arp output, the case the Fedora VM produced.
		"? (192.168.1.73) at 1a:2b:3c:4d:5e:6f on en0 ifscope [ethernet]": true,
		// Single-digit groups occur; the Ruby regex allowed one or two hex digits.
		"? (192.168.1.73) at 1:2:3:4:5:6 on en0 ifscope [ethernet]": true,
		// No entry: what arp prints when the cache is empty for that host.
		"arp: 192.168.1.250: No entry": false,
		"":                             false,
		// "at" without a full address must not count.
		"? (192.168.1.73) at incomplete on en0": false,
		// Five groups instead of six.
		"? (10.0.0.1) at 1a:2b:3c:4d:5e on en0": false,
	}
	for in, want := range cases {
		if got := hasMAC(in); got != want {
			t.Errorf("hasMAC(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIPGuard(t *testing.T) {
	// The guard is deliberately the Ruby original's: shape, not validity.
	cases := map[string]bool{
		"192.168.1.33":   true,
		"1.2.3.4":        true,
		"999.1.1.1":      true, // passes the guard, ping rejects it — as before
		"192.168.1":      false,
		"192.168.1.33.4": false,
		"host.local":     false,
		"":               false,
		" 192.168.1.33":  false,
	}
	for in, want := range cases {
		if got := ipRe.MatchString(in); got != want {
			t.Errorf("ipRe(%q) = %v, want %v", in, got, want)
		}
	}
}
