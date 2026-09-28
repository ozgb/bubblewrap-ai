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

.PHONY: all build clean test lint fmt install uninstall install-hooks deploy-box

all: build

build:
	go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY) $(CMD)

install: build
	install -d $(BINDIR)
	install -m 0755 $(BIN_DIR)/$(BINARY) $(BINDIR)/$(BINARY)
	@echo "installed $(BINDIR)/$(BINARY)"

# git-safe is not installed on the host. It is a sandbox-side command
# that bwai bind-mounts into /run/bwai/bin, and its host half runs as the
# `bwai git-safe` subcommand, which the broker resolves from the bwai
# binary itself. Older installs left a $(BINDIR)/git-safe symlink behind;
# uninstall still cleans it up.
uninstall:
	rm -f $(BINDIR)/$(BINARY) $(BINDIR)/git-safe
	@echo "removed $(BINDIR)/$(BINARY) (and any legacy git-safe symlink)"

test:
	go test ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "The following files are not formatted:"; gofmt -l .;  exit 1; }

lint:
	golangci-lint run ./...

# Update an agent box (docs/agent-box.md): the broker user's binary, then
# the broker, then the agent's client. Needs ssh access as both users.
#   make deploy-box BOX=192.168.1.62
BOX         ?=
BROKER_USER ?= $(USER)
AGENT_USER  ?= agent

deploy-box: test build
	@test -n "$(BOX)" || { echo "usage: make deploy-box BOX=<host> [BROKER_USER=...] [AGENT_USER=...]"; exit 1; }
	@# Copy then rename, so a running broker's executable is never rewritten in place.
	scp -q $(BIN_DIR)/$(BINARY) $(BROKER_USER)@$(BOX):.local/bin/$(BINARY).new
	ssh $(BROKER_USER)@$(BOX) 'mv -f ~/.local/bin/$(BINARY).new ~/.local/bin/$(BINARY) && systemctl --user restart bwai-broker && systemctl --user is-active bwai-broker'
	scp -q $(BIN_DIR)/$(BINARY) $(AGENT_USER)@$(BOX):.local/bin/$(BINARY).new
	scp -q scripts/agent-box/bwai-refresh-context $(AGENT_USER)@$(BOX):.local/libexec/bwai/bwai-refresh-context
	ssh $(AGENT_USER)@$(BOX) 'mv -f ~/.local/bin/$(BINARY).new ~/.local/bin/$(BINARY) && \
		{ grep -q bwai-refresh-context ~/.bashrc.d/bwai.sh || echo bwai-refresh-context >> ~/.bashrc.d/bwai.sh; } && \
		PATH=~/.local/libexec/bwai:$$PATH BWAI_BROKER_SOCKET=~/.bwai/broker.sock bwai-refresh-context'
	@echo "local:  $(VERSION)"
	@printf 'broker: '; ssh $(BROKER_USER)@$(BOX) '~/.local/bin/$(BINARY) --version'
	@printf 'agent:  '; ssh $(AGENT_USER)@$(BOX) '~/.local/bin/$(BINARY) --version'

install-hooks:
	cp scripts/hooks/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit scripts/check.sh

clean:
	rm -rf $(BIN_DIR)/$(BINARY)
