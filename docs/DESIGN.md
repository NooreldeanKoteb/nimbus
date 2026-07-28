# Nimbus — Design Document

**Status:** Draft v0.1
**Date:** 2026-07-26

A wrapper around Claude Code that turns a collection of devices into a single
autonomous engineering team. Sessions, memory, config, and running work follow
you across machines; each device is addressable as a peer agent that can be
handed tasks and can hand tasks back.

Nimbus serves two use cases that share almost all their machinery:

- **Personal mesh** — one owner, several devices. Write the client on the
  laptop, continue the server on the desktop, same task.
- **Managed setup and support** — one operator, many devices. Install one app,
  set up a fleet from a central location, debug a machine you cannot see.

The second is why the audit trail (§8a) is load-bearing rather than hygiene: the
person reading the log is often not the person who ran the commands. Consent
gating and scoped grants are deliberately **not** implemented — the current goal
is fully autonomous setup. They hook into the audit layer when wanted, because
every gated action is already an audited one.

---

## 1. Problem

Claude Code today is per-machine. Conversations, memory, MCP servers, skills,
and settings live in `~/.claude` on one box. Moving to another device means
starting cold. There is no way for a session on one machine to talk to a session
on another, no awareness of what hardware or OS it is running on, and no
survival across reboots.

Concretely, the workflows that fail today:

- Write a client on the laptop, continue the server on the desktop, same task.
- Debug a client/server bug where the two halves live on different machines.
- Fix an OS issue that requires a reboot, and have the work resume afterward.
- Sit down at a fresh machine and be productive in one command.

## 2. Non-goals

**Nimbus does not reimplement Claude Code, and does not reimplement session
storage.** Claude Code owns sessions, transcripts, and the `--session-id` /
`--resume` machinery that operates on them (§7a). Nimbus assigns and tracks
those ids so a conversation can be tied to a task and a device; it does not
store or move the conversations themselves.

Also out of scope:

- A hosted multi-tenant service. Nimbus is single-user, your devices, your git remote.
- A new agent framework. Peers are Claude Code processes; the mesh moves tasks between them.
- Replacing MCP. The mesh *is* an MCP server — that is how Claude reaches it.

## 2a. Zero-dependency constraint

**Hard requirement: a fresh device needs nothing preinstalled.** No Node, no
Python, no git binary, no Tailscale client, no package manager assumptions. The
user downloads one file, logs into their git provider, and everything else is
installed for them.

This forces a single statically linked binary — **Go, `CGO_ENABLED=0`**,
cross-compiled to `{linux,darwin,windows} × {amd64,arm64}`. Every external
dependency in §4–§11 is replaced by an embedded Go library:

| Would have been | Embedded instead | Result |
|---|---|---|
| Tailscale client | `tailscale.com/tsnet` | Full tailnet node inside `nimbusd` — userspace TCP/IP via gVisor, no root, no daemon to install |
| `sops` + `age` binaries | `filippo.io/age` | Encryption/decryption in-process |
| `git` binary | `go-git` (shell out to system git when present, as a fast path) | Clone/commit/push with no git installed |
| `gh` CLI | GitHub device flow over plain HTTPS | `nimbus login` prints a code, user enters it in a browser. No CLI, no PAT paste |
| `tmux` | Go PTY + process supervision in `nimbusd` | Detached sessions survive without a terminal multiplexer |

`tsnet` is the load-bearing one. It runs a self-contained Tailscale node inside
the process — which means the mesh in §6 needs no VPN install, no root, and no
port-forwarding on any device.

### The bootstrap chain

Note the ordering constraint: **Nimbus installs Claude Code, not the reverse.**
Claude cannot install its own prerequisites before it exists. So there are two
tiers:

- **Tier 0 — Nimbus installs, deterministically.** Claude Code itself (via the
  native installer, `curl -fsSL https://claude.ai/install.sh | bash`, which has
  no Node dependency), the state repo, secrets, config symlinks, MCP servers
  from `mcp.manifest.toml`.
- **Tier 1 — Claude installs, adaptively.** Project toolchains, language
  runtimes, databases, anything `nimbus doctor` flagged as missing for the task
  at hand. This is where "have Claude install the requirements" belongs, because
  it needs judgment about the specific project.

**Dependencies of dependencies count.** Claude Code's own installer is a bash
script that requires `curl` or `wget` and `unzip` — none of which exist on a
bare Ubuntu image. A zero-dependency promise is only real if nimbus resolves
its dependencies' dependencies too, so Tier 0 installs those first via the
detected package manager (§9a). This was found by testing on an actually empty
container rather than a developer machine, which is the only way it surfaces.

```
$ curl -fsSL <installer> | sh        # one static binary, no deps (unpublished)
$ nimbus login                       # device flow → git provider
$ nimbus init --new personal         # creates the state repo, then sets the device up
$ nimbus resume                      # continue where you left off
```

`--new <name>` provisions the private state repo through the provider API using
the token from `nimbus login`, so first-run setup never requires visiting a
browser to make a repository by hand. Later devices need no URL at all — see
§4b.

Four commands, one of them a browser login. Auth is pluggable behind a
`Provider` interface (GitHub first, then GitLab, then non-git backends) so
"maybe another account in the future" does not require rearchitecting.

## 3. Core concepts

| Concept | Definition |
|---|---|
| **Node** | One device running `nimbusd`. Has a stable id, a capability profile, and a tailnet address. |
| **State repo** | A private git repo (`~/.nimbus`) holding memory, config, device profiles, and task state. The single source of truth. |
| **Task** | A durable unit of work: goal, plan, progress, artifacts, device affinity, resume contract. Survives sessions, devices, and reboots. |
| **Mesh** | The peer network. Any node can dispatch work to any other node and receive results. |
| **Autonomy level** | Per-node, per-task ceiling on what Claude may do unattended (§12). |

