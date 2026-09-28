BINARY   := claude-usage-agent
GOFLAGS  := -trimpath
LDFLAGS  := -s -w

.PHONY: all build install uninstall run test vet fmt clean

all: build

## build: compile the binary for the current host (set GOOS/GOARCH to cross-compile)
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

## build-windows: cross-compile the Windows amd64 binary
build-windows:
	GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY).exe .

## run: build then run the daemon in the foreground (use Ctrl-C to stop)
run: build
	./$(BINARY) daemon

## install: build, copy the binary to $(PREFIX), and register the background
## service (launchd agent on macOS, systemd user unit on Linux) pointing at the
## installed copy. Flags come from the environment, e.g.
##   CLAUDE_USAGE_PUSH_URL=... CLAUDE_USAGE_PUSH_TOKEN=... make install
## macOS: writing to /usr/local/bin needs sudo — run `sudo make install`, or set
## PREFIX to a writable dir (make install PREFIX=$(HOME)/.local/bin).
## Linux: PREFIX defaults to ~/.local/bin; the service is per-user, so don't sudo.
PREFIX ?= $(if $(filter Linux,$(shell uname -s)),$(HOME)/.local/bin,/usr/local/bin)
install: build
	@echo ">> installing binary to $(PREFIX)/$(BINARY)"
	@mkdir -p $(PREFIX)
	@install -m 0755 $(BINARY) $(PREFIX)/$(BINARY)
	@echo ">> registering the background service"
	@$(PREFIX)/$(BINARY) install

## uninstall: unload the agent and delete the plist (binary left in place)
uninstall:
	./$(BINARY) uninstall

## test: run the full test suite
test:
	go test ./...

## vet: run go vet
vet:
	go vet ./...

## fmt: format all Go sources
fmt:
	gofmt -w *.go

## clean: remove build artifacts
clean:
	rm -f $(BINARY)
