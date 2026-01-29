# mi.lan (Milan)

**A lightweight URL bridge for macOS automation.**

Milan is a small Ruby agent (~300 lines) that executes local scripts and Apple Shortcuts via HTTP. It is designed as a companion for [dy.lan](https://github.com/rhsev/dy.lan): Dylan routes the logic, Milan executes on the Mac. Or Milan just works alone on the Mac.

### Status
Release: February 1st, 2026. Currently testing.

### Key Features
- Remote script execution in ~120ms (vs. ~1s for native Shortcut CLI).
- Reach your Mac via Dylan from any network (LAN/VPN).
- Identity verification with the Dylan master at startup.
- Strict IP allow-listing and no external dependencies.

### Example
`GET http://mi.lan/mac/sync_notes` -> triggers `./scripts/sync_notes.rb` on your Mac.
