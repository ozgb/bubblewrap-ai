# Agent box: the broker as a daemon

`bwai` puts one agent in a bwrap sandbox and starts a broker for that
session. On a machine dedicated to agents you may want something else:
agents that run for days, in their own containers with `sudo` for
package installs, launched remotely. `bwai broker serve` is the broker
for that shape. It runs as the user who holds the credentials, and the
agents run as a different Unix user.

```
┌─ user oscar (holds gh token, ssh key, gpg key) ──────┐
│  systemd --user: bwai broker serve                   │
│    ├─ ~agent/.bwai/broker.sock  (peer uid checked)   │
│    ├─ $XDG_RUNTIME_DIR/bwai-broker/approve.sock      │
│    └─ ~/.local/share/bwai-broker/mirrors/  (pushes)  │
└──────────────────────────────────────────────────────┘
                       ▲ unix socket
┌─ user agent (no wheel, no docker) ───────────────────┐
│  toolbox / podman (rootless, sudo inside)            │
│    opencode, claude, …                               │
│    bwai-outside gh …   git-safe push   git commit    │
│    work tree: ~agent/work (broker has read ACL)      │
└──────────────────────────────────────────────────────┘
```

## Why a separate user, not just a container

The boundary is the UID. A Silverblue toolbox is not a sandbox: it
bind-mounts your real `$HOME`, exposes the host root at `/run/host`,
shares the host network and PID namespaces, and passes your session bus
through, so `flatpak-spawn --host` runs anything on the host. An agent in
*your* toolbox can read `~/.config/gh/hosts.yml` directly.

An agent in the *agent user's* toolbox gets all of that too, but only
for the agent user: its own home, its own session bus. Your home is
`0700`, your gpg-agent and ssh-agent sockets live in `/run/user/<you>`,
and root inside a rootless container is the agent's UID outside it. So
the agent can have passwordless `sudo` in its container, and install
whatever it likes, without getting any closer to your tokens.

Keep the agent user out of `wheel`, `docker` and `libvirt`, which are
all root by another name.

## Setup

On the box, as the credential-holding user, with this repository checked
out:

```sh
make build && install -m 0755 bin/bwai ~/.local/bin/bwai

# Root half: agent user, read ACL on its work tree, socket dir, linger.
sudo scripts/agent-box/setup-host.sh agent

# Agent half: client personas, BWAI_BROKER_SOCKET, git signing shim.
sudo -u agent scripts/agent-box/setup-agent.sh ~/.local/bin/bwai
```

Then add a `serve` block to `~/.config/bwai/bwai.json`. The rules and
`push_allowed_urls` are the same ones a sandbox would use:

```json
{
  "broker": {
    "enabled": true,
    "serve": {
      "socket": "/var/home/agent/.bwai/broker.sock",
      "allowed_uids": [1001],
      "roots": ["/var/home/agent/work"]
    },
    "push_allowed_urls": ["git@github.com:you/project.git"],
    "rules": [
      { "match": ["git-sign"],              "action": "auto_allow" },
      { "match": ["git-safe", "push", "**"], "action": "auto_allow" },
      { "match": ["git-safe", "**"],         "action": "auto_deny" }
    ]
  }
}
```

and start the daemon:

```sh
install -D -m 0644 scripts/agent-box/bwai-broker.service ~/.config/systemd/user/bwai-broker.service
systemctl --user daemon-reload
systemctl --user enable --now bwai-broker
```

`gh`, `git` and whatever else your rules name must be on the broker
user's host `PATH`, not only inside a toolbox.

## Using it

`ssh agent@box` gets the agent a real login session (the setup copies
your `authorized_keys`), which `toolbox create` / `toolbox enter` need.
Inside, `bwai-outside --context` prints the guidance and live rules; put
it wherever the agent reads instructions from, e.g.

```sh
bwai-outside --context > ~/.config/opencode/AGENTS.md
```

Work under `~/work`. The broker refuses requests from anywhere else, and
it compares paths as the host sees them, so the work tree has to be at
the same path inside the container. Toolbox mounts `$HOME` at the same
path, so `~/work` qualifies.

## What the daemon changes

- **Socket and peer check.** `serve.socket` is mode `0666` inside a
  directory only the broker user and the agent's group can traverse.
  Every connection's `SO_PEERCRED` uid must be in `allowed_uids`. Under
  toolbox's keep-id mapping the agent's normal user inside the container
  is the agent's uid on the host; root inside the container (`sudo`) is a
  subordinate uid, so it is refused unless listed.
- **Roots.** `serve.roots` replaces the per-session project dir. It
  checks the request cwd and confines `git-safe push`'s git dir.
- **Confirm cap.** The 30-confirm cap counts over the last hour rather
  than the process lifetime.
- **Approvals.** A headless box has no desktop notifications. Confirm
  requests wait for `bwai approve` (over ssh), which also looks for the
  daemon's `approve.sock`. Prefer rules that are `auto_allow` behind a
  closed wrapper.

Everything else is the same broker: host commands run in an empty
directory, `git-safe push` goes through the host mirror, commits are
signed by `git-sign`, and the audit log is at
`~/.local/state/bwai/broker.log`.

## Backstops outside the box

The broker limits what the agent can ask for. These limit what happens
if something gets past it:

- A token that can do less: a fine-grained PAT or GitHub App limited to
  the repositories the agent works on, with contents, issues and pull
  requests, and no administration, secrets or workflow scopes.
- Branch protection or a ruleset on the default branch: no force push,
  no deletion, changes through pull requests. This enforces the same
  policy `git-safe push` does, on GitHub's side. A repository admin can
  apply `scripts/agent-box/default-branch-ruleset.json` with

  ```sh
  gh api -X POST repos/OWNER/REPO/rulesets --input scripts/agent-box/default-branch-ruleset.json
  ```

  It requires one approving review from someone other than the last
  pusher, which matters when the agent pushes with your token: your own
  approval no longer counts.