## 4. Topology

```
        ┌──────────────────── git remote (private) ────────────────────┐
        │              memory · config · tasks · device profiles        │
        └───────┬──────────────────────────────────┬────────────────────┘
                │ sync                             │ sync
        ┌───────┴────────┐                 ┌───────┴────────┐
        │  node: desktop │◄── tailnet ────►│  node: laptop  │
        │  ┌──────────┐  │   mTLS/WG       │  ┌──────────┐  │
        │  │ nimbusd  │  │                 │  │ nimbusd  │  │
        │  └────┬─────┘  │                 │  └────┬─────┘  │
        │   MCP │        │                 │   MCP │        │
        │  ┌────┴─────┐  │                 │  ┌────┴─────┐  │
        │  │  claude  │  │                 │  │  claude  │  │
        │  └──────────┘  │                 │  └──────────┘  │
        └────────────────┘                 └────────────────┘
```

Two independent channels, deliberately:

- **Git** carries durable state. Slow, auditable, works offline, survives everything.
- **Tailnet** carries live coordination. Fast, ephemeral, requires both nodes up.

Nothing critical depends on the tailnet. If it is down, nodes degrade to
git-as-transport (poll a `bus/` directory) with worse latency but no data loss.

The tailnet node is embedded in `nimbusd` via `tsnet` (§2a) — there is no VPN
client to install and no privileged daemon on any device.

### State repo layout

```
~/.nimbus/
├── memory/
│   ├── long-term/<id>.md      # curated durable facts, one file each
│   ├── scratch/<node>/<id>.md # per-device working state, TTL'd, never conflicts
│   └── journal/<node>.jsonl   # per-device log of what was added, kept, dropped
├── config/
│   ├── settings.json       # symlinked to ~/.claude/settings.json
│   ├── mcp.manifest.toml   # declarative MCP server list + install method
│   ├── skills/ agents/ commands/
│   └── CLAUDE.md
├── nodes/
│   └── <node-id>.json      # OS, arch, tools, hardware, capabilities, health
├── audit/
│   └── <node-id>.jsonl     # hash-chained action log, one per device
├── tasks/
│   └── <task-id>/
│       ├── task.json           # goal, repo, branch, needs, claim
│       ├── progress/<node>.jsonl  # append-only timeline, one shard per device
│       └── sessions/<node>.json   # that device's latest handoff
├── bus/
│   └── <node>/
│       ├── messages/<id>.json # written only by the sender
│       └── receipts/<id>.json # written only by the receiver
├── policy/
│   └── <node-id>.json      # this device's autonomy ceiling
├── secrets/                # sops+age encrypted; keys never in git
└── system/
    └── <node-id>.changes.jsonl  # OS modifications + rollback commands
```

**Conflict strategy.** Every file in the repo is either *device-owned* — no
other machine ever writes it — or *shared and additive*, where concurrent
changes land on different paths. Per-node scratch and journals cannot collide;
long-term memory is one file per fact, so two devices remembering different
things never touch the same path. Genuinely shared files (`config/manifest.json`,
`tasks/<id>/task.json`) are last-write-wins, which is what makes a task claim a
lease. This is the difference between a system that survives real concurrent use
and one that spends its life in merge conflicts.

An earlier draft proposed a single append-only `journal.jsonl` on the theory
that concurrent appends produce a mergeable diff. They do — under a git *merge*.
Reconcile hard-resets to origin (§4a), so a shared append-only file would lose
this device's lines every time. Sharding per device is what actually holds.

### 4a. Automatic sync (implemented)

Every state-changing command commits and pushes on its own; `nimbus state sync`
exists only as a manual retry. A repo that syncs only when asked is stale
exactly when another device needs it.

Divergence is the interesting case. Two devices that both publish without
seeing each other produce a non-fast-forward push, and go-git's `Pull` performs
only fast-forward merges — so a general three-way merge is not available.

The data model makes that unnecessary. Each device is authoritative for a known
set of paths that no other device ever writes:

```
nodes/<id>.json                    this device's profile
audit/<id>.jsonl                   this device's action log
tasks/*/progress/<id>.jsonl        its shard of every task timeline
tasks/*/sessions/<id>.json         its handoff on every task
```

So reconciliation is: capture this device's owned files, hard-reset to `origin`,
reapply them, commit, push. Origin wins for everything else, which means a
shared file another device edited is never silently reverted — and, for
`tasks/<id>/task.json`, means the first device to push keeps the claim (§7).

The last two are glob patterns rather than fixed paths, because the set grows
whenever any device in the fleet creates a task; a device cannot enumerate what
it owns without looking.

Two failure modes are deliberately distinguished:

- **Offline** — commit locally, report it, succeed. The push happens next run.
- **Rejected** — reconcile and retry, up to three attempts.

Sync failures never fail the command that triggered them: the work the user
asked for already succeeded, and an unpushed commit is durable. Set
`NIMBUS_NO_SYNC=1` to opt out entirely.

### 4b. Finding a system (implemented)

A **system** is one state repo plus the fleet of devices on it. The second
device originally needed `nimbus init --remote <url>`, which meant remembering
or going to look up a URL at exactly the moment the promise was "sit down at any
machine, one command". Discovery removes that step.

**Two mechanisms, and only one of them is authoritative.**

| | Purpose | Trusted? |
|---|---|---|
| A `nimbus-state` provider topic | Narrows hundreds of repositories to a handful without opening any | No — a hint |
| A `nimbus.json` marker at the repo root | Decides whether a repo is a system, and names it | Yes |

The topic exists because the alternative is unaffordable: checking a marker
requires reading a file, and reading a file from every repo on an account is
either a lot of API calls or a lot of clones. The topic makes the candidate set
small; the marker then decides.

