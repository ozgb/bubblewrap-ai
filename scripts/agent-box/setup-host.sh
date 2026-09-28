#!/usr/bin/env bash
# Root half of the agent-box setup: an unprivileged agent user whose work
# tree the broker user can read, and a socket directory both can reach.
# Run with sudo on the box. Idempotent.
#
#   sudo ./setup-host.sh [agent-user] [broker-user]
set -euo pipefail

AGENT=${1:-agent}
BROKER=${2:-${SUDO_USER:?run with sudo, or pass the broker user}}

[[ $EUID -eq 0 ]] || { echo "run as root (sudo)" >&2; exit 1; }

if ! id "$AGENT" &>/dev/null; then
	useradd --create-home --user-group --shell /bin/bash "$AGENT"
fi
# The UID is the boundary: no admin, container-runtime or other groups
# that are root in disguise.
for g in wheel docker libvirt; do
	if id -nG "$AGENT" | tr ' ' '\n' | grep -qx "$g"; then
		gpasswd -d "$AGENT" "$g"
	fi
done
# Rootless podman needs a subordinate id range that overlaps nobody else's.
if ! grep -q "^$AGENT:" /etc/subuid; then
	start=$(awk -F: '{e=$2+$3} e>m{m=e} END{print (m>524288?m:524288)}' /etc/subuid /etc/subgid)
	usermod --add-subuids "$start-$((start + 65535))" --add-subgids "$start-$((start + 65535))" "$AGENT"
fi

HOME_DIR=$(getent passwd "$AGENT" | cut -d: -f6)
WORK=$HOME_DIR/work
SOCK_DIR=$HOME_DIR/.bwai

# The broker reads the agent's repositories (git upload-pack into its
# mirror). An ACL rather than a group, so it applies without re-login.
install -d -o "$AGENT" -g "$AGENT" -m 0700 "$WORK"
setfacl -m "u:$BROKER:x" "$HOME_DIR"
setfacl -R -m "u:$BROKER:rX" "$WORK"
setfacl -R -d -m "u:$BROKER:rX" "$WORK"

# Owned by the broker user so only it can create the socket; the agent
# gets search permission to connect. The broker checks the peer uid too.
install -d -o "$BROKER" -g "$AGENT" -m 0750 "$SOCK_DIR"

# Let the agent log in over ssh with the broker user's keys, so it gets a
# real session (toolbox needs one).
BROKER_HOME=$(getent passwd "$BROKER" | cut -d: -f6)
if [[ -f $BROKER_HOME/.ssh/authorized_keys ]]; then
	install -d -o "$AGENT" -g "$AGENT" -m 0700 "$HOME_DIR/.ssh"
	install -o "$AGENT" -g "$AGENT" -m 0600 "$BROKER_HOME/.ssh/authorized_keys" "$HOME_DIR/.ssh/authorized_keys"
fi

loginctl enable-linger "$AGENT" "$BROKER"

echo "agent user:  $AGENT (uid $(id -u "$AGENT"))"
echo "work tree:   $WORK"
echo "socket dir:  $SOCK_DIR"
