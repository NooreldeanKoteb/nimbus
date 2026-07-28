#!/usr/bin/env bash
# Bare-device integration test.
#
# The zero-dependency claim in DESIGN.md §2a is only true if it is tested on a
# device with nothing installed. This runs the linux/amd64 binary inside
# deliberately empty containers and exercises the full CLI surface.
#
# Two tiers:
#   offline  --network=none, proves nothing is fetched at runtime
#   online   real `nimbus init`, proves the one-command bootstrap works
#
# alpine is the important image: it ships musl, not glibc, so any accidental
# CGO dependency fails there and nowhere else.
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
BIN="$ROOT/dist/nimbus-linux-amd64"

OFFLINE_IMAGES=(
    "docker.io/library/alpine:latest"
    "docker.io/library/busybox:latest"
    "docker.io/library/ubuntu:24.04"
)
ONLINE_IMAGE="docker.io/library/ubuntu:24.04"

pass=0
fail=0

ok()  { printf '    \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '    \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }

[[ -x "$BIN" ]] || { echo "missing $BIN — run scripts/build.sh first" >&2; exit 1; }

echo "==> static linking"
if command -v ldd >/dev/null 2>&1; then
    # ldd exits non-zero for a static binary, so capture before testing —
    # under `set -o pipefail` a direct pipe would report failure on success.
    ldd_out=$(ldd "$BIN" 2>&1 || true)
    if grep -qiE 'not a dynamic executable|statically linked' <<<"$ldd_out"; then
        ok "binary is statically linked"
    else
        bad "binary is dynamically linked: $(head -3 <<<"$ldd_out")"
    fi
fi

# Everything below must work with no git, no age, no curl, and no CA certs.
read -r -d '' OFFLINE <<'EOF' || true
set -eu
export NIMBUS_HOME=/tmp/nimbus-test
export HOME=/root
fails=0
check() {
    if eval "$2" >/tmp/out 2>&1; then
        echo "      ok   $1"
    else
        echo "      FAIL $1"
        sed 's/^/           /' /tmp/out
        fails=$((fails + 1))
    fi
}

# --- Phase 0: binary, auth surface, state repo, secrets ---
check "version"            "/nimbus version | grep -q nimbus"
check "help lists init"    "/nimbus help | grep -q 'init '"
check "unknown cmd errors" "! /nimbus definitely-not-a-command"
check "whoami logged out"  "! /nimbus whoami"
check "state init"         "/nimbus state init"
check "state status clean" "/nimbus state status | grep -q 'status: clean'"

mkdir -p "$NIMBUS_HOME/repo/memory"
echo '{"event":"boot"}' > "$NIMBUS_HOME/repo/memory/journal.jsonl"

check "state sees change"  "/nimbus state status | grep -q 'uncommitted'"
check "state sync commits" "/nimbus state sync | grep -q 'no origin'"
check "clean after sync"   "/nimbus state status | grep -q 'status: clean'"
check "secrets keygen"     "/nimbus secrets keygen | grep -q 'public recipient'"
check "keygen is guarded"  "! /nimbus secrets keygen"
check "encrypt roundtrip"  "echo 'API_KEY=hunter2' | /nimbus secrets encrypt | /nimbus secrets decrypt | grep -q 'API_KEY=hunter2'"

# --- Phase 1: device profiling, setup, fleet, audit ---
check "doctor runs"        "/nimbus doctor | grep -q '^device'"
check "doctor finds os"    "/nimbus doctor | grep -qE '^os +linux/amd64'"
check "doctor finds cpus"  "/nimbus doctor | grep -qE '^hardware +[0-9]+ cpu'"
check "detects container" "/nimbus doctor | grep -q container"
check "doctor is headless" "/nimbus doctor | grep -q headless"
check "doctor json valid"  "/nimbus doctor --json | grep -q '\"package_manager\"\\|\"hostname\"'"
check "doctor sees no git" "/nimbus doctor | grep -qE '^  - git'"

# init on a device with nothing: no network, so Claude install is skipped.
check "init bootstraps"    "/nimbus init --skip-claude | grep -q 'ready'"
check "init is idempotent" "/nimbus init --skip-claude | grep -q 'existing'"