The marker has to be the authority because the topic is not durable. It can be
deleted by hand, a fork does not carry one, and a repository shared as a link
from somebody else's account may never have had one. A repo whose name matches
the `nimbus-state` convention is also treated as a candidate, so a fleet
enrolled before this feature is still found rather than appearing to vanish on
upgrade.

**The marker is also a safety check, and this is the part that was actually
broken before.** `--remote <url>` previously cloned whatever it was pointed at
and began writing `nodes/`, `audit/`, and `secrets/` into it. Pointed at an
ordinary project by a typo or a mis-paste, the first sign of trouble would have
been a commit on somebody else's repository. Verifying the marker before cloning
turns that into a refusal.

**Selection rules.** Ordered so that automation never blocks:

1. An explicit `--remote` wins, after verification.
2. A device already on a system stays on it — re-running `init` never re-asks.
3. `--new <name>` creates one.
4. Otherwise discover: one match is taken silently, several are offered.

Ambiguity in front of a person is a prompt. Ambiguity with nobody watching is an
error listing the options and naming `--system`, because `init` runs in
installers and containers where a prompt is a hang rather than a question. This
reuses the attended/unattended distinction the autonomy ladder already defines
(§12).

**Identity survives renaming.** The marker carries a generated id alongside the
name. The name is what people read and therefore what people change; anything
that refers to a system refers to the id, so renaming does not orphan it.

**A device belongs to one system at a time.** Every path in §4 is relative to a
single `~/.nimbus`, so joining a second system means re-running `init` and
pointing that directory at the other repo. Simultaneous membership would mean
per-system paths throughout, which buys little for a single-user tool.

## 5. Components

### `nimbusd` — the daemon

One per node. systemd user unit on Linux, launchd agent on macOS. Responsible for:

- Node registration, heartbeat, and capability advertisement to the mesh
- State repo sync (commit + push on session end, checkpoint, and interval)
- Inbox and task queue; spawning headless Claude Code for dispatched work
- Boot detection and task resume (§10)
- Serving the local MCP endpoint that Claude connects to

### `nimbus` — the CLI

```
nimbus init                 # bootstrap this device from the state repo
nimbus doctor               # detect OS/tools/hardware; diff against manifest
nimbus alias [name]         # show or set this device's human-readable name
nimbus install              # install everything doctor found missing
nimbus resume [task-id]     # claim a task here and print the full context
nimbus boot                 # pick up work left running when the machine restarted
nimbus context              # the same brief, for the SessionStart hook
nimbus task new|list|show|note|handoff|done|release
nimbus task queue|steal     # work whose lease lapsed, and taking it
nimbus memory add|list|search|show|promote|forget|expire
nimbus send <node> "..."    # message another device, by alias
nimbus dispatch <node> <task-id>   # release the claim here, ask them there
nimbus inbox|outbox|ack     # read mail, see delivery, answer
nimbus mcp                  # serve the MCP tools on stdio (registered by init)
nimbus daemon run|install|status|uninstall
nimbus sync                 # force state repo reconcile
nimbus autonomy [level]     # show or set the unattended ceiling for this node
nimbus system apply|list|show|rollback   # OS changes, journaled with their undo
```

### The Nimbus MCP server

How Claude reaches the mesh. Registered in `mcp.manifest.toml`, so it installs
on every node automatically.

## 6. The mesh — devices as peer agents

The design goal: **a peer device should feel like a subagent that happens to have
different hardware.** Your existing `SendMessage` coordination patterns should
work across machines with no conceptual change.

### 6a. Transport: git first, not tailnet (implemented)

This section originally specified a tailnet embedded via `tsnet` as *the*
transport. Measured, `tailscale.com/tsnet` pulls **547 Go modules** against the
four nimbus currently has — and, decisively, it needs a Tailscale account. That
contradicts §2a and the promise that logging into git is enough.

Git is already the durable channel: authenticated, synced, offline-tolerant, and
now proven against a live remote. So the mesh runs on git, and a tailnet can be
added later behind the same interface as a pure latency optimization. The
ordering in the original roadmap was backwards — it put the hard dependency
first and the useful capability second.

The cost is honest: latency is a sync interval rather than a round trip, and
`peer_tail`-style live streaming is not possible over git at all. Everything
else in the table below is.

```
bus/<from>/messages/<id>.json   written only by the sender
bus/<from>/receipts/<id>.json   written only by the receiver
```

**No file has two writers.** A sender never writes into the recipient's
directory and a receiver never edits a message, so the bus needs no merge
strategy — the same ownership rule that makes profiles, audit logs, task shards,
and scratch memory conflict-free (§4a).

**Delivery state is derived, not stored.** A message is delivered because a
receipt exists, not because someone flipped a field. Two devices therefore
cannot disagree about it, and acknowledging can never race the sender.

Message ids are time-prefixed and random-suffixed: a directory listing is
chronological without opening every file, and two devices sending in the same
second cannot collide on a path.

**Unread mail is injected into the session brief** (§9). A message that only
appears when someone remembers to run `nimbus inbox` is a message nobody reads;
putting it in the SessionStart payload makes another device's request the first
thing Claude sees.

`nimbus dispatch <device> <task>` releases the claim here and messages there in
one command, because doing it in two invites the state where a task is announced
but still held — which blocks the device being asked to do it. Dispatch checks
the target's published capabilities first and refuses to send GPU work to a
machine without a GPU.

Current surface: `nimbus send`, `nimbus dispatch`, `nimbus inbox`,
`nimbus outbox`, `nimbus ack`. Still to build: `nimbusd` for automatic polling
and unattended execution, and the MCP server that exposes the tools below.

### 6b. The MCP server (implemented)

`nimbus mcp` speaks MCP over stdio and is registered in the manifest, so every
device that runs `nimbus init` gets the mesh reachable from inside Claude. The
protocol is hand-rolled over `encoding/json` — MCP on stdio is JSON-RPC 2.0 with
three methods that matter, and an SDK would cost more than it saves against §2a.

