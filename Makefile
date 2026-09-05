BIN    := milan
BINDIR ?= $(HOME)/bin
PREFIX ?= /usr/local

# The dispatcher plus the two things that travel with it: `widget` (a shell
# script in scripts/, the board's push command) and the compiled livesync
# health check, which lives in the machine-local scripts/custom/ and is built
# rather than shipped. Convention: ../BUILD.md.

.PHONY: build install uninstall livesync link unlink test clean

build:
	go build -o $(BIN) .

livesync:
	go build -o scripts/custom/livesync ./cmd/livesync

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
