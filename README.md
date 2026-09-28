# bubblewrap-ai

Runs AI coding agents (Codex, Claude, Gemini, Goose, opencode, Command Code) inside a [bubblewrap](https://github.com/containers/bubblewrap) sandbox. The host filesystem is read-only, only the current project directory and the dotfiles you whitelist are accessible. The sandbox also starts with a clean environment, only variables explicitly allowed are visible to the agent.

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
[🫧] > codex
[🫧] > goose
[🫧] > gemini
[🫧] > opencode
```

### Running a command directly

To skip the shell and launch an agent (or any command) directly, you can either:

1. Set the `command` field in `~/.config/bwai/bwai.json`:

```json
{ "command": ["claude"] }
```

2. Use the `--command` (or `-c`) CLI flag, which overrides the config file:

```sh
bwai --command claude

```

To append arguments to the command configured in `~/.config/bwai/bwai.json`, use `--`:

```sh
# With "command": ["goose"] in config
bwai -- session -r  # runs "goose session -r" to resume a session

# With "command": ["claude"] in config
bwai -- --model gemini-2.0-flash-exp  # runs "claude --model gemini-2.0-flash-exp"
```

Everything after `--` is passed as extra arguments to the resolved command.

## Configuration

`bwai` works out of the box with no config file. To customise behaviour, create `~/.config/bwai/bwai.json` (respecting `$XDG_CONFIG_HOME`) as a global config. The legacy `~/.bwai.json` is read only when that file is absent, and prints a deprecation notice. Either can be overridden per-run with the `--config` flag:

```sh
bwai --config /path/to/my-config.json
```

To see the full default configuration as a starting point, run:

```sh
bwai --dump-config > ~/.config/bwai/bwai.json
```

Example `~/.config/bwai/bwai.json`:

```json
{
  "bwrap_path": "bwrap",
  "bwrap_extra_args": ["--unshare-pid", "--unshare-ipc"],
  "command": ["bash"],
  "home_allow": [
    ".codex",
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
    "CODEX_HOME",
    "OPENROUTER_API_KEY",
    "COMMAND_CODE_API_KEY",
    "COMMANDCODE_API_URL",
    "CMD_LOCAL_ONLY",
  ],
  "state_root": null,
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
| `state_root` | Host directory mounted read-write as a persistent home for tool caches and installed binaries; `null` uses the data dir, `""` disables. See below | `$XDG_DATA_HOME/bwai` (`~/.local/share/bwai`) |
| `env_set` | Literal environment variables to set in the sandbox (`~` expanded) | none |
| `path_prepend` | Directories prepended to the sandbox `PATH`, in order (`~` expanded) | none |
| `broker` | Host-execution broker for letting specific commands escape the sandbox with user approval. See below. | disabled |

For sub-paths (entries containing a `/`), `home_allow` is applied after `home_block` and so takes precedence. A direct (slash-free) name present in both lists is blocked, since blocked top-level entries are skipped entirely.

### Project-local config

A `.bwai.json` in the directory you run `bwai` from is layered on top of the base config (the global `~/.config/bwai/bwai.json`, or `--config` if given). Its **set-like fields** — `home_allow`, `home_block`, `env_allow`, and `path_prepend` (lists, appended), `env_set` (a map, merged per key), and `broker.rules` (appended, base first) — are added to the base, so a project only names what it adds. Because rule matching is first-match, base rules keep precedence and a project's rules extend the set rather than shadowing it. Everything else overrides the base value, including the argv lists `command` and `bwrap_extra_args` (appending those would reorder or duplicate flags) and the trust-anchor lists `broker.push_allowed_urls` and `broker.protected_branches` (a project may narrow them). A missing local file is ignored.

The local file sits in the tree the agent can write, and it is read the next time `bwai` starts there, so it is only applied once you have trusted its exact contents:

```sh
bwai trust            # ./.bwai.json; or `bwai trust path/to/.bwai.json`
```

The broker follows config edits during a session: when the global config, the local `.bwai.json` or `trusted.json` changes, it reloads the rules, `push_allowed_urls`, `protected_branches` and the approval timeout within a couple of seconds, and re-renders the rule list in `/run/bwai/CLAUDE.md` (an agent that already read it will not notice until it looks again; `bwai-outside --list-rules` is always live). The same trust rule applies: if a local file that was in use is edited and not re-trusted, the reload is refused and the previous config stays, with a desktop notification. Mounts, `home_allow` and the environment are fixed when the sandbox starts.

`bwai trust` prints the file and records its sha256 in `~/.config/bwai/trusted.json`, which the sandbox cannot write. An untrusted or since-edited file is skipped with a notice, so an agent cannot write itself a wider sandbox — `bwrap_path`, `home_allow: [".ssh"]`, `auto_allow` rules — for its next session.

```json
{
  "home_allow": [".rwe"]
}
```

### Persistent tool caches (`state_root`)

The sandbox's package-manager caches persist on the host under `state_root`, which defaults to `$XDG_DATA_HOME/bwai` (`~/.local/share/bwai`). bwai creates it, mounts it read-write, points npm, yarn, pip, uv, cargo, go, bun, Playwright and Hugging Face at subdirectories of it, and prepends `<state_root>/bin` to `PATH` — so caches stay warm across sessions while the host's own caches are left alone.

The host's real caches (`.npm`, `.cargo`, …) are still read-only visible to the agent. To hide them and guarantee the two never mix, block them:

```json
{
  "home_block": [".npm", ".cargo", ".cache/pip", ".cache/uv"]
}
```

Relocating `CARGO_HOME` also relocates cargo's config lookup, so copy your cargo config once:

```sh
mkdir -p ~/.local/share/bwai/cargo
cp ~/.cargo/config.toml ~/.local/share/bwai/cargo/
```

Set `state_root` to move the whole thing elsewhere, or to `""` to disable it:

```json
{
  "state_root": "~/.bwai",
  "env_set": { "SOMETHING_HOME": "~/.bwai/something" },
  "path_prepend": ["~/.bwai/extra-bin"]
}
```

`env_set` covers any variable the built-in bundle doesn't, and `path_prepend` adds directories ahead of `<state_root>/bin`. A leading `~` in `state_root`, `env_set` values, and `path_prepend` is expanded by bwai; bwrap expands neither `~` nor `$VAR`.

### Codex CLI

Launch Codex with `bwai --command codex`, or set `"command": ["codex"]` in the bwai config. Codex CLI can authenticate with an `OPENAI_API_KEY` (already passed through by the default environment allowlist), or with credentials stored by its login flow. For saved login, the default config exposes `~/.codex` read-write inside the sandbox. This includes Codex's local configuration, credentials, and session state, so only use it if you are comfortable making those files available to the agent. If your existing config has an explicit `home_allow` list that excludes `.codex`, add `.codex` to it for saved login and broker guidance; otherwise use API-key authentication.

Codex normally uses `~/.codex` as its home. To keep its files in bwai's persistent state area instead, set `CODEX_HOME` to a path under `state_root`, for example:

```json
{
  "env_set": { "CODEX_HOME": "~/.local/share/bwai/codex" }
}
```

When the broker is enabled, bwai overlays its generated broker guidance as `AGENTS.md` inside `CODEX_HOME` (or `~/.codex` when unset). Codex loads that as global instructions; the overlay does not change the host's Codex files or the project's own `AGENTS.md`. A custom `CODEX_HOME` must be inside the project, your home directory, or `state_root` so it exists within the sandbox. If that location is read-only under your config, bwai leaves it unchanged and reports that broker guidance was not mounted.

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

Sometimes an agent needs to run something that requires keys the sandbox deliberately hides — signing a commit needs `~/.gnupg`, `git push` over SSH needs `~/.ssh`, `gh` needs its token. The broker lets specific argv lists escape to the host with per-command rules.

Enable it by adding a `broker` block to `~/.config/bwai/bwai.json`:

```json
{
  "broker": {
    "enabled": true,
    "approval_timeout_s": 120,
    "rules": [
      { "match": ["gh", "pr", "view", "**"],   "action": "auto_allow" },
      { "match": ["gh", "pr", "create", "**"], "action": "confirm" }
    ]
  }
}
```

Each rule's `match` is an argv pattern. Tokens match literally except for two wildcards: `*` matches exactly one argv slot, and `**` matches zero or more *trailing* slots (last position only). `argv[0]` is always literal. Anything not matched by a rule is denied. See `docs/broker.md` for the full matcher rules and `bwai broker check <argv>...` to dry-run a request against your config.

Three actions:

- `auto_allow` — runs immediately on the host.
- `confirm` — runs only after explicit approval from a second terminal.
- `auto_deny` — explicit reject (use to carve exceptions out of broader rules in a later release).

Host commands run in an empty directory owned by the broker, never in the project. A repository in the agent's tree carries hooks and config (`.git/hooks/*`, `core.hooksPath`, `core.fsmonitor`, `core.sshCommand`, filter drivers) that any git the host command runs would execute with the host's credentials — `gh pr create` alone runs several git commands. The request cwd is still checked (after resolving symlinks) against the project and worktree roots, and wrappers that need it get it in `BWAI_REQUEST_CWD`. Arguments are checked too: one that names an existing file outside the project roots — the whole token, the value after `=`, or after a leading `@` — is refused, so an allowed `gh issue create --body-file ~/.git-credentials` cannot post a host secret. The consequences: relative paths do not resolve, `gh` needs `-R owner/repo` (and `--head` for `pr create`), and file contents go over stdin with `bwai-outside --stdin … -F - < body.md`. Rules that hand git a path into the tree anyway (`git -C`, `--git-dir`, `gh pr checkout`) reopen the hole; do not write them.

Inside the sandbox, the agent invokes `bwai-outside` instead of the bare command:

```sh
bwai-outside gh pr create -R org/repo --head my-branch --title "Fix" --body "…"
```

If the rule action is `confirm`, the sandbox sees a `waiting for host approval` message and the broker enqueues the request. From a second terminal on the host:

```sh
$ bwai approve
1 pending request:

[a7f3c0e1] sandbox wants to run on host
  cwd: /home/oscar/source/repos/foo
  cmd: gh pr create -R org/repo --head my-branch --title Fix --body …
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

The audit log lands at `~/.local/share/bwai-broker/broker.log` — a directory the sandbox sees as an empty tmpfs, so the agent can neither read nor rewrite it — as JSONL: timestamp, argv, cwd, matched rule, decision, exit code.

### Restricted commands (`git-safe`)

Some commands are too dangerous to expose under their own name. `git push` is the motivating case: no rule pattern can catch `git push origin +main` (force by refspec), `git push origin :main` (delete by refspec), or a `--force` placed after the refspec — tokens match literally and `**` is only valid as the final token.

The answer is a wrapper with a closed argument surface. `git-safe` is built from the same binary as `bwai` — bwai binds that one copy into the sandbox twice, so it appears beside `bwai-outside` under `/run/bwai/bin`. Nothing is installed on the host. It is called directly, with no `bwai-outside` prefix:

```sh
git-safe push            # current branch to origin
git-safe push backup     # current branch to the `backup` remote
```

In the sandbox `git-safe` is only a client: it forwards `["git-safe", …]` to the broker and decides nothing itself. The policy runs on the host, as the `bwai git-safe` subcommand the broker resolves the request to. That split is load-bearing — the agent can reach anything the sandbox can reach, so a check performed inside the sandbox would be advisory only.

`git-safe push` never runs git in the agent's repository. It reads the branch and the remote's push URL from the git dir as plain files, fetches the branch into a host-owned mirror (`~/.local/share/bwai-broker/mirrors/`, hidden from the sandbox by a tmpfs), and pushes from there. `git upload-pack` — the server half of that fetch — is the one git command designed to be safe against an untrusted repository, so the agent's hooks and config never run. A `.git` that is a symlink, a gitdir pointer out of the sandbox's writable roots, and object alternates are refused. On success the sandbox-side client updates `refs/remotes/<remote>/<branch>` and the upstream itself.

It pushes the current branch and refuses everything else: no flags, no refspecs, no detached HEAD, no protected branch, and no non-fast-forward update — it fetches the remote tip and requires it to be an ancestor of `HEAD` before pushing, so a destructive push is impossible by construction. The remote branch is created on the first push. It takes an optional remote name (default `origin`), but the name proves nothing: the sandbox owns `.git/config` and can retarget any remote, so the *push URL* is what gets authorised against `broker.push_allowed_urls`. The broker injects its own copy of that list into the host-side push, and a project-local `.bwai.json` only reaches that copy once you have trusted it, so editing the project tree cannot widen it. An empty list allows no push.

The protected set is the built-in `main`/`master`/`trunk`/`develop` plus any patterns in `broker.protected_branches`. A pattern is an exact branch name or a shell glob (`release-*`, `release/*`), and because the refspec is always `HEAD:refs/heads/<branch>`, protecting the branch you are on protects the destination branch on every remote.

```json
{
  "broker": {
    "push_allowed_urls": ["git@github.com:CubeB/RWE-B4.git"],
    "protected_branches": ["release-*"]
  }
}
```

Entries are remote URLs and branch patterns, and both sides are normalised before comparison, so `git@github.com:CubeB/RWE-B4.git` and `https://github.com/CubeB/RWE-B4` are the same URL entry.

That reduces the broker rules to:

```json
{ "match": ["git-safe", "push", "**"], "action": "auto_allow" },
{ "match": ["git-safe", "**"],         "action": "auto_deny" }
```

Note the action: **`auto_allow`, not `confirm`.** That is the payoff of moving the policy into the wrapper. A rule over raw `git push` needs a human because the pattern cannot express the check that matters; `git-safe` carries the check itself, so the ancestry decision is already made in code before git runs. A prompt would add nothing — an approver reading `git-safe push` learns only what the rule already told them — and an approval that is always granted is worse than none, because it trains the habit of approving without reading.

Treat `confirm` as the last resort rather than the cautious default. Every confirm rule stalls an unattended session on a human who may be asleep, so each one is a standing bug report on the rule set: it marks a judgement nobody has moved into code yet. Write the wrapper, then write `auto_allow`. The agent is told the same thing in its injected context — hunt the rule list for an `AUTO_ALLOW` path before issuing a command that lands on a `confirm`.

The rule needs a trailing `**` for the optional remote name, but the wrapper is what makes the tail safe: `push` accepts at most a bare remote name, so the pattern never has to enumerate the bad flags.

### Signed commits (`git-sign`)

The agent commits with plain `git commit`, and the commit is signed with the host's key. Inside the sandbox, bwai sets git's `gpg.program` and `gpg.ssh.program` to `bwai-gpg` (through `GIT_CONFIG_*` environment config, which outranks every config file). `bwai-gpg` forwards the buffer git wants signed to the broker as `["git-sign"]`; on the host, `bwai git-sign` checks that the buffer is a commit object and signs it with the key, format and program from the host's own git config. The key the sandbox's git names is ignored.

```json
{ "match": ["git-sign"], "action": "auto_allow" }
```

The host never runs `git` in the agent's repository to do this. That is the point: a repository the agent can write carries hooks and config — `.git/hooks/*`, `core.hooksPath`, `core.fsmonitor`, `gpg.program`, `core.sshCommand`, filter drivers — that git executes, so any host-side git command in that tree runs the agent's code with the host's credentials. Signing only a validated commit buffer keeps the host key from becoming a general-purpose signing oracle, but it does sign any commit the agent builds; treat the signature as "made on this machine", not "reviewed by me".

Verification (`git log --show-signature`, `git verify-commit`) is not available inside the sandbox.

### Agents running as another user (`bwai broker serve`)

For a machine dedicated to agents, `bwai broker serve` runs the broker as a long-lived daemon for agents that run as a separate Unix user (in their own rootless containers, with `sudo` inside if you like), gated by the connecting uid. See [docs/agent-box.md](docs/agent-box.md).

### Telling the agent it can call `bwai-outside`

The sandbox is a fresh world — agents need a way to discover `bwai-outside`. When the broker is enabled, `bwai` writes a fragment describing the tool, **including the current rule set rendered at broker startup**, and exposes it read-only under `/run/bwai/`. So the agent knows what it may and may not run before its first turn, without having to think to run `bwai-outside --list-rules` — that command remains for re-checking live. Claude Code and Command Code opt in with a flag or mod, and opencode and Codex load an isolated overlay at their global `AGENTS.md`; `bwai` does not modify the host's agent configuration.

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

**opencode** needs no opt-in. When the broker is enabled `bwai` overlays a merged `~/.config/opencode/AGENTS.md` inside the sandbox: your host global instructions followed by the bwai fragment. opencode v2 loads instructions only from `AGENTS.md` — it accepts the config `instructions` field but never resolves it — so the overlay is what actually reaches the model. The host file and the project tree are not touched.

Inside the sandbox, the agent (or you) can always check what's allowed:

```sh
bwai-outside --help          # usage + rule list
bwai-outside --list-rules    # just the rules, e.g. for piping to less
bwai-outside --check gh pr view 123   # dry-run: which rule would fire?
```

Rules print in config order, first-match wins, each row numbered `rules[N]` — the same index denials and `--check` cite, so "why was this denied?" is a one-line lookup instead of an exercise in re-deriving pattern precedence. Denied and pending messages name the rule that fired (`denied (rule) — rules[12] AUTO_DENY gh secret **`), and since patterns match argv token-by-token, the rule list footer spells out the classic trap: `gh issue -R org/repo create` does **not** match `gh issue create **` (its second token is `-R`, not `create`) — pass the flag to land on the narrow rule.

See `docs/broker.md` for the full design.