**The tools are shims over the CLI, not a parallel implementation.** Each one
shapes arguments and calls the same dispatch loop a person's shell does, with
output captured. One code path means the tool surface and the CLI can never
disagree about when a claim is refused or what a dispatch does, and every fix
reaches both at once.

Exposed: `nimbus_context`, `nimbus_fleet`, `nimbus_send`, `nimbus_inbox`,
`nimbus_ack`, `nimbus_dispatch`, `nimbus_task_note`, `nimbus_handoff`,
`nimbus_memory_add`, `nimbus_memory_search`.

Two things the protocol punishes if you get them wrong:

- **stdout carries protocol only.** One stray print corrupts the stream and the
  client disconnects with a parse error naming nothing useful. Human-facing
  output goes to stderr.
- **Notifications must never be answered.** They arrive with no id, immediately
  after `initialize`, and replying is a violation some clients treat as fatal.

A failing tool returns `isError: true` with the message as content rather than a
JSON-RPC error, so the model reads why it failed and tries something else
instead of the request dying at the transport.

### 6c. `nimbusd` — the daemon (implemented, narrow)

`nimbus daemon run` syncs on an interval and reports new mail. `nimbus daemon
install` writes a **per-user** systemd unit or launchd agent — never system-wide,
because the daemon reads the user's credentials and writes the user's state repo,
so running it as root would either fail on permissions or succeed and leave
root-owned files in a home directory.

Since Phase 4 it also runs `nimbus boot` once at startup, which is the boot
resume hook (§10): the daemon is what the service manager starts at login, so it
is the only thing present to notice a task was in flight when the machine went
down. A failed resume never stops the sync loop — syncing is what every other
device depends on, resuming is this one's convenience.

Scope is otherwise deliberately narrow: it syncs and it reports. It does **not**
execute dispatched work unattended — that needs a transport that can stream
results back (§6a). What it buys today is that a message sent from another
device arrives without anyone typing a command, which is the difference between
a mesh and a pair of repos.

Sync times are jittered. Devices started by the same unit at boot would
otherwise line up on one schedule, collide on every push, and spend their cycles
reconciling each other rather than syncing.

`nimbus daemon status` reports the repo's last commit time rather than only what
the service manager thinks: a process can be up and failing every cycle, but the
commit timestamp moves only when a sync actually did something.

The peer surface this section originally proposed, against what shipped:

| Proposed | Status |
|---|---|
| `peer_list` | `nimbus_fleet` |
| `peer_dispatch` | `nimbus_dispatch` |
| `peer_message` | `nimbus_send` |
| `peer_inbox` | `nimbus_inbox` |
| `peer_claim` | `nimbus_task_queue` + `nimbus_task_steal` (§12a) |
| `peer_await` | Not shipped. Blocking on a git round-trip is a poll loop wearing a different name |
| `peer_tail` | Not possible over git. Needs a real transport |
| `peer_exec` | Allowlist is implemented and enforced (§12, invariant 3); nothing dispatches through it yet |

**Dispatch lifecycle.** `peer_dispatch` writes the task to the state repo, pushes,
then notifies the target over the tailnet. Target's `nimbusd` pulls, spawns a
headless Claude Code (`claude -p`) inside a detached tmux session, and streams
progress back. The tmux session means the work survives `nimbusd` restarting and
stays attachable for a human.

**The cross-device debugging case.** Client on laptop, server on desktop:

1. Laptop Claude opens a *debug session* — a task with `affinity = ["laptop", "desktop"]`.
2. Both nodes join; a shared correlation id is injected into both sides' logging.
3. Laptop reproduces; `peer_tail` streams the desktop's server logs into the laptop session in real time.
4. Either side proposes a fix; `peer_exec` applies and restarts on the correct machine.
5. Both write to the same `progress.jsonl`, so the transcript is unified.

This is the feature with the least prior art and the most value. It is also why
the mesh is a first-class component rather than a sync tool with extras.

## 7. Task model and device affinity (implemented)

A task is the unit that moves between machines. It lives in the state repo as
three kinds of file, and the split between them is the whole design:

```
tasks/<id>/task.json              shared    goal, repo, branch, needs, claim
tasks/<id>/progress/<node>.jsonl  owned     append-only timeline, one per device
tasks/<id>/sessions/<node>.json   owned     that device's latest handoff
```

**Sharded, not shared.** Two devices appending to one `progress.jsonl` would
conflict on every sync. Two devices appending to their own shards never do, and
a merged read reconstructs the single timeline — the same trick the audit log
already uses. `Progress()` globs every shard and sorts by timestamp.

**The claim is a lease, and that is why `task.json` is deliberately not owned.**
When history diverges, reconcile takes origin's copy of anything outside the
owned set (§4a), so two devices claiming at once resolve to whichever pushed
first. The loser finds out: `nimbus resume` reloads after syncing and reports
that it does not hold the task. Its own progress and handoff shards survive
regardless, because those *are* owned — losing a race must not cost you work.

**Affinity** is what makes cross-device handoff correct rather than merely
possible. `needs` lists capability labels matched against the device profile's
`Capabilities()` (§9). `nimbus resume` refuses a task whose requirements this
device does not meet, naming what is missing and pointing at `nimbus fleet`;
`--force` overrides. The gap is repeated in the session brief, because the human
who dismissed the warning is not the one who then attempts the work.

**Handoff notes.** At session end Claude writes what was done, what is in
flight, what is next, what is blocked, and any state that lives outside git:

```
nimbus task handoff --done "client token refresh" \
                    --next "server verification middleware" \
                    --outside-git "postgres in docker on :5432" --release
```

That last field is the one people forget and the one that wastes the most time
on the receiving machine. One file per device rather than a shared `handoff.md`:
a shared file is overwritten by whichever device pushes last, silently
discarding a note the other had just written.

