# mi.lan (Milan)

A lightweight URL bridge for macOS automation.

Milan is a HTTP agent designed to execute local scripts and Apple Shortcuts via simple URL calls. It acts as a persistent bridge, allowing you to trigger local automation from any HTTP-capable source (browser, curl, Stream Deck, or other scripts).

It can do both:

* Standalone: It works perfectly as a standalone tool on your Mac.
* Companion: It connects with [dy.lan](https://github.com/rhsev/dy.lan) to act as the remote helper for your Mac, allowing you to trigger complex workflows from any device on your local (or Tailscale) network.

## Why Milan?

* URL Triggers: Turn any local script into an HTTP endpoint instantly.
* Speed: Persistent agent design ensures execution in ~120ms for scripts (and ~1 sec for Shortcuts).
* Simplicity: Single Go binary, no runtime dependencies.
* Privacy: Strict IP allow-listing and no external cloud services.
* Reach: Reach your Mac via Dylan from any network (LAN/VPN).
* Security: Identity verification with the Dylan master at startup.

## The Bridge: Server Redirector to macOS Agents

Milan creates a connection between your server and your client. It establishes a clear separation between logic and execution:

* dy.lan (The Redirector): Your central hub and logic engine running on Docker or Synology. It identifies where a request needs to go and "points" the way.
* mi.lan (The Agent): The local executor on your Mac. It waits for instructions and handles the heavy lifting, like running scripts or Apple Shortcuts.

### The Workflow

1. Request: A client (like your iPhone) sends a request to the redirector (e.g., `http://mi.lan/mini/shortcut/Note`).
2. Handshake: Before starting, the agent can ask the redirector "Who am I?" via `http://dy.lan/whoami` to ensure the bridge is correctly configured.
3. Redirection: The hub recognizes the target agent ("mini") and passes the request to the specific Mac's IP (e.g., `192.168.1.118:8080`).
4. Execution: The agent performs the local action and sends the result back.

```
iPhone -> Dylan (Synology) -> Milan (Mac) -> Script -> Response

```

## Requirements

* macOS (tested on Sequoia)
* Go 1.21+ (to build)
* Ruby 3+ (to run `.rb` scripts)

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
anywhere, create a symlink — Milan uses `filepath.EvalSymlinks` internally and
resolves the real binary location correctly:

```bash
ln -sf /path/to/milan-dir/milan /usr/local/bin/milan
```

Do not move the binary alone without the config and scripts alongside it.

## Quick Start

```bash
# Download binary from releases, or build from source:
go build -o milan .

# Setup config
cp config.yaml.example config.yaml
# Edit config.yaml: add allowed IPs

# Start Milan
./milan start
```

## Usage Examples

Via Dylan (Remote):

* `http://mi.lan/hello` triggers `./scripts/hello.rb` on your Mac via Dylan
* `http://mi.lan/shortcut/Note` triggers Apple Shortcut "Note"
* `http://mi.lan/shortcut/Note/Hello%20Milan` triggers Shortcut "Note" with input "Hello Milan"

Standalone (Local):

* `http://localhost:8080/hello/World` runs `scripts/hello.rb` with "World" as `ARGV[0]` locally on your Mac
* `http://localhost:8080` sends status information

## Streaming

Scripts can stream output line by line via SSE (Server-Sent Events):

```
GET /stream/<script>
GET /stream/<script>/<arg>
```

The response is a `text/event-stream`. Each line of stdout is sent as a `data:` event. When the script finishes, Milan sends `event: done`. On non-zero exit: `event: stream_error`.

**Background mode:** If the client disconnects mid-stream, Milan switches to silent mode — the script continues running, collects output into a log file, and records a background job entry when it finishes.

## Background Jobs

When a stream is abandoned, Milan records the job in `data/jobs/status.json`:

```
GET /jobs/all       → all job records (JSON)
GET /jobs/pending   → unacknowledged jobs
GET /jobs/ack/<id>  → mark job as acknowledged
```

Jobs are identified by `<script>_<timestamp>` and include script name, exit status, log path, timestamp, and acknowledged flag. History is capped at 100 entries.

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

* `milan://hello/World` runs `scripts/hello.rb` — same as the HTTP call, but without opening Safari
* `milan://stream/hello/World` uses the streaming endpoint — required for long-running scripts or GUI apps
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

`milan` is reliable across restarts: it detects stale PID files, clears any process holding the port (via `lsof`), and waits for the HTTP health endpoint to respond before reporting success.

## Writing Scripts

Scripts live in `./scripts/` (or `./scripts/custom/` for private scripts, gitignored) and receive URL path segments as arguments. Supported types:

| Extension | Interpreter |
|-----------|-------------|
| `.rb`     | Ruby        |
| `.sh`     | sh          |
| `.py`     | python3     |
| (none)    | direct (needs executable bit) |

Apple Shortcuts are handled by `scripts/shortcut.rb` via the `shortcuts` CLI — no special extension needed.

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

Rules:

* Script names: `[a-z0-9_-]` only
* One script per name — `hello.rb` and `hello.sh` together cause a 500 error
* Timeout: 5 seconds (synchronous execution); no timeout for streams
* stdout → HTTP response
* Exit code != 0 → HTTP 422

## Security

* IP Allowlist: Only configured IPs can trigger scripts
* Wildcards: `192.168.1.*` allows entire subnet
* Localhost: Always allowed (127.0.0.1, ::1)
* Script Names: Validated (no path traversal possible)

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

## License

MIT
