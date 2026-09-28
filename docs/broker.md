# Host-execution broker

A mechanism for sandboxed agents to request execution of specific commands
on the host with user approval. Motivating use case: signing git commits
when the sandbox blocks `~/.gnupg`.

> **Status:** first-PR slice landed. Tracking checklist at the bottom of
> the doc lists what's implemented vs what's still pending.

## Problem

`bwai` runs an agent inside a `bwrap` sandbox with no access to `~/.gnupg`,
`~/.ssh`, etc. This is deliberate — the agent must not be able to read or
sign with those keys. The cost: legitimate operations like
`git commit -S` fail.

Users who run the agent with `--dangerously-skip-permissions` inside the
sandbox have no in-agent prompt to escalate from. The gate has to live at
the sandbox boundary.

## Goals

- Allow specific, user-defined commands to escape the sandbox and run on
  the host with full host env (`gpg-agent`, `ssh-agent`, etc.).
- Per-command approval, configurable per rule.
- No shell evaluation of agent-supplied strings.
- Defaults that deny by omission.
- Don't corrupt the agent's TUI when prompting.

## Architecture

```
┌─ host: bwai parent process ──────────────────────────┐
│  ├─ broker: listens on broker.sock                   │
│  ├─ approver: listens on approve.sock                │
│  ├─ pending-queue: map req_id → request              │
│  ├─ exec bwrap with bind mount + env                 │
│  └─ embedded bwai-outside helper in tmpdir           │
└──────────────────────────────────────────────────────┘
                       │ bind: /tmp/bwai-$PID → /run/bwai
                       ▼
┌─ sandbox ────────────────────────────────────────────┐
│  agent ─► `bwai-outside git commit -m ...`           │
│            └─ connects to broker.sock                │
└──────────────────────────────────────────────────────┘
```

### Tmpdir layout

`/tmp/bwai-$PID/` (mode 0700) contains:

- `broker.sock` (mode 0600) — sandbox-facing. Bind-mounted into the
  sandbox at `/run/bwai/broker.sock`.
- `approve.sock` (mode 0600) — host-only. **Not** bind-mounted into the
  sandbox. Used by `bwai approve` from a second terminal or a tmux popup.
- `bin/bwai-outside` — symlink (or copy) of the running `bwai` binary,
  bind-mounted into the sandbox so the agent can invoke it.

The sandbox env is extended with:

- `BWAI_BROKER_SOCKET=/run/bwai/broker.sock`
- `PATH=$PATH:/run/bwai/bin`

`bwai-outside` is `bwai` itself, dispatched on argv[0]. No separate
binary to ship.

## Wire protocol

Newline-delimited JSON in both directions over the Unix socket. One
request per connection, then a stream of reply frames until the broker
closes the connection.

> **Implementation note:** an earlier draft of this doc specified
> length-prefixed requests with NDJSON replies. Implementation
> unified on NDJSON both ways — simpler framing, easier to debug
> with `socat` / `nc`, and the `data` fields are JSON-escaped strings
> so embedded newlines survive.

### Sandbox → host (`broker.sock`)

```json
{
  "v": 1,
  "argv": ["git", "commit", "-S", "-m", "fix bug"],
  "cwd": "/home/oscar/source/repos/foo",
  "stdin_inherit": false
}
```

- `argv` — exact argv to execute on the host. `argv[0]` is resolved
  against the host's `PATH`.
- `cwd` — must resolve inside the project bind mount. Reject otherwise.
- `stdin_inherit` — reserved for future pty passthrough. MVP: always
  false.
- `stdin` — optional bytes (base64 in JSON) fed to the host command's
  stdin, which is then closed. A request, stdin included, is capped at
  4 MiB.

### Host → sandbox reply frames

```json
{"type": "pending", "id": "a7f3"}
{"type": "stdout",  "data": "..."}
{"type": "stderr",  "data": "..."}
{"type": "exit",    "code": 0}
{"type": "denied",  "reason": "rule" | "user" | "timeout" | "ratelimit"}
```