# Syncing is automatic; a local-only repo must say so rather than claim a push.
check "init auto-syncs"    "/nimbus init --skip-claude | grep -qE '^sync'"
check "reports no origin"  "/nimbus init --skip-claude | grep -q 'no origin configured'"
check "sync opt-out works" "NIMBUS_NO_SYNC=1 /nimbus init --skip-claude | grep -q 'NIMBUS_NO_SYNC'"
check "doctor auto-syncs"  "/nimbus doctor --publish | grep -qE '^sync'"
check "manifest created"   "test -f $NIMBUS_HOME/repo/config/manifest.json"
check "profile published"  "ls $NIMBUS_HOME/repo/nodes/*.json >/dev/null"
check "fleet lists device" "/nimbus fleet | grep -q '\\*'"
check "fleet json valid"   "/nimbus fleet --json | grep -q '\"hardware\"'"

# --- device aliases: a node id is 32 hex characters and unusable as a name ---
check "has default name"   "! /nimbus alias | grep -q 'no alias set'"
check "alias set"          "/nimbus alias build-box | grep -q build-box"
check "alias persists"     "/nimbus alias | grep -q build-box"
check "doctor shows alias" "/nimbus doctor | grep -qE '^device +build-box'"
check "fleet shows alias"  "/nimbus fleet | grep -q build-box"
check "alias validated"    "! /nimbus alias 'Not Valid'"
check "audit by alias"     "/nimbus audit --node build-box | grep -q init.start"
check "unknown node fails" "! /nimbus audit --node nonexistent"

# --- Phase 2: tasks, handoff, resume, session context ---
check "session hook set"   "grep -q 'nimbus context' $NIMBUS_HOME/repo/config/claude/settings.json"
check "task new"           "/nimbus task new port-server --goal 'port the server' --branch feat/port"
check "task is claimed"    "/nimbus task list | grep -q '\\* port-server'"
check "task note"          "/nimbus task note 'server skeleton compiles'"
check "task show"          "/nimbus task show port-server | grep -q 'server skeleton compiles'"
check "handoff needs data" "! /nimbus task handoff"
check "task handoff"       "/nimbus task handoff --next 'wire up auth' --outside-git 'db on :5432'"

# Resume is the payload: everything the next device needs, in one place.
check "resume shows goal"  "/nimbus resume port-server | grep -q 'port the server'"
check "resume shows next"  "/nimbus resume port-server | grep -q 'wire up auth'"
check "resume shows state" "/nimbus resume port-server | grep -q 'db on :5432'"
check "handoff is named"   "/nimbus resume port-server | grep -q 'Left by build-box'"
check "brief is read-only" "/nimbus resume port-server --brief | grep -q 'Nimbus context'"

# Sessions are bound per device so returning here continues the real
# conversation; a transcript that is not on this machine must not be promised.
check "assigns a session"  "/nimbus resume port-server | grep -q 'claude --session-id'"
check "session is stable"  "test \"\$(/nimbus task show port-server | awk '/^session/{print \$2}')\" = \"\$(/nimbus task show port-server | awk '/^session/{print \$2}')\""
check "session is a uuid"  "/nimbus task show port-server | grep -qE 'session +[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}'"
check "no false resume"    "! /nimbus resume port-server | grep -q 'claude --resume'"

# The SessionStart hook runs on every claude session; it must never fail.
check "context works"      "/nimbus context | grep -q 'port-server'"
check "context names os"   "/nimbus context | grep -q 'linux'"
check "affinity blocks"    "/nimbus task new gpu-job --goal train --needs gpu:nope && ! /nimbus resume gpu-job"
check "force overrides"    "/nimbus resume gpu-job --force | grep -q 'does not meet'"
check "task done"          "/nimbus task done port-server && ! /nimbus task list | grep -q port-server"
check "bad task id"        "! /nimbus task new ../escape --goal x"

