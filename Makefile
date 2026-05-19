BINARY  := bwai
CMD     := ./cmd/bwai
BIN_DIR := bin

VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"

# Install location. Defaults to ~/.local/bin so no root needed and the
# binary lands where the README's curl install also puts it. Override
# with `make install PREFIX=/usr/local` for a system-wide install.
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

.PHONY: all build clean test lint fmt install uninstall install-hooks

all: build

build:
	go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY) $(CMD)

install: build
	install -d $(BINDIR)
	install -m 0755 $(BIN_DIR)/$(BINARY) $(BINDIR)/$(BINARY)
	@echo "installed $(BINDIR)/$(BINARY)"

uninstall:
	rm -f $(BINDIR)/$(BINARY)
	@echo "removed $(BINDIR)/$(BINARY)"

test:
	go test ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "The following files are not formatted:"; gofmt -l .;  exit 1; }

lint:
	golangci-lint run ./...

install-hooks:
	cp scripts/hooks/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit scripts/check.sh

clean:
	rm -rf $(BIN_DIR)/$(BINARY)