`pending` is emitted as soon as the request is queued for human approval
so the agent sees a "waiting on user" signal instead of silence. `auto_allow`
rules skip straight to `stdout`/`stderr`/`exit`.

Output streams incrementally: each read from the child's stdout/stderr
pipes becomes one frame, so a long-running command shows output to the
sandbox client as it goes rather than waiting for exit. stdout and
stderr frames can interleave at chunk boundaries — matches real
terminal behaviour.

## Approval flow

When a request arrives at the broker:

1. **Match rules** in order. First match wins.
2. **`auto_deny` (explicit or implicit)** → emit `denied: rule`, close.
3. **`auto_allow`** → run on host immediately, stream reply.
4. **`confirm`** → enqueue as pending, emit `{"type":"pending","id":...}`,
   invoke the first available approver from the stack.

### Approver stack

| Mode | Trigger | UX |
|---|---|---|
| `tmux` | `$TMUX` is set | `tmux display-popup -E -- bwai approve --id $id`. Popup overlays the user's tmux session; the agent's pane is untouched. (Not yet wired.) |
| `web` | `"web"` in `broker.prompt` and a session D-Bus bus is reachable | Rich desktop notification with **Approve** / **Deny** / **Open page** buttons. Approve/Deny resolve straight from the toast (trusted — the session bus is host-only). **Open** (or clicking the toast body) launches a token-protected loopback web page with the full request view and the *always-this-session* option. See [Web approval](#web-approval). |
| `gui` | `$DISPLAY` or `$WAYLAND_DISPLAY` set, `zenity` on `$PATH` | `zenity --question --text=...` (Not yet wired; the `web` mode covers the graphical case via D-Bus.) |
| `oob` | always available | stderr notice + `notify-send`. User runs `bwai approve` in a second terminal, which connects to `approve.sock` and walks them through pending requests. |

First mode whose trigger is satisfied wins. Configurable via
`broker.prompt`: an ordered list, e.g. `["web", "oob"]`. Users can
restrict the stack (e.g. `["oob"]` to force out-of-band). The default
is `["oob"]`, so `web` is strictly opt-in. In every case `bwai approve`
on `approve.sock` remains available — `web` and `oob` only add a
shortcut, they never replace the CLI, which is the only headless/SSH
path.

### Web approval

When `"web"` is in `broker.prompt`, the broker starts a loopback HTTP
server (`broker.web.addr`, default `127.0.0.1:0` — an ephemeral
per-instance port) and, if a session D-Bus bus is reachable, posts a
rich notification when a confirm request lands. The notification carries
action buttons:

- **Approve** / **Deny** — resolve the request directly from the toast.
- **Open page** (and clicking the toast body) — open
  `http://<addr>/r/<id>?k=<token>` in the browser. The page shows the
  command, project, cwd, and age, with Approve / Deny / **Always this
  session** buttons that `POST` the decision back.

If no session bus is reachable (headless / SSH), the broker degrades to
the `oob` `notify-send` nudge (if `"oob"` is also in the stack) and
`bwai approve` still works.

#### Why two trust models

The sandbox is launched **without** `--unshare-net`, so it shares the
host network namespace and can reach any `127.0.0.1:PORT` the broker
binds — including the approval server. Two boundaries keep the agent
from self-approving:

- **D-Bus session bus is host-only.** `DBUS_SESSION_BUS_ADDRESS` is not
  in `env_allow`, and `/run` is a tmpfs *inside* the sandbox, so the bus
  socket is unreachable from the sandbox. Toast `ActionInvoked` events
  are therefore **trusted** and resolve with no token.
- **HTTP loopback is sandbox-reachable**, so every web mutation is
  gated on a **single-use, 128-bit per-request token** (constant-time
  compared). The token travels *only* in the URL embedded in the
  host-side notification — it never enters the sandbox. Mutations are
  `POST`-only so a link prefetch or `GET` can't approve anything, and
  the bind address is validated to be loopback (config load fails
  otherwise — defence in depth against exposing approvals to the LAN).

The token is single-use: `resolve()` is `sync.Once`-guarded and the
pending entry is deleted once the request returns, so a replayed `POST`
gets a `404`. Knowing a request id (8 hex chars) is useless without the
128-bit token.

All approvers connect to `approve.sock` — the popup, the GUI dialog
wrapper, and the manual `bwai approve` invocation are all just clients of
the same approval API. No special-casing per mode.

### Approval timeout

Default 120s, configurable via `broker.approval_timeout_s`. Times out to
`denied: timeout`. Generous default because the out-of-band path needs
walking-to-keyboard time.

### `bwai approve` CLI

```
$ bwai approve
1 pending request:

[a7f3] sandbox wants to run on host
  cwd:  /home/oscar/source/repos/foo
  cmd:  git commit -S -m "fix bug"
  age:  4s

[y]es / [n]o / [a]lways-this-session / [s]kip
> y
approved.
```

`--id <id>` operates on one specific request (tmux popup uses this).
Without `--id`, iterates the pending queue.

`always-this-session` adds the exact argv to an in-memory auto-allow list
for the lifetime of the `bwai` process. **Never persisted** to disk —
persisting it would erode the explicit-config security model.

## Allowlist

```json
{
  "broker": {
    "enabled": true,
    "prompt": ["tmux", "gui", "oob"],
    "approval_timeout_s": 120,
    "rules": [
      { "match": ["git", "push", "--force", "**"], "action": "auto_deny" },
      { "match": ["git", "push", "**"],            "action": "confirm" },
      { "match": ["git", "commit", "**"],          "action": "confirm" },
      { "match": ["git", "tag", "-s", "**"],       "action": "confirm" },

      { "match": ["git", "status"],                "action": "auto_allow" },
      { "match": ["git", "log", "**"],             "action": "auto_allow" },
      { "match": ["git", "diff", "**"],            "action": "auto_allow" },
      { "match": ["ssh-add", "-l"],                "action": "auto_allow" }
    ]
  }
}
```

### Three actions

- `auto_allow` — runs immediately on the host, no prompt.
- `confirm` — runs only after user approval via the approver stack.
- `auto_deny` — explicit reject, no prompt. Used to carve exceptions out
  of broader rules.

**No-match is implicit `auto_deny`.** There are only two human-interaction
states: `confirm` or nothing. Every rule action just decides whether the
automatic decision is allow or deny.

### Pattern matching

| Pattern | Matches |
|---|---|
| `["git", "status"]` | exactly `git status`, no extra args |
| `["git", "commit", "**"]` | `git commit` with any args after |
| `["git", "**"]` | any `git` invocation (including `git push --force`) |
| `["git", "*", "--help"]` | `git <anything> --help`, exactly 3 args |

Rules:

- Tokens match literally unless they are `*` (one arg, any value) or `**`
  (zero or more, valid only as the last token).
- **`argv[0]` is always literal** — no wildcards in the command-name
  slot. You can never write "auto-allow any command."
- Patterns match against the resolved `argv` array. No shell parsing.

### Common scopes

| Intent | Rule |
|---|---|
| All `git` commands | `{ "match": ["git", "**"], "action": "auto_allow" }` |
| Just `git commit` (any args) | `{ "match": ["git", "commit", "**"], "action": "confirm" }` |
| Read-only `git` only | enumerate: `status`, `log`, `diff`, `show`, `branch -l`, etc. |
| Allow `git` except `push` | `confirm` rule for `["git","push","**"]` *above* an `auto_allow` rule for `["git","**"]` |

### Dry-run helper

```
$ bwai broker check git push --force main
matched: rules[0]  { "match": ["git","push","--force","**"], "action": "auto_deny" }
result:  AUTO_DENY
```

Cheap to implement and the only reliable way to audit a non-trivial rule
set without spelunking the JSON by hand.

The same verdict is available from *inside* the sandbox, where the
config file is not mounted, via a `check` op over broker.sock:

```
$ bwai-outside --check gh issue -R org/repo create "title"
matched: rules[1]  { "match": ["gh","issue","-R","org/repo","create","**"], "action": "auto_allow" }
result:  AUTO_ALLOW
```

`--check` executes nothing and consults no cwd — matching is a pure
function of the rule list and argv — but it does apply the
"always-this-session" promotion, so it answers what a real exec would
do, not what the static config says. This is the agent's first stop
when unsure which rule a spelling would hit: patterns match argv
token-by-token, so a flag anywhere shifts every later token
(`gh issue -R org/repo create` does **not** match
`["gh","issue","create","**"]` — its second token is `-R`), and the
agent should not have to re-derive first-match precedence by hand to
discover that.

For the same reason, pending and denied frames name the rule that fired
(`matched: {idx, action, rule}` on the wire, rendered as
`denied (rule) — rules[12] AUTO_DENY gh secret **` in the client), so a
denial is self-explaining instead of a nudge to go read the list.
`--list-rules` numbers each row with the same `rules[N]` index so the
three views — list, denial, dry run — share one coordinate system.

## Security model

- **Rules are the hard gate.** Approval is a second factor; you can
  never approve something not in the rules. Defends against
  approval-fatigue attacks where the agent spams requests.
- **No shell.** `argv` goes straight to `os/exec`. No quoting, no
  injection paths.
- **Host env, not sandbox env.** Commands run with the *host's* env
  (`$SSH_AUTH_SOCK`, `$GPG_TTY`, etc.), not whatever the sandbox passes.
  Sandbox can't smuggle in `LD_PRELOAD` or hostile `PATH`.
- **cwd confined** to the project bind mount, checked after resolving
  symlinks, and **never used as the working directory**. Host commands
  run in an empty directory under the broker tmpdir; the resolved request
  cwd reaches wrappers as `BWAI_REQUEST_CWD`. A repository in the agent's
  tree is agent-controlled code — hooks, `core.fsmonitor`,
  `core.sshCommand`, filter drivers — and any git a host command runs
  there executes it with host credentials. Rules that hand git a path
  into the tree (`git -C`, `--git-dir`, `gh pr checkout`) reopen that.
- **Rate limit.** Max 1 confirm prompt per 2s, 30 confirms per session.
  Excess requests get `denied: ratelimit`. `auto_allow` is not rate
  limited.
- **Sockets.** Tmpdir 0700; both sockets 0600; owned by the invoking
  user.
- **Audit log** at `~/.local/state/bwai/broker.log`. Append-only JSONL:
  timestamp, request id, argv, cwd, matched rule, decision, exit code.

### Defaults

`bwai --dump-config` ships with `rules: []`. Empty list = everything
denied. README contains copy-paste fragments for common scopes
("read-only git", "sign and push", "ssh-agent introspection") so users
assemble from known-good pieces rather than writing from scratch.

## Restricted commands (`git-safe`)

The matcher is deliberately tiny, and that is most visible around `git
push`. The dangerous spellings are not enumerable as patterns:

| Form | Why a rule can't catch it |
|---|---|
| `git push origin +main` | force-by-refspec; the `+` is inside a token, and tokens match literally |
| `git push origin :main` | delete-by-empty-refspec; same problem with `:` |
| `git push origin main --force` | `**` is only valid as the *final* token, so "flag anywhere" is unwritable |
| `git push --force-with-lease=origin/main` | `=`-valued; a token distinct from `--force-with-lease` |

Enumerating the known-bad forms reduces approval fatigue, but it is not a
guarantee. So the commands that need a real judgement call live behind a
**wrapper** instead of a pattern, and the broker authorizes a short,
literal argv:

```json
{ "match": ["git-safe", "push", "**"], "action": "auto_allow" },
{ "match": ["git-safe", "**"],         "action": "auto_deny" }
```

`git-safe` is an argv[0] persona of the `bwai` binary, like
`bwai-outside`. bwai binds the same copy of that binary into the sandbox
twice — `/run/bwai/bin/bwai-outside` and `/run/bwai/bin/git-safe` — so
neither has to exist on the host PATH. Inside the sandbox the agent calls
`git-safe push` directly.

The name means two different halves depending on where it runs. In the
sandbox it is a **client**: it forwards `["git-safe", …]` and decides
nothing. The **policy** runs on the host as the `bwai git-safe …`
subcommand, which the broker resolves the request to (`hostArgv` in
`broker.go`). That split is load-bearing rather than incidental — the
agent can reach everything the sandbox can reach, so a check performed
inside the sandbox would be advisory only. The rules still match the
original argv, so the host-side mapping can never name a command the rules
did not authorize.

`git-safe push` is closed over its arguments — no flags, no refspecs, at
most one remote name — and enforces the policy in code:

- never runs git in the agent's repository: it reads HEAD, the
  gitdir pointer, `commondir` and the remote's URL (`git config --file`,
  no includes) as files, fetches the branch into a host-owned bare mirror
  under `~/.local/share/bwai-broker/mirrors/` — upload-pack is designed to
  be safe against an untrusted repository, and `transfer.fsckObjects` is on
  — and pushes from the mirror. The mirror's directory is a tmpfs inside
  the sandbox, since its hooks would run with host credentials;