# --- memory tiers: scratch decays, long-term is kept on purpose ---
check "memory add"         "/nimbus memory add 'the handler is stubbed out' | grep -q scratch"
check "memory expires"     "/nimbus memory add 'another note' | grep -q promote"
check "memory list"        "/nimbus memory list | grep -q 'handler is stubbed'"
check "memory show"        "/nimbus memory show the-handler-is-stubbed-out | grep -q scratch"
check "memory search"      "/nimbus memory search stubbed | grep -q handler"
check "search excludes"    "! /nimbus memory search stubbed | grep -q 'another note'"
check "memory promote"     "/nimbus memory promote the-handler-is-stubbed-out | grep -q 'not expire'"
check "promoted is kept"   "! /nimbus memory show the-handler-is-stubbed-out | grep -q expires"
check "long-term add"      "/nimbus memory add --long-term 'builds need 16GB' | grep -q long-term"
check "one file per fact"  "test \$(ls $NIMBUS_HOME/repo/memory/long-term/*.md | wc -l) -eq 2"
check "journal written"    "ls $NIMBUS_HOME/repo/memory/journal/*.jsonl >/dev/null"
check "journal has add"    "grep -q '\"action\":\"add\"' $NIMBUS_HOME/repo/memory/journal/*.jsonl"
check "memory in context"  "/nimbus context | grep -q '## Memory'"
check "durable in context" "/nimbus context | grep -q '16GB'"
check "memory forget"      "/nimbus memory forget builds-need-16gb | grep -q forgot"
check "forget journalled"  "grep -q '\"action\":\"forget\"' $NIMBUS_HOME/repo/memory/journal/*.jsonl"
check "bad memory id"      "! /nimbus memory show ../escape"
check "empty memory"       "! /nimbus memory add"

# --- mesh: the bus runs on git, so it works with no network at all ---
check "empty inbox"        "/nimbus inbox | grep -q 'no messages'"
check "empty outbox"       "/nimbus outbox | grep -q 'nothing sent'"
check "no self-send"       "! /nimbus send build-box hello"
check "unknown recipient"  "! /nimbus send nonexistent-device hello"
check "send needs args"    "! /nimbus send"

# A second device is simulated by overriding the node id, which is the same
# mechanism a VM clone needs (DESIGN.md §9).
NIMBUS_NODE_ID=peer-device /nimbus doctor --publish >/dev/null 2>&1
check "fleet has two"      "test \$(/nimbus fleet | grep -c .) -ge 2"
check "send to peer"       "/nimbus send peer-device 'the build is failing' | grep -q sent"
check "outbox shows it"    "/nimbus outbox | grep -q 'build is failing'"
check "undelivered"        "/nimbus outbox | grep -q unread"
check "peer sees it"       "NIMBUS_NODE_ID=peer-device /nimbus inbox | grep -q 'build is failing'"
check "peer acks it"       "NIMBUS_NODE_ID=peer-device /nimbus inbox --ack | grep -q acknowledged"
check "delivery derived"   "/nimbus outbox | grep -q 'read '"
check "inbox now empty"    "NIMBUS_NODE_ID=peer-device /nimbus inbox | grep -q 'no messages'"
check "separate writers"   "ls $NIMBUS_HOME/repo/bus/peer-device/receipts/*.json >/dev/null"

# --- MCP server: stdout carries protocol only, so a stray print breaks it ---
INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
LIST='{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
CTX='{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nimbus_context","arguments":{}}}'
BAD='{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}'
NOTIFY='{"jsonrpc":"2.0","method":"notifications/initialized"}'

check "mcp handshake"      "printf '%s\\n' '$INIT' | /nimbus mcp | grep -q protocolVersion"
check "mcp lists tools"    "printf '%s\\n' '$LIST' | /nimbus mcp | grep -q nimbus_context"
check "mcp calls a tool"   "printf '%s\\n' '$CTX' | /nimbus mcp | grep -q 'Nimbus context'"
check "mcp rejects unknown" "printf '%s\\n' '$BAD' | /nimbus mcp | grep -q '\"error\"'"
check "mcp ignores notify" "test -z \"\$(printf '%s\\n' '$NOTIFY' | /nimbus mcp)\""
check "mcp stdout is json" "printf '%s\\n' '$INIT' | /nimbus mcp | while read -r l; do echo \"\$l\" | grep -q '^{' || exit 1; done"
check "mcp in manifest"    "grep -q '\"nimbus\"' $NIMBUS_HOME/repo/config/manifest.json"

