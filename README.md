# mi.lan (Milan)

[![test](https://github.com/rhsev/mi.lan/actions/workflows/test.yml/badge.svg)](https://github.com/rhsev/mi.lan/actions/workflows/test.yml)

A lightweight URL bridge for local automation.

Milan is an HTTP agent that runs local scripts, and on macOS also Apple Shortcuts, when a URL is called. It keeps running in the background, so any HTTP-capable source (browser, curl, Stream Deck, other scripts) can trigger local automation. It also works as a webhook receiver: signed POST bodies reach scripts on stdin.

It can do both:

* Standalone: It works as a standalone tool on a Mac or on a Linux server, where it makes a small, self-contained webhook target.
* Companion: Together with [dy.lan](https://github.com/rhsev/dy.lan) it runs scripts on your Mac for requests from any device on your local (or Tailscale) network.

## Why Milan?

* URL triggers: Every script in the scripts folder is an HTTP endpoint, without configuration.
* Speed: Milan keeps running, so a script answers in about 120 ms (a Shortcut in about 1 s).
* Simplicity: Single Go binary, no runtime dependencies.
* Privacy: Only allowlisted addresses get in, and no cloud service is involved.
* Reach: Through Dylan, your Mac is reachable from the LAN or a VPN.
* Identity: At startup, Milan confirms its name with Dylan.

## How Milan works with Dylan

* dy.lan (the router) runs in Docker, for example on a Synology. It reads the URL, decides which agent the request is for, and passes it on.
* mi.lan (the agent) runs on the Mac and executes the script or Apple Shortcut the request asks for.

### The Workflow

1. Request: A client (like your iPhone) sends a request to the redirector (e.g., `http://mi.lan/mini/shortcut/Note`).
2. Handshake: Before starting, the agent tells the redirector its name via `http://dy.lan/whoami?name=mini`, and the redirector confirms it against its agent list (see [Identity check](#identity-check)).
3. Redirection: The hub recognizes the target agent ("mini") and passes the request to the specific Mac's IP (e.g., `192.168.1.118:8080`).
4. Execution: The agent performs the local action and sends the result back.

```
iPhone -> Dylan (Synology) -> Milan (Mac) -> Script -> Response

```

## Requirements

* macOS (tested on Sequoia) or Linux. The binaries are static, any distro
  works; `scripts/milan.service` is a systemd unit to start from
* Ruby 3+ (to run `.rb` scripts)
* Go 1.26, only if you build from source

The dispatcher itself is platform-neutral: the test suite runs on Linux in
CI, and a milan has been serving on an Ubuntu VPS since 2.3.0. What is
macOS-only is the automation *around* it: Apple Shortcuts
(`scripts/shortcut.rb`), the launchd awareness of the control commands, the
urgent widgets passing through [ticker](https://github.com/rhsev/ticker),
the `milan://` URL scheme. Everything else behaves the same on both:
scripts, streams, the widget inbox, background jobs, `bind:`, `secrets:`.

## Directory layout

Milan resolves all paths relative to its own binary:

```
milan-dir/
├── milan              # binary
├── config.yaml        # your config (copy from config.yaml.example)
├── scripts/           # scripts served as HTTP endpoints
│   └── custom/        # private scripts (gitignored)
├── data/              # background job logs (auto-created)
└── milan.log          # runtime log
```

Keep the binary and `config.yaml` in the same directory. To call `milan` from
anywhere, create a symlink. Milan uses `filepath.EvalSymlinks` internally and
resolves the real binary location correctly:

```bash
ln -sf /path/to/milan-dir/milan /usr/local/bin/milan
```

Do not move the binary alone without the config and scripts alongside it.

## Quick Start

Download the binary from the [latest release](https://github.com/rhsev/mi.lan/releases/latest)
(`milan-darwin-arm64` for Apple Silicon, `milan-darwin-amd64` for Intel,
`milan-linux-amd64` / `milan-linux-arm64` for Linux).
`config.yaml.example` is attached to the same release, or take it from this
repository.

```bash
chmod +x milan-darwin-arm64 && mv milan-darwin-arm64 milan
# Downloaded with a browser? Clear the quarantine flag first, or macOS refuses
# to run an unsigned binary:  xattr -d com.apple.quarantine milan

# Setup config
cp config.yaml.example config.yaml
# Edit config.yaml: add allowed IPs

# Start Milan
./milan start --standalone
```

The release ships the runner, not the scripts: a fresh install answers
`/health` and `/status` and lists no endpoints at all. The scripts under
`scripts/` in this repository are examples (`hello`, `greet`, `counter`) plus
the tools that travel with milan; clone or copy the ones you want, or write
your own. An endpoint is any executable file dropped in `scripts_dir`.

`--standalone` skips the identity check. Without it, `milan start` asks Dylan
to confirm its name and refuses to start when nobody answers or Dylan
disagrees, so use the flag until Dylan is running. Point `DYLAN_URL` at your own instance (default
`http://dy.lan/whoami`) and drop the flag.

Or build from source instead of downloading:

```bash
make build              # writes ./milan, next to the config it reads
make install            # copies it to /usr/local/bin (PREFIX=~/.local for a user install)
```

One macOS trap when building yourself: a bare `go build` leaves an *ad-hoc*
code signature that changes with every build, and macOS ties the "Local
Network" permission to the signature, so a rebuilt milan can silently lose LAN
access to its scripts, while internet and ping keep working. milan flags
such a build at startup, in `/status`, in `milan status`, and as an
`X-Milan-Warning` header on `/health`. The fix is signing with a stable
identity after every build. A self-signed certificate is enough; it does
not have to be trusted. That is what the warning means by `build.sh`; yours
needs just two lines:

```bash
make build              # not bare `go build`: make stamps the version from the tag
codesign -f -s "your-dev-cert" milan
```

## Configuration

`config.yaml` (copy from `config.yaml.example`):

```yaml
milan:
  port: 8080
  allowed_ips:
    - "192.168.1.*"
  scripts_dir: "./scripts"
  notes:
    - id: my-notes
      path: /path/to/notes
```

| Key | Default | Description |
|---|---|---|
| `name` | LocalHostName, lower-cased (Linux: host name) | Agent name claimed at Dylan (see [Identity check](#identity-check)). Leave it unset in a `config.yaml` shared between machines |
| `port` | `8080` | HTTP port Milan listens on |
| `bind` | all interfaces | Single address to listen on, e.g. a Tailscale IP. Empty keeps the old behaviour |
| `allowed_ips` | | IPs allowed to trigger scripts. Wildcards supported (`192.168.1.*`). Localhost is always allowed |
| `scripts_dir` | `./scripts` | Directory for scripts, relative to the binary |
| `secrets` | | Per-script HMAC secrets (see [Signed requests](#signed-requests)) |
| `notes` | | List of note sources (see [Notes / Wiki](#notes--wiki)) |
| `script_env` | | Environment for scripts: `path_prepend` and `vars` (see [Script environment](#script-environment)) |

## Usage Examples

Via Dylan (Remote):

* `http://mi.lan/hello` triggers `./scripts/hello.rb` on your Mac via Dylan
* `http://mi.lan/shortcut/Note` triggers Apple Shortcut "Note"
* `http://mi.lan/shortcut/Note/Hello%20Milan` triggers Shortcut "Note" with input "Hello Milan"

Standalone (Local):

* `http://localhost:8080/hello/World` runs `scripts/hello.rb` with "World" as `ARGV[0]` locally on your Mac
* `http://localhost:8080` sends status information

A POST body reaches the script on stdin, unchanged. The script decides what
it means (parse it with `jq`, `JSON.parse`, or ignore it). URL argument and body
combine freely:

```bash
curl -d '{"branch":"main"}' http://localhost:8080/deploy
```

## Streaming

Scripts can stream output line by line via SSE (Server-Sent Events):

```
GET /stream/<script>
GET /stream/<script>/<arg>
```

The response is a `text/event-stream`. Each line of stdout is sent as a `data:` event. When the script finishes, Milan sends `event: done`. On non-zero exit: `event: stream_error`. A POST body reaches stream scripts on stdin, same as one-shot calls.

**Background mode:** If the client disconnects mid-stream, Milan switches to silent mode: the script keeps running, collects output into a log file, and records a background job entry when it finishes.

## Background Jobs

When a stream is abandoned, Milan records the job in `data/jobs/status.json`:

```
GET /jobs/all       → all job records (JSON)
GET /jobs/pending   → unacknowledged jobs
GET /jobs/ack/<id>  → mark job as acknowledged
```

Jobs are identified by `<script>_<timestamp>` and include script name, exit status, log path, timestamp, and acknowledged flag. History is capped at 100 entries.

## Widget Inbox

A place for scripts to leave their current state. Each target keeps only the
latest push: a new push replaces the previous one, so a copy job can update
its progress once a second and the target still holds one entry. Dylan's board reads
all targets at once and draws them as tiles.

```
POST   /widget/<target>        → push (JSON body, or text/plain + query params)
GET    /widgets                → every target as one JSON array
DELETE /widget/<target>        → remove the tile
GET    /widget/<target>/clear  → same, for callers that cannot send DELETE
```

Targets must match `^[a-z0-9_-]{1,32}$`, because they are path segments on disk
(`data/widgets/<target>.json`, written atomically). Fields, all optional
except the target: `title`, `text`, `progress` (0–100), `icon`, `color`,
`urgent`, `ttl`, `sort`. The server adds `updated_at`.

`ttl` (seconds) marks how long the state stays valid; afterwards the board
greys the tile out instead of deleting it. Without `ttl` the tile stands
until something replaces it. `text` may contain ANSI; Milan stores it raw and
lets the display side decide. With `urgent: true` the text additionally goes
through [ticker](https://github.com/rhsev/ticker) once, on arrival.

`scripts/widget` is the client, a thin wrapper around curl:

```bash
widget copy --text "Backup läuft" --progress 42 --icon download --ttl 120
na | widget na --color '#A3BE8C'      # stdin becomes the text
widget copy --clear
```

It talks to `http://127.0.0.1:8080` unless `MILAN_URL` says otherwise;
localhost is always allowed, so local scripts push without any configuration.

## Notes / Wiki

Milan can serve Markdown and HTML files from configured directories:

```
GET /notes                          → list sources (JSON)
GET /notes/<source>                 → list files in source (JSON)
GET /notes/<source>/<file>          → render file (HTML)
GET /notes/<source>/assets/<path>   → serve asset (image or CSS)
```

Markdown files are rendered via [Apex](https://github.com/ttscoff/apex). HTML files are served as-is. Both `images/` and `css/` subdirectories are served as assets.

Configure sources in `config.yaml`:

```yaml
milan:
  notes:
    - id: my-notes
      path: /path/to/notes/directory
```

Via URL Scheme:

* `milan://hello/World` runs `scripts/hello.rb` like the HTTP call, but without opening Safari
* `milan://stream/hello/World` uses the streaming endpoint, needed for long-running scripts or GUI apps
* `ref://` works the same way as `milan://`, but is intended for document references rather than script execution

The `milan://` and `ref://` URL schemes are handled by [ticker](https://github.com/rhsev/ticker), which registers them as part of its app bundle. No separate URL handler app is needed.

## Service Control (milan)

```bash
./milan start                # Start with Dylan identity check
./milan start --standalone   # Start without Dylan
./milan stop                 # Stop service
./milan restart --standalone # Restart service
./milan status               # Show status and PID
./milan log                  # Tail the log file
./milan whoami               # Check identity with Dylan
```

`milan start` ignores a stale PID file, refuses to start when the address is already in use, and waits for the health endpoint to answer before it reports success.

Under a supervisor, restart through it instead: milan detects launchd itself and says so; under systemd use `systemctl restart milan` (see the notes in `scripts/milan.service`).

## Writing Scripts

Scripts live in `./scripts/` (or `./scripts/custom/` for private scripts, gitignored) and receive URL path segments as arguments. milan looks in `custom/` first. Supported types, in lookup order:

| Extension | Interpreter |
|-----------|-------------|
| (none)    | direct (needs executable bit) |
| `.rb`     | Ruby        |
| `.sh`     | sh          |
| `.py`     | python3     |

Apple Shortcuts are handled by `scripts/shortcut.rb` via the `shortcuts` CLI; no special extension is needed.

Examples:

```ruby
# scripts/hello.rb
#!/usr/bin/env ruby
name = ARGV[0] || 'World'
puts "Hello, #{name}!"
```

```bash
# scripts/greet.sh
#!/bin/sh
echo "Hello, ${1:-World}!"
```

Lookup order is the table's order, so a **compiled binary wins over a
same-named script**. That is how a slow script gets replaced without its
endpoint URL changing: build the port next to the original, and the original
stays as the reference for comparing output.

`scripts/custom/livesync` (source in [cmd/livesync](cmd/livesync)) is an
example: the daemon health check, ported from `livesync.rb` because 106 of its
185 ms were Ruby starting up for a check that runs every five minutes. The
endpoint went from 137 ms to 38 ms.

```sh
go build -o scripts/custom/livesync ./cmd/livesync
```

Rules:

* Script names: `[a-z0-9_-]` only
* One script per name: if `hello.rb` and `hello.sh` both exist, the first in lookup order (`hello.rb`) runs
* Timeout: 5 seconds (synchronous execution); no timeout for streams
* POST body → stdin (capped at 10 MB)
* stdout → HTTP response
* Exit code != 0 → HTTP 422
* Environment: milan's own, plus `script_env` and a UTF-8 locale (see below)
* HTML output: escape every interpolated data value at render time (Ruby → `CGI.escapeHTML` / a small `h()` helper, Go → `html/template`, bash → don't build HTML with data). Dylan can't do it for you: by the time it has the assembled HTML, data and markup are already mixed. Scraped content (page titles, descriptions) is attacker-influenceable, so this is not optional for data-bearing HTML.

### Script environment

Scripts inherit milan's environment, and that depends on how milan was
started. A launchd plist or a systemd unit is sparse (a short `PATH`, no
locale), while a terminal is not. So a script can work when milan runs by hand
and fail as a service: a tool in `~/bin` is not found, or Ruby reads a file
name with an umlaut as ASCII and raises.

`script_env` in `config.yaml` closes that gap the same way however milan runs,
and keeps machine-specific paths out of the service definition:

```yaml
milan:
  script_env:
    path_prepend: [~/bin]          # in front of the inherited PATH
    vars:                          # set or override
      GRUBBER_NOTES: ~/Notes
      REGISTER_BIN: ~/bin/register
```

A leading `~` expands to the home directory. One default needs no config: if
none of `LC_ALL`, `LC_CTYPE` and `LANG` is set, scripts get
`LANG=en_US.UTF-8` on macOS and `LANG=C.UTF-8` elsewhere. The startup banner
shows what was added (variable names only; values stay out of the log because the
section may hold tokens).

## Security

* IP Allowlist: Only configured IPs can trigger scripts
* Wildcards: `192.168.1.*` allows entire subnet
* Localhost: Always allowed (127.0.0.1, ::1)
* Bind: `bind:` narrows the listener to one interface; the others get no open port
* Signatures: scripts listed under `secrets:` require a signed request body
* Script Names: Validated (no path traversal possible)

## Signed Requests

For callers whose address cannot be allowlisted, such as a cloud service's
webhook arriving through a reverse proxy, list the script under `secrets:` and sign
the raw request body the way GitHub does:

```yaml
milan:
  secrets:
    deploy: "long-random-string"
```

```bash
sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}')
curl -d "$body" -H "X-Hub-Signature-256: sha256=$sig" http://host:8080/deploy
```

A listed script answers 403 unless the signature matches (a GET needs the
signature of the empty body); scripts without an entry stay open. The IP
allowlist still applies first: for webhook senders, allowlist the proxy; the
signature then decides.

## Dylan Integration

To connect Dylan to Milan agents, configure `config/milan.yaml` on Dylan:

```yaml
milan:
  enabled: true
  agents:
    mini: "http://192.168.1.118:8080"   # Mac Mini
    book: "http://192.168.1.188:8080"   # MacBook

```

With the `35-milan-connect.rb` plugin, requests are routed through Dylan:

```
http://mi.lan/mini/hello/World  ->  Mac Mini: GET /hello/World
http://mi.lan/book/shortcut/Note  ->  MacBook: GET /shortcut/Note

```

Routing goes by these URLs alone. The identity check below never changes
where Dylan sends a request.

### Identity check

`milan start`, `restart` and `whoami` tell Dylan who they are:
`GET http://dy.lan/whoami?name=<name>` (or `$DYLAN_URL` with the name added).
The name is `name:` from `config.yaml`, or else the machine's LocalHostName
lower-cased, so a Mac called `Mini` claims `mini` (on Linux: the short host
name). Dylan checks the claim
against its agent list:

| Dylan answers | Meaning | milan |
|---|---|---|
| `200 mini (192.168.1.118)` | the claim matches the caller's address | `OK - I am mini (192.168.1.118)` |
| `404 Unknown agent: x` | no agent of that name | refuses to start |
| `409 book is registered at … but called from …` | the claim comes from another address | refuses to start |

milan also refuses a 200 that names an agent other than the one it claimed.

Behind NAT the caller's address is the router's. A MacBook on a Thunderbolt
cable to the mini reaches Dylan with the mini's address, and used to be told
"you are mini". List that address for it on Dylan, and it starts as itself
with Wi-Fi on or off:

```yaml
milan:
  whoami_also_from:
    book: ["192.168.1.118"]
```

Such an OK says who the caller is, not that Dylan can reach it. On the cable
the MacBook's milan serves only locally (`milan://` links through ticker),
and becomes reachable for Dylan again as soon as Wi-Fi brings its address
back, without a restart.

Deploy Dylan before milan: a Dylan older than the name check does not route
`/whoami?name=` and answers 404. The other way round works, since a current
Dylan still answers an old milan that sends no name, by address alone.

## License

MIT

---

*Part of a family of plain-text tools. See the [profile page](https://github.com/rhsev) for an overview.*