- refuses a `.git` symlink, a git dir outside the broker's writable roots
  (`BWAI_ALLOWED_ROOTS`), and object alternates;
- refuses a detached HEAD;
- refuses the protected branches — the built-in `main`, `master`, `trunk`,
  `develop` plus any patterns in `broker.protected_branches` (exact names or
  shell globs such as `release-*`, `release/*`);
- pushes only the current branch;
- authorizes the **push URL** of the chosen remote against
  `broker.push_allowed_urls` (default remote `origin`), because the sandbox
  owns `.git/config` and can retarget any remote — the remote name is not a
  boundary;
- fetches the remote tip and requires it to be an **ancestor of HEAD**
  before pushing, so a non-fast-forward update cannot happen;
- constructs the refspec itself (`<sha>:refs/heads/<branch>`, no `+`
  prefix, never `--force`), pushing to the allowlist entry's own spelling
  and creating the remote branch on first push;
- leaves the agent's repository alone afterwards: the sandbox-side client
  updates the remote-tracking ref and upstream with the sandbox's git.

The allowlist and the protected-branch list are trust anchors, not ordinary
settings: the broker snapshots both at session start and injects them into
the host-side push (as `BWAI_PUSH_ALLOWED` and `BWAI_PROTECTED_BRANCHES`), so
a mid-session edit to the project tree — including the `.bwai.json` they may
have been read from — cannot widen them. An empty URL list allows no push;
the built-in protected branches are always in force. URL entries are
normalised before comparison, so `git@github.com:o/r.git` and
`https://github.com/o/r` are the same entry.

