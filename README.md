# bubblewrap-ai

Runs AI coding agents (Claude, Gemini, Goose) inside a [bubblewrap](https://github.com/containers/bubblewrap) sandbox. The host filesystem is read-only, only the current project directory and the dotfiles you whitelist are accessible. The sandbox also starts with a clean environment, only variables explicitly allowed are visible to the agent.

## Requirements

- Linux
- [`bwrap`](https://github.com/containers/bubblewrap) installed (e.g. `sudo dnf install bubblewrap` or `sudo apt install bubblewrap`)

## Install

### From GitHub Releases (recommended)

```sh
curl -Lo ~/.local/bin/bwai https://github.com/umago/bubblewrap-ai/releases/latest/download/bwai
chmod +x ~/.local/bin/bwai
```

### From source

```sh
make build
cp bin/bwai ~/.local/bin/
```

## Usage

Run `bwai` from inside the project directory you want to give the agent access to:

```sh
cd ~/my-project
bwai
```

By default, `bwai` opens a sandboxed `bash` shell. From there you can launch any agent:

```sh
[🫧] > claude
[🫧] > goose
[🫧] > gemini
```

### Running a command directly

To skip the shell and launch an agent (or any command) directly, you can either:

1. Set the `command` field in `~/.bwai.json`:

```json
{ "command": ["claude"] }
```

2. Use the `--command` (or `-c`) CLI flag, which overrides the config file:

```sh
bwai --command claude

```

To append arguments to the command configured in `~/.bwai.json`, use `--`:

```sh
# With "command": ["goose"] in config
bwai -- session -r  # runs "goose session -r" to resume a session

# With "command": ["claude"] in config
bwai -- --model gemini-2.0-flash-exp  # runs "claude --model gemini-2.0-flash-exp"
```

Everything after `--` is passed as extra arguments to the resolved command.

## Configuration

`bwai` works out of the box with no config file. To customise behaviour, create `~/.bwai.json` as a global config. This can be overridden per-run with the `--config` flag:

```sh
bwai --config /path/to/my-config.json
```

To see the full default configuration as a starting point, run:

```sh
bwai --dump-config > ~/.bwai.json
```

Example `~/.bwai.json`:

```json
{
  "bwrap_path": "bwrap",
  "bwrap_extra_args": ["--unshare-pid", "--unshare-ipc"],
  "command": ["bash"],
  "home_allow": [
    ".claude",
    ".gemini",
    ".claude.json",
    ".config/goose",
    ".config/gcloud",
    ".local/state",
    ".local/share/goose",
    ".cache",
    ".cargo"
  ],
  "home_block": [
    ".gnupg",
    ".ssh",
    ".pki",
    ".aws",
    ".kube",
    ".azure",
    ".bashrc",
    ".bashrc.d",
    ".password-store",
    ".bash_history*",
    ".config/Bitwarden"
  ],
  "env_allow": [
    "TERM",
    "COLORTERM",
    "LANG",
    "LC_ALL",
    "LC_MESSAGES",
    "LC_CTYPE",
    "HOME",
    "USER",
    "LOGNAME",
    "PATH",
    "EDITOR",
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_MODEL",
    "ANTHROPIC_DEFAULT_OPUS_MODEL",
    "ANTHROPIC_DEFAULT_SONNET_MODEL",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL",
    "CLAUDE_CODE_USE_VERTEX",
    "CLOUD_ML_REGION",
    "ANTHROPIC_VERTEX_PROJECT_ID",
    "GEMINI_API_KEY",
    "GOOGLE_API_KEY",
    "GCLOUD_PROJECT",
    "GOOGLE_CLOUD_PROJECT",
    "GOOSE_PROVIDER",
    "GOOSE_MODEL",
    "GOOSE_PLANNER_PROVIDER",
    "GOOSE_PLANNER_MODEL",
    "OPENAI_API_KEY",
    "OPENAI_API_BASE",
    "OPENROUTER_API_KEY",
  ]
}
```

| Field | Description | Default |
|---|---|---|
| `bwrap_path` | Path to the `bwrap` binary | `"bwrap"` |
| `bwrap_extra_args` | Extra arguments forwarded to `bwrap` (e.g. `--unshare-net`) | `["--unshare-pid", "--unshare-ipc"]` |
| `command` | Command (and args) to run inside the sandbox | `["bash"]` |
| `home_allow` | Dotfiles/dirs in `$HOME` the agent may read and write | see above |
| `home_block` | Dotfiles/dirs in `$HOME` that are never exposed | see above |
| `env_allow` | Environment variables from the host passed into the sandbox | see above |
| `broker` | Host-execution broker for letting specific commands escape the sandbox with user approval. See below. | disabled |

`home_allow` takes precedence over `home_block`.

## Host-execution broker (experimental)

Sometimes an agent needs to run something that requires keys the sandbox deliberately hides — `git commit -S` needs `~/.gnupg`, `git push` over SSH needs `~/.ssh`. The broker lets specific argv lists escape to the host with per-command rules.

Enable it by adding a `broker` block to `~/.bwai.json`:

```json
{
  "broker": {
    "enabled": true,
    "approval_timeout_s": 120,
    "rules": [
      { "match": ["git", "status"],                 "action": "auto_allow" },
      { "match": ["git", "commit", "-S", "-m", "fix"], "action": "confirm" }
    ]
  }
}
```

Rules in this release match argv **literally** — no `*` / `**` wildcards yet. Each token must equal the corresponding sandbox argv exactly. Anything not matched by a rule is denied.

Three actions:

- `auto_allow` — runs immediately on the host.
- `confirm` — runs only after explicit approval from a second terminal.
- `auto_deny` — explicit reject (use to carve exceptions out of broader rules in a later release).

Inside the sandbox, the agent invokes `bwai-outside` instead of the bare command:

```sh
bwai-outside git commit -S -m "fix"
```

If the rule action is `confirm`, the sandbox sees a `waiting for host approval` message and the broker enqueues the request. From a second terminal on the host:

```sh
$ bwai approve
1 pending request:

[a7f3c0e1] sandbox wants to run on host
  cwd: /home/oscar/source/repos/foo
  cmd: git commit -S -m fix
  age: 4012ms
[y]es / [n]o / [a]lways-this-session / [s]kip
> y
approved.
```

`always-this-session` adds the exact argv to an in-memory allowlist for the lifetime of this `bwai` process. Never persisted.

The audit log lands at `~/.local/state/bwai/broker.log` as JSONL: timestamp, argv, cwd, matched rule, decision, exit code.

See `docs/broker.md` for the full design.
