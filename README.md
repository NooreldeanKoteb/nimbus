# Nimbus

**Your Claude Code sessions, memory, and devices as one mesh.**

Claude Code is per-machine. Conversations, memory, MCP servers, skills, and
settings live in `~/.claude` on one box. Move to another device and you start
cold: no history, no context, no idea what the machine you are sitting at can
actually do.

Nimbus makes all of it portable. Write the client on the laptop, continue the
server on the desktop — same task, same context, one command. Every device is
addressable as a peer that can be handed work and can hand work back.

```
$ ./scripts/build.sh                  # one static binary, no dependencies
$ nimbus login                        # device flow to your git provider
$ nimbus init --new personal          # creates the state repo, sets this device up
$ nimbus resume                       # continue where you left off
```

Four steps, one of them a browser login. Nothing else needs to be installed
first — not Node, not Python, not even `git`.

---

## Contents

- [What it does](#what-it-does)
- [Install](#install)
- [Walkthroughs](#walkthroughs)
- [Commands](#commands)
- [Autonomy](#autonomy)
- [How it works](#how-it-works)
- [Status](#status)
- [Development](#development)

---

## What it does

**Portability.** `~/.claude` — settings, skills, agents, commands, `CLAUDE.md` —
lives in a private git repo and is symlinked into place on every device. A new
machine runs one command and has your whole setup, including Claude Code itself.

**Task continuity.** A task is the unit that moves between machines. It carries
the goal, the branch, the capabilities a device needs to work on it, and a
timeline that merges across every device that touched it. `nimbus resume` claims
it here and prints everything the last device left behind.

**A mesh.** Devices message each other over the same git repo. Send a note, hand
off a task, take work whose owner has gone quiet. Unread mail shows up at the
start of the next Claude session automatically.

**Peers that act.** `nimbus exec studio "systemctl status nimbus"` runs there and
streams the output back here — but only if that machine put the command on its
own allowlist. A device decides for itself what it will do for others, and says
so when the answer is no.

**Situational awareness.** A `SessionStart` hook injects what OS, hardware, and
tooling this machine has, what task is in flight, and what the previous device
said — so Claude stops guessing which platform it is on.

**Autonomy with hard limits.** A per-device ladder controls what may happen
while nobody is watching, over a set of invariants that hold at every level and
cannot be turned off.

---

## Install

Nimbus is not published yet — there are no tagged releases and no hosted
installer. Build it:

```sh
./scripts/build.sh                        # all six targets into dist/
install -m 755 dist/nimbus-linux-amd64 ~/.local/bin/nimbus
```

**Targets:** `{linux,darwin,windows} × {amd64,arm64}`, `CGO_ENABLED=0`. Go 1.26
is the only build dependency; the binary itself has none.

`scripts/install.sh` is the eventual distribution path — POSIX `sh`, needing
only `curl`-or-`wget` and `uname`, fetching a release binary into
`~/.local/bin`. It is written and works against a real release, but nothing is
released for it to fetch, so it is not the install instruction yet.

### First device

```sh
nimbus login                    # GitHub device flow — prints a code, you paste it
nimbus init --new personal      # creates the private state repo, then sets up
```

### Every device after that

```sh
nimbus login
nimbus init --alias studio
```

**No URL to remember.** `init` scans the account you just logged into, finds the
systems already there, and joins the one it finds. You only reach for a flag
when there is a genuine choice to make.

`init` then installs Claude Code (resolving its prerequisites first), links your
config, registers the Nimbus MCP server and the `SessionStart` hook, publishes
this device's profile, and pushes. Re-running it changes nothing.

### Systems

A **system** is one state repo and the fleet of devices on it. Most people want
one. You can have several — personal and work, say — and a device joins one at a
time.

```sh
nimbus systems                  # what this account can reach, and where you are
nimbus init --new work          # start another one
nimbus init --system work       # join a specific one, no prompting
nimbus init --remote <url>      # join one somebody shared with you
```

With one system, `init` takes it silently. With several it lists them and asks —
unless nobody is there to answer, in which case it names them and tells you to
pass `--system`, because a prompt written into a pipe is a hang.

Systems are found two ways. A `nimbus-state` topic on the repository narrows
hundreds of repos to a handful without opening any of them; a `nimbus.json`
marker at the repo root then decides, because a topic can be deleted by hand and
a repo shared as a link may never have had one. **The marker is the authority.**

That marker is also a safety check. `--remote <url>` verifies it before cloning,
so pointing nimbus at an ordinary repository is refused instead of quietly
filling somebody's project with device profiles and an audit trail.

---

## Walkthroughs

### Move a task between machines

```sh
# on the laptop
nimbus task new auth-refactor --goal "move session auth to JWT" --branch feat/jwt
nimbus task note "client token refresh works"
nimbus task handoff \
    --done "client token refresh" \
    --next "server verification middleware" \
    --outside-git "postgres running in docker on :5432" \
    --release

# on the desktop
nimbus resume auth-refactor --exec
```

`--outside-git` is the field people forget and the one that wastes the most time
on the receiving machine: the container that is running, the database that was
seeded by hand, the config that never got committed.

`--exec` starts Claude Code on the session bound to this task on this device, so
returning to a machine continues the actual conversation rather than a summary
of it.

### Ask another device to do something

```sh
nimbus fleet                                   # who is out there, and what can they do
nimbus send studio "the build fails on libssl" # message a device by name
nimbus dispatch studio auth-refactor --note "you have the GPU"
nimbus inbox                                   # read what arrived
nimbus ack <id> --note "installed libssl-dev, build is green"
```

A task can declare what a device must have — `--needs gpu:nvidia`,
`--needs tool:docker`, `--needs display`. Dispatching to a machine that does not
meet them is refused rather than accepted and then stalled.

### Have another device actually run it

Messages and dispatches wait for someone to read them. `nimbus exec` does not —
the far device runs the command itself and the output streams back.

On the machine that will do the work, once:

```sh
nimbus autonomy l3                        # peer execution needs the top rung
nimbus autonomy allow "systemctl status nimbus"
nimbus autonomy allow "journalctl -u nimbus"
nimbus daemon install --work              # answer requests without anyone typing
```

From anywhere else:

```sh
nimbus exec studio "systemctl status nimbus" --wait
nimbus exec studio "journalctl -u nimbus -n 200"   # returns an id
nimbus exec --follow 20260728T054212-96ff56b6      # re-attach any time
```

**The allowlist belongs to the machine being asked.** It lives in that device's
own policy file, it is empty until someone fills it in, and nothing in a request
can widen it. A refusal comes back as a result with the reason, so you are never
left wondering whether a command is still running or was never going to.

To let a peer start a whole Claude session rather than one named command — which
no allowlist bounds — that machine needs `--act` on top of L3:

```sh
nimbus daemon install --work --act
```

### Pick up work whose owner went quiet

A claim is a lease, not a lock. If a device goes offline holding a task, the
work does not go with it.

```sh
nimbus task queue          # unclaimed work, plus lapsed claims this device can run
nimbus task steal port-server
```

### Change the system safely

```sh
nimbus system apply "apt install -y ripgrep" --rollback "apt remove -y ripgrep"
nimbus system list                        # every change, with how to undo it
nimbus system rollback 20260727T142201-a3f
```

The rollback is written to disk and flushed *before* the command runs, so a
change that stops the machine coming back still leaves behind both the fact that
it was attempted and the command that reverses it.

### Survive a reboot

```sh
nimbus task new fix-boot \
    --goal "repair the bootloader" \
    --on-boot \
    --allowed-first "efibootmgr -v" \
    --require-human "mkfs*"

nimbus daemon install      # picks the task back up at login
```

On restart, nimbus runs the `allowed_first` checks itself and hands their output
to the session — so "only these may run before state is re-established" is an
enforced rule rather than an instruction. `require_human` patterns are refused
outright, at every autonomy level.

By default this is **propose-only**: it writes down what it would do and stops.
`nimbus daemon install --act` promotes it to starting the session, which still
requires L3.

---

## Commands

### Setup and identity

| Command | |
|---|---|
| `nimbus login` / `logout` / `whoami` | Git provider auth via device flow |
| `nimbus init` | Set this device up; finds your system automatically |
| `nimbus systems` | List the systems this account can reach |
| `nimbus doctor` | Profile the OS, hardware, and tooling; diff against the manifest |
| `nimbus alias [name]` | Show or set this device's human-readable name |
| `nimbus fleet` | Every device, with capabilities and what is missing |

### Work

| Command | |
|---|---|
| `nimbus task new\|list\|show\|note\|handoff\|done\|release` | The task lifecycle |
| `nimbus task queue` / `steal` | Work-stealing across the fleet |
| `nimbus resume [task]` | Claim a task here and print the full context |
| `nimbus context` | The same brief, for the `SessionStart` hook |
| `nimbus boot` | Pick up work left running when the machine restarted |

### Memory

| Command | |
|---|---|
| `nimbus memory add [--long-term]` | Remember a fact; scratch expires in seven days |
| `nimbus memory list\|search\|show` | Find it again |
| `nimbus memory promote\|forget\|expire` | Keep it, drop it, or sweep what lapsed |

### Mesh

| Command | |
|---|---|
| `nimbus send <device> "..."` | Message another device by name |
| `nimbus dispatch <device> <task>` | Release the claim here, ask them there |
| `nimbus exec <device> "<cmd>"` | Ask a device to run a command; `--wait` streams it back |
| `nimbus work [--act]` | Do what peers have asked this device for |
| `nimbus inbox` / `outbox` / `ack` | Read mail, see delivery, answer |
| `nimbus mcp` | Serve the MCP tools on stdio (registered by `init`) |
| `nimbus daemon run\|install\|status\|uninstall` | Keep this device synced, and `--work` to answer peers |

### Safety

| Command | |
|---|---|
| `nimbus autonomy [level]` | Show or set what may happen unattended |
| `nimbus autonomy allow\|deny "<cmd>"` | What peers may run on *this* device |
| `nimbus system apply\|list\|show\|rollback` | OS changes, journaled with their undo |
| `nimbus audit [--verify]` | Hash-chained log of everything nimbus did |
| `nimbus secrets keygen\|encrypt\|decrypt` | age-encrypted values, keys never in git |
| `nimbus state sync\|status` | Force a reconcile with the remote |

### MCP tools

`nimbus init` registers Nimbus as an MCP server, so Claude reaches the mesh
directly:

```
nimbus_context          nimbus_fleet            nimbus_send
nimbus_inbox            nimbus_ack              nimbus_dispatch
nimbus_task_note        nimbus_handoff          nimbus_task_queue
nimbus_task_steal       nimbus_memory_add       nimbus_memory_search
nimbus_system_apply     nimbus_system_history   nimbus_system_rollback
nimbus_exec             nimbus_exec_result
```

They are thin shims over the same commands a person runs, sharing one dispatch
path — so the two surfaces cannot drift apart about what a dispatch does or when
a claim is refused.

---

## Autonomy

`nimbus autonomy` sets a ceiling on what happens on this device **while nobody
is watching**. Tasks may ask for less, never more.

| Level | Claude may |
|---|---|
| **L0** | Read and propose only. No writes. |
| **L1** | Write code, run tests, commit to a feature branch. No push, no system changes. *(default)* |
| **L2** | Push branches, install packages, dispatch to peers. No system changes. |
| **L3** | System configuration, service management, boot resume. |

Five invariants hold at **every** level, including L3. They are not prompts and
there is no flag that lifts them:

1. Never force-push, and never push to a default branch.
2. Never commit unencrypted secrets. Enforced at the commit itself, so no caller
   can skip it.
3. A peer may only run commands on **this** device's allowlist — set here, empty
   by default, and not widenable by whoever is asking.
4. Every system change is journaled with its rollback *before* it is applied.
5. Destructive filesystem operations outside the working tree require a human.

That is what makes L3 safe to run on your own machines: the limits are enforced
in code rather than by asking you each time.

**The ladder governs unattended work.** A person typing a command is the
confirmation the ladder would otherwise have to ask for, so the ceiling does not
cap them — but it never lifts an invariant, and a `require_human` pattern still
takes an explicit `--confirm`.

---

## How it works

### One binary, no dependencies

Every external tool the design would normally reach for is an embedded Go
library instead:

| Would have been | Embedded instead |
|---|---|
| `git` binary | `go-git` |
| `gh` CLI | GitHub device flow over plain HTTPS |
| `sops` + `age` binaries | `filippo.io/age` |

A bare Ubuntu container has no `curl` and no `unzip`, which Claude Code's own
installer needs — so nimbus resolves its dependencies' dependencies too. That is
only findable by testing on an actually empty machine, which
[`test/bare/run.sh`](test/bare/run.sh) does on every change.

### The state repo

A private git repo, `~/.nimbus`, holding everything portable:

```
config/     settings.json, CLAUDE.md, skills, agents, the MCP manifest
nodes/      <node>.json          — OS, hardware, tools, capabilities
tasks/      <id>/task.json       — goal, branch, needs, claim
            <id>/progress/<node>.jsonl
            <id>/sessions/<node>.json
memory/     long-term/<id>.md, scratch/<node>/<id>.md, journal/<node>.jsonl
bus/        <node>/messages/<id>.json, <node>/receipts/<id>.json
policy/     <node>.json          — this device's autonomy ceiling
system/     <node>.changes.jsonl — OS changes and their rollbacks
audit/      <node>.jsonl         — hash-chained action log
secrets/    age-encrypted; keys never in git
```

**Conflict strategy.** Every file is either *device-owned* — no other machine
ever writes it — or *shared and additive*, where concurrent changes land on
different paths. Per-node shards cannot collide, and long-term memory is one
file per fact, so two devices remembering different things never touch the same
path.

Genuinely shared files (`tasks/<id>/task.json`) are last-write-wins, which is
exactly what makes a claim a lease: two devices claiming at once, the one that
pushes first keeps it, and the other is told so rather than silently believing
it won.

### The audit trail

Every action lands in `audit/<node>.jsonl`, **hash-chained**: each entry commits
to the hash of its predecessor, so editing or removing a past entry invalidates
everything after it and `nimbus audit --verify` says so. That is what makes the
log evidence rather than a debugging aid — which matters when the person reading
it is not the person who ran the commands.

Each entry carries the rollback command, recorded before the action ran.

### Sessions

Nimbus does not reimplement Claude Code's session storage. It assigns and tracks
session ids so a conversation can be tied to a task and a device, and uses
`--session-id` / `--resume` to reach them. Transcripts stay where Claude Code
puts them: they are large, keyed to a working directory, and full of things that
should not be synced. The handoff is the cross-device mechanism, and that is the
correct design rather than a fallback.

---

## Status

Nimbus is usable today for everything through Phase 5. Each phase below lists
what shipped and what is deliberately still open.

### Phase 0 — The binary ✅

Go skeleton, static cross-compilation to six targets, `nimbus login` via GitHub
device flow, embedded `go-git` and `age`, install script.
*Delivers: a dependency-free binary that can authenticate and clone.*

### Phase 1 — Portability ✅

State repo, `nimbus init`, Claude Code installation with prerequisite
resolution, config adopt + symlink, manifest, `nimbus doctor`, `nimbus fleet`,
hash-chained `nimbus audit`, device aliases.
*Delivers: sit down at any machine, one command, full setup.*

### Phase 2 — Task continuity ✅

Task model with device affinity, per-device progress shards, handoffs,
`nimbus resume`, session binding, memory tiers (scratch with a TTL, curated
long-term), the `SessionStart` hook.
*Delivers: start on one device, continue on another, same task.*

*Open:* tokens are `0600` on disk rather than in the OS keychain.

### Phase 3 — Mesh ✅ (partial)

Git-backed message bus, `nimbus send` / `dispatch` / `inbox` / `outbox` / `ack`,
the Nimbus MCP server, and `nimbusd` — a per-user systemd or launchd unit that
syncs on a jittered interval and announces new mail.

The transport is git rather than a tailnet: `tailscale.com/tsnet` pulls 547 Go
modules and needs a Tailscale account, which contradicts the promise that
logging into git is enough. A tailnet may return later as a latency
optimization behind the same interface.

*Open:* nothing further. Streaming a peer's output arrived in Phase 5, over git
after all — at sync resolution rather than in real time.

### Phase 4 — Autonomy ✅

The autonomy ladder and its five invariants, enforced in code; the system change
journal with rollback (`nimbus system apply`); the resume contract and boot
resume in propose-only mode; the work-stealing queue with claims as leases; and
secret scanning that refuses the commit rather than warning about it.
*Delivers: a device that can be trusted to act while you are not watching.*

*Open:* nothing further. Unattended execution of dispatched work became Phase 5.

### Phase 5 — Unattended peers ✅

`nimbus exec` asks another device to run a command; `nimbus work` is the half
that answers, bounded by that device's own allowlist. Output streams back
through the state repo, so a long command is watchable from elsewhere while it
runs. `nimbus daemon run --work` does it without anyone typing. Dispatched
Claude sessions need `--act` on top of L3, because no allowlist bounds a
session.
*Delivers: a device that does what other devices ask, and refuses out loud when
it will not.*

*Changed:* streaming shipped over git rather than needing a new transport. The
resolution is the sync interval, not the keystroke — enough to watch a build,
not enough to interleave two live logs.

*Open:* the tailnet is still deferred. It is now a latency optimization rather
than a missing capability, and the first thing that would genuinely require it
is video (below).

### Phase 6 — Out-of-band device (note only)

A small hardware peer that plugs into another machine's HDMI out and a USB port,
captures the video, and presents itself as a keyboard and mouse — so a machine
that will not boot is still reachable from the fleet. Everything nimbus does
today assumes the target can run nimbus, and that assumption fails exactly when
it matters: a kernel panic, a bad initramfs, a GRUB prompt, a display manager
that never comes up.

It fits the mesh cleanly — a peer with `kvm:video` and `kvm:hid` capabilities,
routed to like any other. Two things are genuinely new: video cannot ride on
git, so this is the requirement that would finally justify a tailnet; and
sending keystrokes is unbounded by construction, since no allowlist can inspect
what a keyboard is about to type. Sketched in
[docs/DESIGN.md §15](docs/DESIGN.md), not designed.

---

## Development

```sh
go test ./...              # 374 tests across 15 packages
go vet ./... && gofmt -l .
./scripts/build.sh         # all six targets

./test/bare/run.sh         # containers with nothing installed
SKIP_ONLINE=1 ./test/bare/run.sh   # offline tier only, no network
```

The bare-device suite runs the binary inside deliberately empty containers.
`alpine` is the important one: it ships musl rather than glibc, so an accidental
CGO dependency fails there and nowhere else. The online tier performs a real
bootstrap on a stock `ubuntu:24.04` image, including installing Claude Code.

Design rationale, alternatives considered, and open questions live in
[`docs/DESIGN.md`](docs/DESIGN.md).