The ancestry check is the part a deny-list cannot express: it makes a
destructive push impossible by construction — a property of the commit
graph — rather than by blacklisting flags.

The rule carries a trailing `**` for the optional remote name. That is the
one loose spot in the pattern, and the wrapper is what keeps it meaningful:
the tail can only ever be a bare remote name.

### Signing (`git-sign`)

An earlier `git-safe commit` ran `git commit -S` on the host, in the
agent's working tree. That was a sandbox escape: the tree is writable
from the sandbox, and git executes what the repository tells it to —
`.git/hooks/pre-commit`, `core.hooksPath`, `core.fsmonitor`,
`gpg.program`, filter drivers — so the agent could plant a hook and have
the auto-allowed commit run it on the host with `gpg-agent` and
`ssh-agent` in reach. No flag list closes that; the only fix is for the
host not to run git in a tree the agent controls.

So signing is the only part that crosses the boundary. Inside the
sandbox, bwai sets `gpg.program` and `gpg.ssh.program` to the `bwai-gpg`
persona through `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n` (environment config
outranks every config file). The agent runs ordinary `git commit`; git
hands the commit buffer to `bwai-gpg`, which forwards it as
`["git-sign"]` with the buffer on `stdin`. On the host, `bwai git-sign`:

- refuses anything but a commit object (`tree` first, then only
  `parent`/`author`/`committer`/`encoding`/`mergetag` headers), so the
  host key is not a general signing oracle — no tags, no arbitrary data;
