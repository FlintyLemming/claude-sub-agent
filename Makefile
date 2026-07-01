BINARY   := claude-usage-agent
GOFLAGS  := -trimpath
LDFLAGS  := -s -w

.PHONY: all build install uninstall run test vet fmt clean

all: build

## build: compile the binary for the current (darwin/arm64) host
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

## run: build then run the daemon in the foreground (use Ctrl-C to stop)
run: build
	./$(BINARY) daemon

## install: build, copy the binary to /usr/local/bin, and register the launchd agent.
## Note: writing to /usr/local/bin needs sudo on recent macOS — run as:
##   sudo make install   (or set PREFIX to a writable dir, e.g. make install PREFIX=$(HOME)/.local/bin)
PREFIX ?= /usr/local/bin
install: build
	@echo ">> installing binary to $(PREFIX)/$(BINARY)"
	@install -m 0755 $(BINARY) $(PREFIX)/$(BINARY)
	@echo ">> registering launchd agent"
	@./$(BINARY) install

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
