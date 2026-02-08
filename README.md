# mi.lan (Milan)

A lightweight URL bridge for macOS automation.

Milan is a HTTP agent designed to execute local Ruby scripts and Apple Shortcuts via simple URL calls. It acts as a persistent bridge, allowing you to trigger local automation from any HTTP-capable source (browser, curl, Stream Deck, or other scripts).

It can do both:

* Standalone: It works perfectly as a standalone tool on your Mac.
* Companion: It connects with [dy.lan](https://github.com/rhsev/dy.lan) to act as the remote helper for your Mac, allowing you to trigger complex workflows from any device on your local (or Tailscale) network.

## Why Milan?

* URL Triggers: Turn any local script into an HTTP endpoint instantly.
* Speed: Persistent agent design ensures execution in ~120ms for Ruby scripts (and ~1 sec for Shortcuts).
* Simplicity: One Ruby script, minimal dependencies (async-http).
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
* Ruby 3.1+ (tested with 3.3.10)
* Gems: `async`, `async-http`

## Quick Start

```bash
# Install gems
gem install async async-http

# Setup config
cp config.yaml.example config.yaml
# Edit config.yaml: add allowed IPs

# Start Milan
./milanctl start

```

## Usage Examples

Via Dylan (Remote):

* `http://mi.lan/hello` triggers `./scripts/hello.rb` on your Mac via Dylan
* `http://mi.lan/shortcut/Note` triggers Apple Shortcut "Note"
* `http://mi.lan/shortcut/Note/Hello%20Milan` triggers Shortcut "Note" with input "Hello Milan"

Standalone (Local):

* `http://localhost:8080/hello/World` runs `scripts/hello.rb` with "World" as `ARGV[0]` locally on your Mac
* `http://localhost:8080` sends status information

Via MilanOpener (URL Scheme):

* `milan://hello/World` runs `scripts/hello.rb` — same as the HTTP call, but without opening Safari
* `ref://` works the same way, but is intended for document references rather than script execution

MilanOpener is a minimal Swift app that registers the `milan://` and `ref://` URL schemes and forwards requests to the local Milan agent. It runs as a background-only app (no dock icon, no window).

Build and install:

```bash
cd MilanOpener
mkdir -p MilanOpener.app/Contents/MacOS
swiftc -o MilanOpener.app/Contents/MacOS/MilanOpener MilanOpener.swift -framework AppKit
cp Info.plist MilanOpener.app/Contents/Info.plist
cp -R MilanOpener.app ~/Applications/
```

macOS automatically registers the URL schemes when the app is placed in `~/Applications/`. When updating, always copy `Info.plist` into the app bundle before installing — `swiftc` only compiles the binary, it does not update the plist.

## Service Control (milanctl)

```bash
./milanctl start                # Start with Dylan identity check
./milanctl start --standalone   # Start without Dylan
./milanctl stop                 # Stop service
./milanctl restart --standalone # Restart service
./milanctl status               # Show status and PID
./milanctl log                  # Tail the log file
./milanctl whoami               # Check identity with Dylan

```

## Writing Scripts

Scripts are executable files in `./scripts/`. They receive URL path segments as arguments.

Example `scripts/hello.rb`:

```ruby
#!/usr/bin/env ruby
name = ARGV[0] || 'World'
puts "Hello, #{name}!"

```

Rules:

* Script names: `[a-z0-9_-]` only
* Timeout: 5 seconds
* stdout -> HTTP response
* Exit code != 0 -> HTTP 422

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