- takes no arguments, and resolves format, key and program from the
  host's own git config (read from `/`, so no repository config applies)
  — the key the sandbox's git asked for is ignored;
- answers in the shape git expects: an armored signature on stdout and
  the gpg status line on stderr for OpenPGP, the `.sig` contents for SSH
  (the shim writes them to `<file>.sig`, as `ssh-keygen -Y sign` would).

```json
{ "match": ["git-sign"], "action": "auto_allow" }
```

The agent can still get any commit it builds signed; the signature means
"made on this machine", not "reviewed". Verification is not forwarded.

The rules above are `auto_allow`, and that is the point of the exercise.
`confirm` is a last resort, not a safe default: it is what you reach for
only when a pattern cannot express the check that matters and a human has
to stand in for the missing judgement. Once that judgement lives in the
wrapper, the prompt adds no information — an approver looking at `git-safe
push` learns only what the rule already told them — and approval that is
always granted is worse than no approval, because it trains the habit of
approving without reading. So a `confirm` rule is a standing bug report on
the rule set: it says nobody has written the wrapper yet. Before adding
one, ask what closed wrapper would let it be `auto_allow` instead; the
wrapper earns the stronger action by making the decision the human would
otherwise have made.

The same applies to the agent at runtime, which is why the injected
context tells it to hunt the rule list for an `AUTO_ALLOW` path before
issuing anything that lands on a `confirm`. An unattended session that
stalls on an approval nobody is awake to give has failed, and it usually
failed one line above the rule it matched.

