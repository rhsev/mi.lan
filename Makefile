BIN    := milan
BINDIR ?= $(HOME)/bin
PREFIX ?= /usr/local

# The dispatcher plus the two things that travel with it: `widget` (a shell
# script in scripts/, the board's push command) and the compiled livesync
# health check, which lives in the machine-local scripts/custom/ and is built
# rather than shipped. Convention: ../BUILD.md.

# Stamp the version from the tag rather than trusting a literal in main.go,
# which shipped 2.2.0 as 2.1.0. A dirty or untagged tree says so in /status.
VERSION := $(shell git describe --tags --dirty --always 2>/dev/null | sed 's/^v//')
# Outside a checkout (a source tarball) describe says nothing, and an empty
# -X would stamp an empty version; fall back to the variable's own default.
ifeq ($(VERSION),)
VERSION := dev
endif
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build install uninstall livesync reach link unlink test clean

build:
	go build -ldflags="$(LDFLAGS)" -o $(BIN) .

livesync:
	go build -o scripts/custom/livesync ./cmd/livesync

# Same idea as livesync: the busiest endpoint, compiled. The extensionless
# binary in scripts/custom/ takes precedence over reach.rb, which stays as the
# reference for the ping flags and their measurements.
reach:
	go build -o scripts/custom/reach ./cmd/reach

# Link once, then never again: rebuilding is deploying. A stale copy fails silently,
# a dangling link fails at the next call.
link: build
	@install -d $(BINDIR)
	@ln -sfn $(CURDIR)/$(BIN) $(BINDIR)/$(BIN)
	@ln -sfn $(CURDIR)/scripts/widget $(BINDIR)/widget
	@echo "linked $(BINDIR)/$(BIN) and $(BINDIR)/widget -> $(CURDIR)"

unlink:
	rm -f $(BINDIR)/$(BIN) $(BINDIR)/widget

test:
	go test ./...

# milan.pid and milan.log are runtime state, not build output — left alone.
# Published repo: `install` copies into $(PREFIX)/bin for anyone who clones
# this. A clone is not a stable place to point a symlink at — the symlink
# form is `make link`, for the machine this is developed on. See ../BUILD.md.
install: build
	install -d $(PREFIX)/bin
	install -m 755 $(BIN) $(PREFIX)/bin/$(BIN)

uninstall:
	rm -f $(PREFIX)/bin/$(BIN)

clean:
	rm -f $(BIN)
	rm -rf dist

# Cross-compiled release binaries, named as the README's Quick Start expects.
# Pure Go with CGO off: each file is static, so one Linux binary covers every
# distro, glibc or musl. dist/ is gitignored; the files attach to a release.
RELEASE_TARGETS := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64

.PHONY: release
release:
	@mkdir -p dist
	@for t in $(RELEASE_TARGETS); do \
		echo "dist/$(BIN)-$$t"; \
		GOOS=$${t%%-*} GOARCH=$${t##*-} CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$(BIN)-$$t . || exit 1; \
	done
