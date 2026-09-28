#!/usr/bin/env bash
# Agent half: installs the broker client for the agent user. Run as the
# agent user, with the bwai binary to install as the first argument.
#
#   ./setup-agent.sh /path/to/bwai
set -euo pipefail

BIN=${1:?usage: setup-agent.sh /path/to/bwai}
SOCK=$HOME/.bwai/broker.sock

install -D -m 0755 "$BIN" "$HOME/.local/bin/bwai"
# The client personas dispatch on argv[0]; they live off the default PATH
# dir so `git-safe` and friends only appear where this file puts them.
LIBEXEC=$HOME/.local/libexec/bwai
mkdir -p "$LIBEXEC"
for p in bwai-outside git-safe bwai-gpg; do
	ln -sf "$HOME/.local/bin/bwai" "$LIBEXEC/$p"
done

mkdir -p "$HOME/.bashrc.d"
cat >"$HOME/.bashrc.d/bwai.sh" <<EOF
export BWAI_BROKER_SOCKET=$SOCK
case ":\$PATH:" in *":$LIBEXEC:"*) ;; *) PATH=$LIBEXEC:\$PATH ;; esac
EOF

# Commits are signed on the host by the broker; git only needs to hand the
# buffer to the shim. The key named here is ignored by the host.
git config --global gpg.program "$LIBEXEC/bwai-gpg"
git config --global commit.gpgsign true
git config --global user.signingkey host

echo "installed; open a new shell, then: bwai-outside --context"