# --- daemon: one cycle is the loop without the timer ---
check "daemon cycle"       "/nimbus daemon run --once"
check "daemon no origin"   "/nimbus daemon run --once | grep -qE 'offline|synced'"
check "daemon status"      "/nimbus daemon status | grep -q 'not installed'"
check "daemon usage"       "! /nimbus daemon frobnicate"

# --- Phase 4: autonomy ladder, invariants, system journal, boot resume ---
check "default is l1"      "/nimbus autonomy | grep -q 'level   L1'"
check "shows the ladder"   "/nimbus autonomy | grep -q 'L3  system configuration'"
check "states invariants"  "/nimbus autonomy | grep -q 'never force-push'"
check "brief states level" "/nimbus context | grep -q 'Autonomy: \\*\\*L1\\*\\*'"
check "rejects bad level"  "! /nimbus autonomy l9"
check "sets level"         "/nimbus autonomy l3 --note 'my own box' | grep -q L3"
check "level persists"     "/nimbus autonomy | grep -q 'my own box'"
check "policy is a file"   "test -f $NIMBUS_HOME/repo/policy/*.json"

# Invariant 4: no rollback, no change. This one holds at every level, so it is
# checked at L3 where nothing else would stop it.
check "rollback required"  "! /nimbus system apply 'touch /tmp/x'"
check "cites invariant 4"  "/nimbus system apply 'touch /tmp/x' 2>&1 | grep -q 'invariant 4'"
check "system apply runs"  "/nimbus system apply 'touch /tmp/changed' --rollback 'rm -f /tmp/changed' | grep -q applied"
check "change took effect" "test -f /tmp/changed"
check "journal records it" "/nimbus system list | grep -q 'rm -f /tmp/changed'"
check "dry run is inert"   "/nimbus system apply 'touch /tmp/never' --rollback true --dry-run | grep -q 'would run' && ! test -f /tmp/never"

CHANGE=$(/nimbus system list | head -1 | awk '{print $1}')
check "system show"        "/nimbus system show $CHANGE | grep -q 'touch /tmp/changed'"
check "rollback undoes it" "/nimbus system rollback $CHANGE | grep -q rolled-back && ! test -f /tmp/changed"
check "no double undo"     "! /nimbus system rollback $CHANGE"
check "audit has rollback" "/nimbus audit -n 0 | grep -q 'system.apply'"

# Invariant 2 is enforced at the commit, so it cannot be skipped by a caller.
printf 'token = ghp_%s\n' "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" > "$NIMBUS_HOME/repo/leaked.txt"
check "secret blocks sync" "! /nimbus state sync"
check "names the file"     "/nimbus state sync 2>&1 | grep -q leaked.txt"
check "does not echo it"   "! /nimbus state sync 2>&1 | grep -q ghp_a"
rm -f "$NIMBUS_HOME/repo/leaked.txt"
check "removal unblocks"   "/nimbus state sync"

# Work stealing: a claim is a lease, so a device that goes quiet does not take
# the work with it.
check "nothing to steal"   "/nimbus task queue | grep -q 'nothing to pick up'"
NIMBUS_NODE_ID=peer-device /nimbus task new stuck-job --goal 'wedged on the peer' >/dev/null 2>&1
check "live claim is safe" "! /nimbus task steal stuck-job"
check "lapsed shows up"    "/nimbus task queue --lease 1ns | grep -q stuck-job"
check "steal takes it"     "/nimbus task steal stuck-job --lease 1ns | grep -q 'taken from'"
check "steal is on record" "/nimbus task show stuck-job | grep -q steal"

