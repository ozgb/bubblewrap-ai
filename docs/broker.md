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

MVP: stdout/stderr buffered and sent at the end. Streaming can land later
without protocol changes — frames are already chunked.

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
| `tmux` | `$TMUX` is set | `tmux display-popup -E -- bwai approve --id $id`. Popup overlays the user's tmux session; the agent's pane is untouched. |
| `gui` | `$DISPLAY` or `$WAYLAND_DISPLAY` set, `zenity` on `$PATH` | `zenity --question --text=...` |
| `oob` | always available | stderr notice + `notify-send`. User runs `bwai approve` in a second terminal, which connects to `approve.sock` and walks them through pending requests. |

First mode whose trigger is satisfied wins. Configurable via
`broker.prompt`: an ordered list, e.g. `["tmux", "gui", "oob"]`. Users can
restrict the stack (e.g. `["oob"]` to force out-of-band).

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

## Security model

- **Rules are the hard gate.** Approval is a second factor; you can
  never approve something not in the rules. Defends against
  approval-fatigue attacks where the agent spams requests.
- **No shell.** `argv` goes straight to `os/exec`. No quoting, no
  injection paths.
- **Host env, not sandbox env.** Commands run with the *host's* env
  (`$SSH_AUTH_SOCK`, `$GPG_TTY`, etc.), not whatever the sandbox passes.
  Sandbox can't smuggle in `LD_PRELOAD` or hostile `PATH`.
- **cwd confined** to the project bind mount.
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
- [x] Literal-argv rules only (no `*` / `**` yet)
- [x] Out-of-band approver only (no tmux, no zenity)
- [x] Buffered output, no streaming
- [x] Audit log
- [x] `always-this-session` (in-memory, never persisted — landed alongside the slice because the wire protocol already needed the decision tag)

Follow-ups, in roughly that order:

- [ ] Glob patterns (`*` and `**`) — currently rejected at config load with a friendly error so a typo doesn't silently fall through to implicit deny
- [ ] `bwai broker check` dry-run
- [ ] tmux `display-popup` approver — `broker.prompt` is parsed but only `"oob"` is honored
- [ ] zenity / kdialog approver
- [ ] Output streaming
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