**Keep the wrapper closed.** The guarantee holds only while `git-safe`
refuses to forward arbitrary arguments to `git`. If it ever grows a
passthrough (`git "$@"`), the broker rule stops meaning anything.

The same reasoning applies to `gh`: its subcommands are structured enough
to match literally, but `gh api` is a universal escape hatch (it can
force-update a ref over REST) and `gh repo sync --force` hard-resets a
branch. Deny those explicitly rather than leaning on a broad `gh **` rule.

```
$ bwai broker check git-safe push     # AUTO_ALLOW, rules[0]
$ bwai broker check git push --force  # AUTO_DENY (implicit — no rule)
```

## Daemon mode (`bwai broker serve`)

The same broker can run long-lived, for agents that run as a separate
Unix user rather than in a bwai sandbox. It listens on
`broker.serve.socket`, accepts only the uids in
`broker.serve.allowed_uids` (`SO_PEERCRED`), takes `broker.serve.roots`
as the allowed request cwds, and counts the confirm cap over a sliding
hour. `bwai-outside --context` fetches the agent guidance over the
socket, since nothing injects it. Setup, and why the boundary has to be
a UID rather than a container: [agent-box.md](agent-box.md).

The per-sandbox broker checks peer credentials too, allowing only its own
uid.

## Why not bash-style job control?

The natural instinct is to model this on bash putting jobs in the
background: stop the agent, `tcsetpgrp` back to bwai, prompt on the TTY,
`tcsetpgrp` back, resume. This works mechanically but corrupts the
agent's screen:

- The agent (a TUI) owns the alt-screen buffer. Stopping it leaves its
  rendering in place; writing a prompt over the top is visual garbage.
- Exiting alt-screen mode (`\e[?1049l`), prompting, and re-entering
  works in some terminals but not all — alt-screen content preservation
  across re-entry isn't universally implemented.
- `SIGCONT` doesn't reliably trigger a TUI redraw without an
  accompanying `SIGWINCH`, and the cursor-position model the agent
  thinks it has is now wrong.