# Boot resume ships propose-only: it says what it would do and stops.
check "nothing to resume"  "/nimbus boot | grep -q 'nothing to resume'"
check "on-boot task"       "/nimbus task new fix-boot --goal 'repair the bootloader' --on-boot --allowed-first 'echo entry intact' --require-human 'mkfs*'"
check "contract is shown"  "/nimbus task show fix-boot | grep -q 'always needs a person'"
check "boot runs checks"   "/nimbus boot | grep -q 'entry intact'"
check "boot briefs it"     "/nimbus boot | grep -q 'After the reboot'"
check "boot proposes"      "/nimbus boot --act | grep -q 'not starting a session'"
check "proposal recorded"  "/nimbus task show fix-boot | grep -q proposal"
check "require_human hard" "! /nimbus system apply 'mkfs.ext4 /dev/sdb1' --rollback true"

# --- audit trail ---
# -n 0 rather than the default tail: by this point the run has recorded enough
# actions that the first one has scrolled off.
check "audit has entries"  "/nimbus audit -n 0 | grep -q 'init.start'"
check "audit records task" "/nimbus audit -n 0 | grep -q 'task.new'"
check "audit verifies"     "/nimbus audit --verify | grep -q 'chain intact'"

# Tampering is the property the hash chain exists to catch. Rewrite a past
# entry and confirm verification fails rather than silently accepting it.
AUDIT=$(ls "$NIMBUS_HOME"/repo/audit/*.jsonl | head -1)
sed -i 's/init.start/init.faked/' "$AUDIT"
check "tampering detected" "! /nimbus audit --verify"
check "tamper msg is clear" "/nimbus audit --verify 2>&1 | grep -qi 'FAILED verification'"

exit $fails
EOF

# The online tier performs the real bootstrap, including installing Claude Code
# through the native installer on a machine that starts with nothing.
read -r -d '' ONLINE <<'EOF' || true
set -eu
export NIMBUS_HOME=/tmp/nimbus-test
export HOME=/root
export PATH="$HOME/.local/bin:$PATH"
fails=0
check() {
    if eval "$2" >/tmp/out 2>&1; then
        echo "      ok   $1"
    else
        echo "      FAIL $1"
        sed 's/^/           /' /tmp/out
        fails=$((fails + 1))
    fi
}

# Prove the starting point really is bare. curl matters most: Claude's own
# installer requires it, so nimbus has to satisfy its dependency's dependency.
check "no claude present"  "! command -v claude"
check "no git present"     "! command -v git"
check "no curl present"    "! command -v curl"

check "full init"          "/nimbus init"
check "installs prereqs"   "/nimbus audit | grep -q 'pkg:curl'"
check "curl now present"   "command -v curl"
check "claude installed"   "test -x $HOME/.local/bin/claude || command -v claude"
check "claude runs"        "claude --version | grep -qE '[0-9]+\\.[0-9]+'"
check "doctor sees claude" "/nimbus doctor | grep -qE '^  \\+ claude'"
check "audit records it"   "/nimbus audit | grep -q claude-code"
check "rollback recorded"  "/nimbus audit | grep -q 'rollback: apt-get remove'"
check "audit still valid"  "/nimbus audit --verify | grep -q 'chain intact'"

# Re-running must change nothing, which is what makes init safe to repeat.
check "init idempotent"    "/nimbus init | grep -q 'already installed'"

exit $fails
EOF

echo "==> offline (--network=none)"
for image in "${OFFLINE_IMAGES[@]}"; do
    short=${image##*/}
    echo "  -- $short"
    if podman run --rm --network=none \
        -v "$BIN:/nimbus:ro,Z" \
        "$image" sh -c "$OFFLINE"; then
        ok "$short offline"
    else
        bad "$short offline"
    fi
done

if [[ "${SKIP_ONLINE:-}" == "1" ]]; then
    echo "==> online tier skipped (SKIP_ONLINE=1)"
else
    echo "==> online (real bootstrap)"
    short=${ONLINE_IMAGE##*/}
    echo "  -- $short"
    # Nothing is pre-installed here: the image is used exactly as shipped, so
    # anything nimbus needs it must obtain itself.
    if podman run --rm -v "$BIN:/nimbus:ro,Z" "$ONLINE_IMAGE" sh -c "$ONLINE"; then
        ok "$short online bootstrap"
    else
        bad "$short online bootstrap"
    fi
fi

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[[ $fail -eq 0 ]]