`nimbus resume` then assembles goal, branch, the previous device's handoff, the
merged timeline, and this device's capabilities into one markdown brief. The
same brief is what `nimbus context` prints for the SessionStart hook (§9), so
Claude gets it without being asked.

A task also carries an optional **resume contract** (`on_boot`, `allowed_first`,
`require_human`) governing what happens to it across a reboot — see §10. It is
attached only when something was actually asked for: an empty contract on every
task would make `on_boot` look meaningful everywhere and mean nothing anywhere.

### 7a. Session continuity

An earlier draft of this document planned to integrate `claude --cloud` and
`claude --teleport`. **Those flags do not exist.** Claude Code 2.1.220 offers
`--session-id <uuid>`, `--resume <uuid>`, `--fork-session`, and `--continue`,
all operating on local transcripts. The design below is built on what is
actually there.

`--session-id` is the useful one, because it lets nimbus *assign* a session id
instead of discovering one afterwards — so a conversation can be recorded
against a task before it exists. `nimbus resume` binds one session per
(task, device) and stores it in `tasks/<id>/sessions/<node>.json`, alongside
that device's handoff.

The result is honest about what it can deliver:

| | What you get |
|---|---|
| **Same device, later** | `claude --resume <id>` — the actual conversation, full history |
| **Different device** | A fresh session seeded with the handoff brief |

**Transcripts are deliberately not synced**, for three independently sufficient
reasons:

1. **Size.** Real transcripts run to megabytes and grow append-only. Git stores
   a full copy of every revision, so a few weeks of sessions would produce a
   repo measured in gigabytes.
2. **Secrets.** A transcript contains every command output and every file read
   during the session. The redaction hook (§8) does not exist yet, and shipping
   raw transcripts to a git remote before it does would be reckless.
3. **They would not work anyway.** Claude Code keys sessions by working
   directory — `~/.claude/projects/<encoded-cwd>/<id>.jsonl` — and the same
   project sits at a different path on every machine.

So the handoff brief is not a fallback for cross-device work; it is the correct
mechanism. Claiming otherwise would promise continuity the data cannot deliver.

A binding can outlive its transcript — reimaged machine, pruned history, a
binding pulled from another device. `claude --resume` on a missing session fails
outright, so nimbus checks first and starts a fresh session with an explanation
rather than handing over an error.

## 8. Memory model (implemented)

Two tiers with explicit promotion, rather than one undifferentiated pile:

- **Scratch** (per-device, TTL 7d) — working notes, exploration, dead ends.
- **Long-term** (fleet-wide, curated) — durable facts about you and your systems.

```
memory/scratch/<node>/<id>.md   device-owned, expires on its own
memory/long-term/<id>.md        shared, one file per fact
memory/journal/<node>.jsonl     device-owned record of what was remembered
```

The design originally listed a third **task** tier. It collapsed into scratch
tagged with a task: same owner, same conflict behaviour, same reason to expire.
A separate directory would have been three code paths for one concept.

**One file per fact, not one store.** Two devices adding different memories
touch different paths and never collide, which is what lets long-term memory be
shared without a merge strategy. Combined with the addition-rescue in §4a, a
memory written while another device was pushing survives the reconcile.

**Decay is the point.** `nimbus memory add` writes scratch that expires in seven
days; `nimbus memory promote <id>` is the deliberate act that makes something
durable. Without decay the repo grows without bound and injecting it into a
session degrades the context it was meant to improve. A device only ever sweeps
its *own* scratch — those files are owned, so deleting them cannot race another
machine's writes.

Files are markdown with a small frontmatter block rather than JSON, because
Claude and a person both read and edit them directly. The parser is deliberately
not YAML: the format is ours and the keys are known.

**The journal exists because memory is the one part of the state repo that
deletes things.** When a fact you expected is missing, the absence of a file
cannot distinguish "expired" from "promoted elsewhere" from "never written".
`memory/journal/<node>.jsonl` records add, promote, forget, and expire.

Memory reaches Claude through the same brief as everything else (§7a), capped at
15 entries — durable facts first because they were explicitly kept, then scratch
for the active task. Past that cap, injecting more degrades the context it is
meant to improve; the rest is a `nimbus memory search` away.

**Redaction on sync — not yet implemented.** A pre-commit hook should scan
outbound diffs for keys, tokens, and PII before anything reaches the remote.
Until it exists, treat the state repo as holding whatever you put in it: memory
is written by hand or by Claude, so nothing lands there without an explicit
`memory add`, but nothing scans it either.

**Approval-free writes.** The state repo — and only the state repo — is
pre-approved in `settings.json`:

```json
{ "permissions": { "allow": ["Bash(git -C ~/.nimbus *)", "Write(~/.nimbus/**)"] } }
```

Your code repositories are never written unattended by the sync layer.

## 8a. Audit trail (implemented)

Every action nimbus takes is appended to `audit/<node-id>.jsonl` in the state
repo. The log is **hash-chained**: each entry commits to the hash of its
predecessor, so editing or removing a past entry invalidates every entry after
it, and `nimbus audit --verify` detects it.

That property is what makes the log evidence rather than a debugging aid. In the
support case the reader is not the actor, so "the log says X" has to mean
something stronger than "a file on the machine says X".

Each entry carries a **rollback command recorded before the action runs**, so a
change made by an unattended setup can be undone by someone who did not watch it
happen. Package installs record their removal command; system changes record
their restore path.

## 9. Device profile and capability detection

`nimbus doctor` produces `nodes/<node-id>.toml`:

```toml
id       = "desktop"
os       = "linux"
distro   = "pop-os"
kernel   = "7.0.11-76070011-generic"
arch     = "x86_64"
pkg_mgr  = "apt"
tools    = { node = "20.11.0", python = "3.12", docker = "25.0", gh = false }
hardware = { gpu = "nvidia", display = true, ram_gb = 64 }
services = ["postgres:5432", "redis:6379"]
missing  = ["gh", "sops"]
```