Bash's job-control elegance works because backgrounded jobs in bash's
worldview are line-oriented, not screen-holding. The tmux popup / GUI
dialog / out-of-band approaches sidestep the problem entirely by not
sharing a screen with the agent.

## Open questions

1. **Stdin/tty passthrough.** Needed for `git commit` with no `-m`, or
   `gpg --edit-key`. Requires the broker to allocate a pty and proxy it.
   Significant complexity. Punt to a follow-up; require `-m` /
   non-interactive usage for MVP.
2. **Output streaming.** Buffered MVP is fine for `git commit`. Add
   streaming when someone tries `git push` over a slow link.
3. **`always-this-session` granularity.** Currently exact-argv match
   (this is what shipped). Allow widening to the matched rule pattern?
   Risk: user accidentally blanket-approves a category they only meant
   to approve once.
4. **Multiple concurrent requests.** Rate limiting keeps the queue
   small. UX for n>1 in `bwai approve` (paginate? batch-approve?) is
   secondary.

## First-PR slice

Aim for the smallest end-to-end thing that proves the design:

- [x] `broker.sock` + `approve.sock`
- [x] `bwai-outside` argv[0] dispatch (sandbox-side client)
- [x] `bwai approve` subcommand (host-side approver client)
- [x] Out-of-band approver only (no tmux, no zenity)
- [x] Audit log
- [x] `always-this-session` (in-memory, never persisted — landed alongside the slice because the wire protocol already needed the decision tag)
- [x] Glob patterns (`*` and `**`). `argv[0]` is always literal; `**` is only valid as the final token.
- [x] `bwai broker check` dry-run
- [x] Output streaming
- [x] `oob` desktop notification — `notify-send` nudge when a confirm request becomes pending (gated on `"oob"` in `broker.prompt`; best-effort, no-ops if `notify-send` is absent)
- [x] `web` approver — rich D-Bus notification (Approve/Deny/Open buttons) plus a token-protected loopback web approval page (gated on `"web"` in `broker.prompt`; degrades to `oob` when no session bus is reachable). First third-party dependency: `github.com/godbus/dbus/v5`.
- [x] `git-safe` wrapper — closed-argument `git push` (current branch only, fast-forward only, never a protected branch) and `git commit` (`-m` only, always `-S`, never amend/skip-hooks/pathspec), so the broker authorizes a literal argv instead of an unenumerable flag blacklist. argv[0] persona of the `bwai` binary, bind-mounted into the sandbox as `git-safe` and resolved on the host by the broker, so it is never installed on the host PATH.

Follow-ups, in roughly that order:

- [ ] tmux `display-popup` approver — `broker.prompt` is parsed; `oob` (notify-send) and `web` (D-Bus + web page) modes are honored, but `tmux` is not yet wired
- [ ] zenity / kdialog approver — largely subsumed by the `web` mode's D-Bus toast for graphical sessions; a synchronous dialog fallback is still open
- [ ] Pty passthrough

### Implementation decisions worth recording

- **Outside-client exit codes.** `bwai-outside` returns `126` on `denied`
  and `127` on transport errors (cannot connect, decode failure). Shell
  convention; not part of the wire protocol.
- **Helper installation.** `/tmp/bwai-$PID/bin/bwai-outside` is a full
  copy of the running `bwai` binary, not a symlink. `bwrap --ro-bind`
  resolves symlinks on the host side, which would leak the host path
  into the sandbox view; a real file inside the tmpdir bind-mounts
  cleanly.
- **Multi-instance discovery.** Two concurrent `bwai` instances for the
  same user produce two tmpdirs (`/tmp/bwai-$PID1/`, `/tmp/bwai-$PID2/`).
  `bwai approve` picks the newest by mtime; users who need precision
  pass `--socket`. Good enough for MVP.
- **Approval timeout race.** If the approver decision and the timeout
  fire simultaneously, the `sync.Once`-guarded `resolve()` ensures the
  buffered decision channel is written exactly once; the broker reads
  back whichever value won the race rather than always reporting
  `timeout`.
