# bubblewrap-ai

Runs AI coding agents (Claude, Gemini, Goose, opencode, Command Code) inside a [bubblewrap](https://github.com/containers/bubblewrap) sandbox. The host filesystem is read-only, only the current project directory and the dotfiles you whitelist are accessible. The sandbox also starts with a clean environment, only variables explicitly allowed are visible to the agent.

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
make install
```

Installs to `~/.local/bin/bwai` by default. Override with `PREFIX` or `BINDIR`:

```sh
make install PREFIX=/usr/local        # /usr/local/bin/bwai (needs sudo)
make install BINDIR=/opt/bin          # /opt/bin/bwai
```

Or just build without installing: `make build` puts the binary at `bin/bwai`.

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
[🫧] > opencode
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
    ".config/opencode",
    ".local/share/opencode",
    ".cache",
    ".cargo",
    "go/bin",
    ".commandcode"
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
    ".npmrc",
    ".cargo/credentials.toml",
    ".cargo/credentials",
    ".bash_history*",
    ".config/Bitwarden",
    ".cache/nvidia"
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
    "COMMAND_CODE_API_KEY",
    "COMMANDCODE_API_URL",
    "CMD_LOCAL_ONLY",
  ],
  "state_root": "",
  "env_set": {},
  "path_prepend": []
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
| `state_root` | Host directory mounted read-write as a persistent home for tool caches and installed binaries; see below | disabled |
| `env_set` | Literal environment variables to set in the sandbox (`~` expanded) | none |
| `path_prepend` | Directories prepended to the sandbox `PATH`, in order (`~` expanded) | none |
| `broker` | Host-execution broker for letting specific commands escape the sandbox with user approval. See below. | disabled |

For sub-paths (entries containing a `/`), `home_allow` is applied after `home_block` and so takes precedence. A direct (slash-free) name present in both lists is blocked, since blocked top-level entries are skipped entirely.

### Project-local config

A `.bwai.json` in the directory you run `bwai` from is layered on top of the base config (the global `~/.bwai.json`, or `--config` if given). Its **set-like fields** — `home_allow`, `home_block`, `env_allow`, and `path_prepend` (lists, appended) and `env_set` (a map, merged per key) — are added to the base, so a project only names what it adds. Everything else overrides the base value, including the argv lists `command` and `bwrap_extra_args` (appending those would reorder or duplicate flags). A missing local file is ignored.

```json
{
  "home_allow": [".rwe"]
}
```

### Persistent tool caches (`state_root`)

By default the sandbox's package-manager caches are either ephemeral or read-only, and the host's real caches (`.npm`, `.cargo`, …) are still visible to the agent. To keep the sandbox's caches warm across sessions *without* sharing the host's, point `state_root` at a directory bwai owns:

```json
{
  "state_root": "~/.bwai",
  "home_block": [".npm", ".cargo", ".cache/pip", ".cache/uv"]
}
```

bwai creates `~/.bwai`, mounts it read-write, points npm, yarn, pip, uv, cargo, go, bun, Playwright and Hugging Face at subdirectories of it, and prepends `~/.bwai/bin` to `PATH`. `home_block` hides the host's real caches, so the two never mix: the host's tools keep using `~/.npm`, `~/.cargo`, … while the sandbox writes to `~/.bwai/…`, which survives the session.

Relocating `CARGO_HOME` also relocates cargo's config lookup, so copy your cargo config once:

```sh
mkdir -p ~/.bwai/cargo
cp ~/.cargo/config.toml ~/.bwai/cargo/
```

`env_set` covers any variable the built-in bundle doesn't, and `path_prepend` adds directories ahead of `~/.bwai/bin`:

```json
{
  "state_root": "~/.bwai",
  "env_set": { "SOMETHING_HOME": "~/.bwai/something" },
  "path_prepend": ["~/.bwai/extra-bin"]
}
```

A leading `~` in `state_root`, `env_set` values, and `path_prepend` is expanded by bwai; bwrap expands neither `~` nor `$VAR`.

### Git worktrees

Linked git worktrees (created by `worktrunk` or `git worktree add`) keep their real git dir inside the main repo, which the home sandbox hides. `bwai` detects this automatically — no config needed — and bind-mounts the shared git dir read-write at its real host path, so history, `git status`, commits, and branch ops all work inside the sandbox just like in an ordinary checkout.

Creating a worktree *from inside* the sandbox is also covered. `worktrunk` defaults to a sibling of the repo, which without help would land on the sandbox's tmpfs home and vanish when the session ends. `bwai` pre-binds one dedicated host directory — `<parent>/.<repo>.worktrees` — and points `WORKTRUNK_WORKTREE_PATH` at it, so `wt` worktrees persist on the host. Only that root is exposed, not the repo's parent, so sibling checkouts stay hidden. Plain `git worktree add ../foo` does not follow the worktrunk setting and still lands on tmpfs.

**Starting in a worktree** gets the same treatment: `bwai` follows the worktree's `.git` file back to the main checkout and binds the same `<parent>/.<repo>.worktrees` root, so every session of a repo — wherever it starts — shares one persistent worktree root. The main checkout's working tree is mounted read-write too, on by default; set `worktrees.expose_main` to `false` to keep it hidden:

```json
{
  "worktrees": { "expose_main": false }
}
```

With it off, worktrees still get the persistent root, the shared git dir, and full access to sibling `wt` worktrees — only the main checkout's files stay out of reach.

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

Each rule's `match` is an argv pattern. Tokens match literally except for two wildcards: `*` matches exactly one argv slot, and `**` matches zero or more *trailing* slots (last position only). `argv[0]` is always literal. Anything not matched by a rule is denied. See `docs/broker.md` for the full matcher rules and `bwai broker check <argv>...` to dry-run a request against your config.

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

When a `confirm` request lands, the host also fires a desktop notification (via `notify-send`) so you know to run `bwai approve` without watching the terminal. This is part of the `oob` approver and is on by default; drop `"oob"` from `broker.prompt` to opt out, or it silently no-ops if `notify-send` isn't installed.

### One-click approval (`web` mode)

Add `"web"` to `broker.prompt` for a richer host-side flow on graphical sessions:

```json
{
  "broker": {
    "enabled": true,
    "prompt": ["web", "oob"],
    "web": { "addr": "127.0.0.1:0" },
    "rules": [ { "match": ["git", "push", "**"], "action": "confirm" } ]
  }
}
```

Now a `confirm` request raises a desktop notification (over D-Bus) with **Approve** / **Deny** / **Open page** buttons:

- **Approve** / **Deny** resolve the request straight from the toast.
- **Open page** (or clicking the toast body) opens a small loopback web page showing the command, project, cwd, and age, with an extra **Always this session** button.

The page is served on a loopback-only ephemeral port (`web.addr`, default `127.0.0.1:0`). `bwai` prints the base URL on startup.

**Security model.** The sandbox shares the host network namespace (bwrap runs without `--unshare-net`), so the agent *can* reach that loopback port. Two things stop it from approving its own requests:

- Every web decision requires a **single-use, 128-bit per-request token**. The token lives only in the URL embedded in the desktop notification — it never enters the sandbox — and mutations are `POST`-only. Guessing a request id is useless without the token.
- The toast's Approve/Deny buttons need no token because the **D-Bus session bus is host-only**: `DBUS_SESSION_BUS_ADDRESS` isn't passed into the sandbox and `/run` is a tmpfs there, so the agent can't reach the bus to forge a click.

`web.addr` is validated to be a loopback address; `bwai` refuses to start otherwise. Headless or over SSH (no session bus), `web` mode degrades to the `oob` `notify-send` nudge, and `bwai approve` always works — the CLI is the headless fallback, never replaced.

`always-this-session` adds the exact argv to an in-memory allowlist for the lifetime of this `bwai` process. Never persisted.

The audit log lands at `~/.local/state/bwai/broker.log` as JSONL: timestamp, argv, cwd, matched rule, decision, exit code.

### Restricted commands (`git-safe`)

Some commands are too dangerous to expose under their own name. `git push` is the motivating case: no rule pattern can catch `git push origin +main` (force by refspec), `git push origin :main` (delete by refspec), or a `--force` placed after the refspec — tokens match literally and `**` is only valid as the final token.

The answer is a wrapper with a closed argument surface. `git-safe` is built from the same binary as `bwai` — bwai binds that one copy into the sandbox twice, so it appears beside `bwai-outside` under `/run/bwai/bin`. Nothing is installed on the host. It exposes two operations, and both are called directly, with no `bwai-outside` prefix:

```sh
git-safe push
git-safe commit -m "fix bug"
```

In the sandbox `git-safe` is only a client: it forwards `["git-safe", …]` to the broker and decides nothing itself. The policy runs on the host, as the `bwai git-safe` subcommand the broker resolves the request to. That split is load-bearing — the agent can reach anything the sandbox can reach, so a check performed inside the sandbox would be advisory only.

`git-safe push` pushes the current branch to `origin` and refuses everything else: no flags, no refspecs, no other remote, no detached HEAD, no protected branch (`main`/`master`/`trunk`/`develop`), and no non-fast-forward update — it fetches the remote tip and requires it to be an ancestor of `HEAD` before pushing, so a destructive push is impossible by construction. The remote branch is created on the first push.

`git-safe commit` commits what is staged, GPG-signed. The signing key lives in the host's `~/.gnupg`, which the sandbox hides — that is the reason to route a commit through the host at all. It accepts only `-m <message>` (repeat for extra paragraphs) and adds `-S` itself, so signing is not the caller's choice; every other spelling is refused — `--amend`, `-a`/`--all`, `--no-verify`, `--author`, `-F`, and bare pathspecs. The agent cannot rewrite history or skip a hook, and the broker rule never has to describe those flags. It also refuses a detached HEAD, for the same reason `push` does.

That reduces the broker rules to:

```json
{ "match": ["git-safe", "push"],         "action": "auto_allow" },
{ "match": ["git-safe", "commit", "**"], "action": "auto_allow" },
{ "match": ["git-safe", "**"],           "action": "auto_deny" }
```

Note the action: **`auto_allow`, not `confirm`.** That is the payoff of moving the policy into the wrapper. A rule over raw `git push` needs a human because the pattern cannot express the check that matters; `git-safe` carries the check itself, so the ancestry decision is already made in code before git runs. A prompt would add nothing — an approver reading `git-safe push` learns only what the rule already told them — and an approval that is always granted is worse than none, because it trains the habit of approving without reading.

Treat `confirm` as the last resort rather than the cautious default. Every confirm rule stalls an unattended session on a human who may be asleep, so each one is a standing bug report on the rule set: it marks a judgement nobody has moved into code yet. Write the wrapper, then write `auto_allow`. The agent is told the same thing in its injected context — hunt the rule list for an `AUTO_ALLOW` path before issuing a command that lands on a `confirm`.

`commit` needs a trailing `**` because the message is an argument — unlike `push`, it can't be a two-token rule — but the wrapper is what makes the tail safe: it accepts only `-m` pairs, so the pattern never has to enumerate the bad flags.

### Telling the agent it can call `bwai-outside`

The sandbox is a fresh world — an agent like Claude has no way to discover `bwai-outside` on its own. When the broker is enabled, `bwai` writes a fragment describing the tool, **including the current rule set rendered at broker startup**, and exposes it read-only under `/run/bwai/`. So the agent knows what it may and may not run before its first turn, without having to think to run `bwai-outside --list-rules` — that command remains for re-checking live. Each agent opts in with a flag (except opencode, which is injected via a config env var); `bwai` never writes to the agent's own config or memory files.

**Command Code** reads system-prompt extensions from mods, so `bwai` exposes a tiny mod at `/run/bwai/bwai.ts` that appends the fragment. Start it with:

```sh
cmd --mod /run/bwai/bwai.ts
```

The mod reads `/run/bwai/CLAUDE.md` at call time, so the fragment stays the single source of truth. Your own `~/.commandcode/AGENTS.md` is untouched — command-code has no "additional memory directories" setting, and its subdirectory memory only covers files inside the project, so a mod is what makes this opt-in rather than an override.

**Claude Code** reads CLAUDE.md from additional directories, so `bwai` exposes the fragment at `/run/bwai/CLAUDE.md`. Two pieces to wire it up on the agent side:

1. **Tell Claude Code to load memory from additional directories.** Set the env var globally (e.g. in your shell rc) or per-sandbox via the bash rcfile you point `bwai` at:

   ```sh
   # ~/.bashrc.bwai  (or wherever the sandbox shell sources its rc from)
   export CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1
   ```

   Without this var, `--add-dir` won't load CLAUDE.md files.

2. **Start Claude with `/run/bwai` as an additional directory:**

   ```sh
   claude --add-dir /run/bwai
   ```

    Claude reads `/run/bwai/CLAUDE.md` as part of its memory bootstrap and learns it can call `bwai-outside`.

**opencode** needs no opt-in. When the broker is enabled `bwai` exposes a tiny config at `/run/bwai/opencode.json` and sets `OPENCODE_CONFIG` to it, so opencode loads the fragment through its [`instructions`](https://opencode.ai/docs/config/#instructions) option at startup. The env var is a config *override* — your `~/.config/opencode/opencode.json` and the project config still load, and their `instructions` are merged.

Note: opencode v2 accepts `instructions` but does not load its entries; on v2, drop the fragment into an `AGENTS.md` instead.

Inside the sandbox, the agent (or you) can always check what's allowed:

```sh
bwai-outside --help          # usage + rule list
bwai-outside --list-rules    # just the rules, e.g. for piping to less
bwai-outside --check gh pr view 123   # dry-run: which rule would fire?
```

Rules print in config order, first-match wins, each row numbered `rules[N]` — the same index denials and `--check` cite, so "why was this denied?" is a one-line lookup instead of an exercise in re-deriving pattern precedence. Denied and pending messages name the rule that fired (`denied (rule) — rules[12] AUTO_DENY gh secret **`), and since patterns match argv token-by-token, the rule list footer spells out the classic trap: `gh issue -R org/repo create` does **not** match `gh issue create **` (its second token is `-R`, not `create`) — pass the flag to land on the narrow rule.

See `docs/broker.md` for the full design.