This is injected by a `SessionStart` hook running `nimbus context`, so Claude
always knows where it is running, what it can reach, and what task is in flight
(§7) without being told. It also drives `nimbus install` (reconcile against
`mcp.manifest.toml` + `requires`) and mesh routing decisions.

`nimbus init` registers the hook by merging into `settings.json` rather than
rewriting it — that file holds the user's own hooks, permissions, and model
preferences, and a file that cannot be parsed is left untouched rather than
overwritten. The registered command is bare `nimbus context`, not an absolute
path: `settings.json` travels to every device through the state repo, and
`/home/you/.local/bin` does not exist on the Mac.

**The hook can never fail.** A `SessionStart` hook that exits non-zero turns
every Claude session on the device into an error report, so a missing state
repo or an unreadable task degrades to printing less rather than printing a
failure.

**Machine ids are not as unique as they look.** VM clones, golden images, and
containers from one base all carry the same `/etc/machine-id`. Two nodes sharing
an id would overwrite each other's profile and, worse, each other's audit log —
the record whose whole value is being tamper-evident. `NIMBUS_NODE_ID` overrides
the derived value on the clone.

### 9c. Device aliases

A node id is 32 hex characters. Nobody can read "held by `8f3a2b1c9d4e...`" and
know which machine to walk over to, so every device also carries an **alias**:
`kali-thinkpad`, `build-server`, `studio`.

**The alias is a label over the id, never a replacement for it.** The id appears
in every state repo path (`nodes/<id>.json`, `tasks/*/progress/<id>.jsonl`) and
is hashed into every audit entry — renaming it would orphan the history and
invalidate the chain. Because the alias is only a label, it can be changed as
often as you like, and `nimbus audit --verify` still passes afterwards.

```
nimbus init --alias build-server   # name it at enrollment
nimbus alias kali-thinkpad         # or rename it any time
nimbus alias                       # show the current name and id
```

**Resolved at display time, not stored per reference.** Claims, progress
entries, and handoffs all record the node id; the name is looked up from the
fleet when rendering. So renaming a device updates every record that mentions
it, *including ones written before the rename* — there is no stale copy of the
old name anywhere to go and fix.

**Names are how you address a device**, so anywhere one is accepted —
`nimbus audit --node kali-thinkpad` — takes an alias, a full id, or an
unambiguous prefix of either. Exact matches win outright: without that rule a
device aliased `build` could be shadowed by another whose id merely starts with
`build`, and which one you got would depend on directory order. An ambiguous
reference is refused and lists the candidates rather than guessing.

Aliases must be unique across the fleet, since a name shared by two devices
addresses neither; `--force` is the override for a machine being replaced.
Unnamed devices default to their hostname minus the domain (`studio.local` →
`studio`), so a device is never presented as a hex string just because nobody
got around to naming it.

**Containers are not devices.** Container runtimes bind-mount the host's `/sys`,
so DRM and PCI probing inside one describes hardware the container cannot use.
A container that reports a connected display would be handed GUI work it cannot
run. `InContainer()` detects this and publishes a `container` capability so
affinity rules can exclude them.

### 9a. Prerequisite installation

`nimbus doctor` finds what is missing; Tier 0 installs the subset nimbus needs
to function. Package managers are abstracted (`apt`, `dnf`, `pacman`, `zypper`,
`apk`, `brew`), each with both an install and a **removal** form — the removal
form is what the audit trail records as the rollback.

### 9b. Elevation

Nimbus never reads, stores, or transports a password. Per platform:

| Platform | Mechanism |
|---|---|
| Linux / macOS | `sudo -v` prompts through sudo's own PAM path with the terminal attached; sudo then caches the credential and every later `sudo -n` succeeds. A keepalive refreshes it so a long install cannot expire mid-run. |
| Windows | A running process cannot gain privilege — UAC grants it only at process creation. Uses Windows 11's built-in `sudo` when present, otherwise `RelaunchElevated()` re-executes nimbus through the `runas` verb. |

Elevation is requested **only when something actually needs installing**, so a
second `nimbus init` on a configured device never prompts.

`brew` and `winget` are exempt: brew refuses to run as root, and winget
installs per-user. Demanding elevation for them would break setup on machines
that need none.

**Storing the sudo password is deliberately not supported.** A stored
credential syncing through the state repo would mean compromise of one age key
equals root on every device in the fleet. The unattended path is instead a
scoped `NOPASSWD` sudoers drop-in installed at enrollment — revocable,
auditable, and limited to the commands nimbus actually runs.

**System change journal (implemented).** Since Nimbus is meant to fix OS issues,
every system modification is logged with its rollback command. Non-negotiable
for autonomous OS work.

`nimbus system apply "<cmd>" --rollback "<undo>"` is the enforcement point: a
change made through it is journaled before it runs, and a change made any other
way is not one nimbus can undo for you. The journal is append-only and
device-owned at `system/<node>.changes.jsonl`; a status update appends another
record with the same id rather than rewriting the first, which preserves both
the append-only property and the sync model that depends on it.

Two consequences fall out of the ordering:

- A run that dies mid-flight is visible as a change still marked `planned`,
  which is precisely the state someone recovering the machine needs to see.
- A *failed* change is journaled too. It modified the machine often enough that
  pretending otherwise is the dangerous assumption, and the folded record keeps
  the rollback — a failure is when it matters most.

A rollback is refused on any device other than the one that applied the change:
the command undoes a change to a specific machine's packages and services, and
running it elsewhere would at best do nothing.

## 10. Boot resume (implemented, propose-only by default)

The service unit installed by `nimbus daemon install` starts at login, syncs,
and then runs `nimbus boot` once. The daemon is the hook because it is the only
thing the service manager starts — nothing else is present to notice that a task
was in flight when the machine went down.

Ordering matters: boot resume runs *after* the first sync, not before. Whether a
task is still this device's to resume is a question only the remote can answer,
since another machine may have taken it while this one was off.

A task opts in through its **resume contract**:

```
nimbus task new fix-boot --goal "repair the bootloader" \
    --on-boot \
    --allowed-first "efibootmgr -v" \
    --require-human "mkfs*"
```

The contract exists because a task that triggered a reboot was, by definition,
doing something invasive.

**Nimbus runs `allowed_first` itself** and hands the output to the session,
rather than instructing the session to run those commands before anything else.
That is the difference between an enforced rule and a request: a model that is
told "check state first" can skip it, and a machine that just rebooted is
exactly where skipping it is most expensive.

`require_human` patterns are refused by `nimbus system apply` at every autonomy
level. Unattended they cannot be satisfied at all; attended they take an
explicit `--confirm`, because naming a pattern means somebody has to say "yes,
this one" about that specific command — merely being at the keyboard is not the
confirmation that was asked for.

Pattern matching is deliberately *not* `filepath.Match`. There `*` stops at a
path separator, so the obvious pattern `rm -rf *` would fail to match
`rm -rf /var/lib/postgresql` — the exact command it was written to catch. A
command line is not a path, and treating it as one turns a safety rule into a
rule that quietly does nothing.

**Propose-only is the default.** On boot, nimbus writes what it would do to the
task timeline and stops. `nimbus daemon install --act` promotes it to starting a
headless session, which additionally requires L3 — so acting unattended takes
two deliberate decisions, not one.

## 11. Secrets

Blocking dependency for one-command bootstrap — MCP servers hold API keys, and
those cannot sit in git as plaintext.

- `age` (embedded as a Go library, no binary): encrypted secrets live *in* the state repo safely.
- The age private key lives in the OS keychain — libsecret/DBus on Linux, Keychain on macOS, DPAPI on Windows — reached through pure-Go bindings, not a CLI.
- `nimbus init` decrypts into the process environment; secrets never land on disk unencrypted.

Key acquisition on a new device is the one unavoidable manual step: either paste
the age key once, or derive it from the git provider identity so that logging in
is genuinely sufficient. The latter is better UX and worse security hygiene
(compromise of the git account compromises every secret) — see open question 2.

Revocation: re-key the repo and force-push. A lost device loses access on next pull.

## 12. Autonomy ladder (implemented)

`nimbus autonomy` sets a per-node ceiling; tasks may request less, never more.
The policy lives at `policy/<node>.json` and is device-owned: another machine
reconciling can never raise this one's level.

| Level | Claude may |
|---|---|
| **L0** | Read and propose only. No writes. |
| **L1** | Write code, run tests, commit to a feature branch. No push, no system changes. |
| **L2** | Push branches, open PRs, install packages, dispatch to peers. No system config, no destructive ops. |
| **L3** | Full autonomy: system configuration, service management, boot resume, unattended peer exec. |

**L1 is the default.** Not L0, because a device that cannot write cannot do the
work nimbus exists to carry between machines; not L2, because nothing should
reach another machine — a push, a peer dispatch — as a consequence of a default
nobody chose.

An action nimbus does not recognise costs L3. A wrapper that does not know what
it is about to do should assume the worst, not the least.

### The ladder governs *unattended* work

This is the distinction the whole model rests on, and getting it wrong in either
direction breaks something real.

A person typing a command is the confirmation the ladder would otherwise have to
ask for, so the ceiling does not cap them — otherwise `nimbus init` at the
default level could not install Claude Code, and the one-command bootstrap would
be dead on arrival. But "attended" has to be a property of *how nimbus was
invoked*, never something a command asserts about itself:

| Entry point | Attended |
|---|---|
| The CLI, run by a person | yes |
| An MCP tool call | no — that is Claude acting |
| `nimbus boot`, the daemon, a service unit | no |

Being attended never lifts an invariant. Invariant 5 is the single one that
consults it, and only because it is defined as "always require a human".

### The invariants

These hold at **every** level, including L3, and no flag turns them off:

1. Never force-push, and never push to a default branch.
2. Never commit unencrypted secrets.
3. `peer_exec` runs only allowlisted commands.
4. Every system change is journaled with a rollback command before it is applied.
5. Destructive filesystem operations outside the working tree always require a human.

They are not permission prompts — they are hard constraints in the wrapper. The
point of the ladder is that you can run L3 on your own machines precisely
because the invariants are enforced in code rather than by asking each time.

### Where each one is actually enforced

An invariant that lives in a document is a convention. The enforcement point
matters more than the wording:

| # | Enforced in | Why there |
|---|---|---|
| 1 | `autonomy.Guard.Allow` | Every push proposal passes through the guard |
| 2 | `state.Repo.Commit` | The one path every write to the state repo takes. A check the caller has to remember is a check that gets skipped |
| 3 | `autonomy.Guard.Allow` | Matched on the first word, and refused outright if the command contains shell metacharacters — otherwise `journalctl; rm -rf /` passes an allowlist that contains `journalctl` |
| 4 | `system.Apply` | The record is written *and flushed* before the command runs, so a change that stops the machine coming back still leaves its own undo behind |
| 5 | `autonomy.Guard.Allow` | Path is resolved and compared against the tree, so a `..` traversal out of it counts as outside |

A refusal always says which kind it is. Being told "raise your level" about
something no level permits would be actively misleading, so an invariant refusal
names its number and never suggests `nimbus autonomy`.

### Secret scanning refuses rather than redacts

§8 originally called for a redaction hook. Refusing the commit is stronger:
redaction silently rewrites content the user wrote, and a user who does not
notice assumes the value is still there.

The risk is the opposite one. A false positive here does not warn — it refuses a
commit, and a device that cannot commit cannot sync, which would strand a
machine over a string that merely looked wrong. So every pattern matches a
documented, prefixed token format (`ghp_`, `AKIA`, `sk-ant-`, an age private
key, a PEM private-key header) and nothing generic: no `password =`, no entropy
heuristic. `secrets/` is skipped, because the one directory built to hold
credentials safely must not be the one that blocks every sync.

The error names the file and the line but never the value: a leak report that
quotes the leak gets pasted into issue trackers, which publishes the credential
the refusal just prevented.

## 12a. Work stealing (implemented)

A claim is a lease, not a lock. Without a way to reclaim a lapsed one, a device
that goes offline mid-task takes that work with it and nothing else in the fleet
can continue it — which is the exact failure the state repo exists to prevent.

The lease is measured against the **holder's own progress timeline**, not
against `Claim.At`. A claim never moves once taken, so timing against it would
expire a device that is actively working; a device that has gone away stops
recording progress, and a device that is busy does not. Another machine's
activity on the task is not evidence that the holder is alive.

Default lease is two hours. Deliberately not minutes: a device can legitimately
go quiet while a build runs or a person goes to lunch, and stealing work that is
still in progress is worse than waiting.

`nimbus task queue` filters by capability, which is what makes it a queue rather
than a list — offering GPU work to a laptop only produces a claim that has to be
handed back. `nimbus task steal` is kept distinct from `resume --force`: force
is "I know better", steal is "the lease lapsed", and the timeline records them
as different events because a person reading it later needs to tell them apart.

Since `task.json` is shared and last-write-wins, a steal can still lose a race
with the original holder waking up and pushing first. The command reloads after
syncing and says so rather than reporting a success that did not stick.

## 13. Roadmap

**Phase 0 — The binary. [DONE]** Go skeleton, static cross-compilation,
`nimbus login` (GitHub device flow), embedded `go-git` + `age`, install script.
*Delivers: a dependency-free binary that can authenticate and clone.*

**Phase 1 — Portability. [DONE]** State repo, `nimbus init`, Claude Code
installation with prerequisite resolution, config adopt + symlink, manifest,
`nimbus doctor`, `nimbus fleet`, hash-chained `nimbus audit`.
*Delivers: sit down at any machine, one command, full setup.*

**Phase 2 — Task continuity. [PARTIAL]** Task model with sharded progress and
handoffs, device affinity enforcement, claims as leases, `nimbus task`,
`nimbus resume`, `nimbus context` + SessionStart hook, device aliases,
per-device session binding via `--session-id`/`--resume` (§7a).
plus the two-tier memory model with promotion and decay (§8).
*Delivers: start work on one device, continue on another.*
*Remaining: OS keychain for tokens.*
*Changed: the redaction hook became a refusal at commit time instead (§12).*
*Dropped: `--cloud`/`--teleport` integration — those flags do not exist (§7a).*

**Phase 3 — Mesh. [PARTIAL]** Git-backed message bus (§6a): `nimbus send`,
`dispatch`, `inbox`, `outbox`, `ack`, capability-checked routing, unread mail in
the session brief. MCP server with fifteen tools (§6b). Daemon with per-user
service units (§6c).
*Delivers: devices as peer agents, addressable by name, reachable from inside
Claude.*
*Remaining: unattended execution of dispatched work, `peer_tail` live streaming
(needs a real transport).*
*Reordered: tailnet moved behind the git bus — 547 modules and a second account
for latency nimbus does not yet need (§6a).*

**Phase 4 — Autonomy. [DONE]** Autonomy ladder with the five invariants enforced
in code (§12), each at the one path it cannot be skipped from. System change
journal with rollback recorded before the change runs (§9b). Resume contract and
boot resume, propose-only by default (§10). Work-stealing queue with claims as
leases measured against the holder's own timeline (§12a). Secret scanning that
refuses the commit rather than warning about it.
*Delivers: a device that can be trusted to act while nobody is watching.*
*Remaining: unattended execution of dispatched work — the peer-exec allowlist
exists and is enforced, but nothing yet dispatches through it, because that
needs a transport that can stream results back (§6a).*

**Phase 5 — Unattended peers.** Peer execution end to end, live output
streaming, and a tailnet transport for the cases where git round-trips are too
slow.

Each phase is independently useful. Phase 1 alone solves a real daily problem.

## 14. Open questions

1. **Resolved: sessions are local and per-device.** This was previously an open
   question about routing between Anthropic-hosted and mesh sessions. There is
   no CLI surface for hosted sessions, so the question does not arise: a session
   belongs to a device, and cross-device continuity travels as a handoff brief
   rather than as a transcript (§7a). Revisit only if Claude Code grows a
   documented way to address a remote session.
2. **Secret key acquisition.** Derive the age key from the git provider identity
   (login alone is sufficient, but git account compromise = total secret
   compromise), or require pasting it once per device (safer, breaks the
   one-login promise)? A middle path: derive by default, allow opt-in to a
   separate key for high-value secrets.
3. **Headless dispatch shape.** `claude -p` for one-shot, or a long-lived session
   driven over the MCP channel? The latter is better for interactive debugging,
   the former simpler.
4. **Journal compaction.** Append-only grows forever. Compact on a schedule, or
   only when a threshold is hit?
5. **Tailnet account.** `tsnet` needs no install, but it still needs a Tailscale
   *account* and an auth key. That is a second login, which dents the
   one-login goal. Options: accept it as a Phase 3 cost, provision auth keys
   programmatically via the Tailscale API and store them in the encrypted state
   repo (best UX, ties the mesh to your Tailscale org), or write a custom
   WireGuard-over-`wireguard-go` transport with a self-hosted rendezvous server
   (no third-party account, meaningfully more work).
