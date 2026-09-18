# Shared safety guards for the L3 VM harness (doc 06 §4, Q42, Q79, D20).
#
# Sourced, not executed. Mirrors scripts/devenv/lib.sh's own id-validation
# shape (same character whitelist, same "reject before it reaches a path or
# a command" posture) but is kept independent: the lab (loop devices) and
# the L3 harness (libvirt domains) are different resources with different
# blast radii, and coupling their scripts together would make a change to
# one silently change the other's safety guarantees.
#
# Every domain, disk image and snapshot this harness creates carries
# HOSERVA_LAB_ID, and every script that can affect a domain first checks
# vm_assert_own_domain against the exact name vm_domain_name built — never
# a bare or pattern match against whatever `virsh list` happens to return.
# That is what keeps this harness from ever touching a domain it did not
# create itself, including the invoking user's own qemu:///session domains.

die() { printf 'vm: %s\n' "$*" >&2; exit 1; }

# vm_xml_attr_escape prints $1 safe to place inside a single-quoted XML
# attribute value in domain.xml.tmpl. HOSERVA_VM_REPO_ROOT (and so every
# path derived from it — disk images, the seed ISO, the serial log) is an
# arbitrary filesystem path, not a value this harness controls the
# character set of, so it is escaped before ever reaching generated XML.
vm_xml_attr_escape() {
  local s=$1
  # Every replacement below is written \&... rather than &... : bash's
  # own ${s//pat/rep} treats a bare, unescaped '&' in rep the same way
  # sed does — "insert the text pat matched" — so an unescaped "&lt;"
  # would substitute right back to "<lt;" (confirmed empirically; this
  # is not documented behavior most bash users expect).
  s=${s//&/\&amp;}
  s=${s//</\&lt;}
  s=${s//>/\&gt;}
  s=${s//\'/\&apos;}
  printf '%s' "$s"
}

# vm_sed_replacement_escape prints $1 safe to use as the replacement text
# in a `sed 's|PATTERN|VALUE|'` substitution: backslash and '&' are sed
# replacement metacharacters regardless of which character is chosen as
# delimiter, and a literal '|' in the value would otherwise be read as
# the next delimiter instead of literal text.
vm_sed_replacement_escape() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//&/\\&}
  s=${s//|/\\|}
  printf '%s' "$s"
}

# Every virsh call in this file and every vm-*.sh script goes through the
# process environment set here, in the C locale: virsh's own state names
# (domstate's "running", "shut off", ...) are the fixed English tokens
# this file matches against, and virsh translates them under any other
# locale (found the hard way — a domain whose real state was "laufend"
# under a German LC_ALL passed straight through vm_domain_running's
# "running" match as false, so destroy-vm.sh skipped `virsh destroy`
# entirely and undefined a still-running domain instead, which does not
# stop it; the domain kept running, transient, after its own disk images
# had already been deleted out from under it). Exported once here rather
# than repeated as a prefix on every virsh invocation.
export LC_ALL=C

# Same character class scripts/devenv/lib.sh's lab_id_valid uses, and the
# same pattern the Makefile's own top-level HOSERVA_LAB_ID guard requires
# before this file is ever sourced — checked again here because this file
# must also be safe to source from a plain shell, outside `make`.
vm_id_valid() {
  local id=$1
  [[ "$id" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || return 1
  [[ "$id" != *..* ]] || return 1
  return 0
}

# Sets every path and name this harness uses for the current lab, and
# requires HOSERVA_LAB_ID the same way scripts/devenv/lib.sh's
# lab_require_id does. VM_ROOT lives under the repository workspace
# (never a shared or system location, D20) so disk images, snapshots and
# cloud-init seeds are removed with the workspace and can never collide
# with another lab id or another checkout.
vm_require_id() {
  : "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID — parallel VM lanes must never share a domain name or disk image}"
  vm_id_valid "$HOSERVA_LAB_ID" || die "invalid HOSERVA_LAB_ID '$HOSERVA_LAB_ID': must match ^[a-zA-Z0-9][a-zA-Z0-9_.-]*\$ and must not contain '..'"

  local script_dir repo_root
  script_dir="$(cd "$(dirname "${BASH_SOURCE[1]:-${BASH_SOURCE[0]}}")" && pwd)"
  repo_root="${HOSERVA_VM_REPO_ROOT:-$(cd "$script_dir/../.." && pwd)}"

  VM_REPO_ROOT="$repo_root"
  VM_ROOT="$repo_root/.vm/$HOSERVA_LAB_ID"
  VM_CACHE_DIR="$repo_root/.vm/cache"
  VM_IMG_DIR="$VM_ROOT/img"
  VM_SEED_DIR="$VM_ROOT/seed"
  VM_KEY_DIR="$VM_ROOT/ssh"
  VM_STATE_DIR="$VM_ROOT/state"
  mkdir -p -- "$VM_IMG_DIR" "$VM_SEED_DIR" "$VM_KEY_DIR" "$VM_STATE_DIR" "$VM_CACHE_DIR"

  VM_CONNECT="qemu:///session"
  VM_DOMAIN="hoserva-$HOSERVA_LAB_ID"

  # Deterministic per-lab port offset so parallel lanes never collide on a
  # forwarded host port, without needing a shared port registry: derived
  # from a stable hash of the lab id, not the id's own bytes, so ids that
  # sort close together (e.g. "43-a1", "43-a2") do not also land on
  # adjacent, easily-colliding ports.
  local hash
  hash=$(printf '%s' "$HOSERVA_LAB_ID" | cksum | cut -d' ' -f1)
  VM_SSH_PORT=$((20000 + hash % 10000))
  VM_HTTPS_PORT=$((30000 + hash % 10000))
}

# Refuses any virsh operation whose target is not exactly this lab's own
# domain name — the guard every vm-*.sh script calls before define/start/
# destroy/undefine/snapshot, so a typo or a stale variable can never reach
# a domain this harness did not create (including the invoking user's own
# session domains, e.g. one named without HOSERVA_LAB_ID at all).
vm_assert_own_domain() {
  local name=$1
  [[ -n "$HOSERVA_LAB_ID" ]] || die "vm_assert_own_domain: HOSERVA_LAB_ID is not set — call vm_require_id first"
  [[ "$name" == "hoserva-$HOSERVA_LAB_ID" ]] || die "refusing to operate on domain '$name': does not match this lab's own domain name 'hoserva-$HOSERVA_LAB_ID'"
}

# True if the named domain currently exists under our connection (any
# state — running, shut off, paused). Never used to decide *which*
# domain to touch, only whether ours already does.
vm_domain_exists() {
  local name=$1
  virsh -c "$VM_CONNECT" dominfo "$name" >/dev/null 2>&1
}

vm_domain_running() {
  local name=$1
  local state
  state=$(virsh -c "$VM_CONNECT" domstate "$name" 2>/dev/null) || return 1
  [[ "$state" == "running" ]]
}

# Waits (bounded) for the guest's forwarded SSH port to accept a TCP
# connection. Does not itself prove sshd is ready to authenticate — the
# caller's own ssh retry loop (vm_ssh_wait_ready) covers that.
vm_wait_tcp() {
  local port=$1 timeout_s=$2
  local waited=0
  while ! (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; do
    exec 3>&- 2>/dev/null || true
    waited=$((waited + 1))
    [[ $waited -lt $timeout_s ]] || return 1
    sleep 1
  done
  exec 3>&- 2>/dev/null || true
  return 0
}

VM_SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5)

# ssh/scp into this lab's own guest only: -p/-P always point at
# VM_SSH_PORT (127.0.0.1, the usermode-networking hostfwd this harness's
# own domain XML defines), and the identity is always this lab's own
# generated key under VM_KEY_DIR — never a host key or a path taken from
# outside this lab's own state.
vm_ssh() {
  [[ -n "$VM_SSH_PORT" ]] || die "vm_ssh: VM_SSH_PORT is not set — call vm_require_id first"
  ssh "${VM_SSH_OPTS[@]}" -i "$VM_KEY_DIR/id_ed25519" -p "$VM_SSH_PORT" "hoserva@127.0.0.1" "$@"
}

vm_scp() {
  [[ -n "$VM_SSH_PORT" ]] || die "vm_scp: VM_SSH_PORT is not set — call vm_require_id first"
  scp "${VM_SSH_OPTS[@]}" -i "$VM_KEY_DIR/id_ed25519" -P "$VM_SSH_PORT" "$@"
}

# Bounded retry loop for "the guest has booted and cloud-init has finished
# far enough that sshd answers with our key" — cloud-init on a fresh
# qcow2 boot can take upwards of a minute, so this polls rather than
# assuming any fixed delay.
vm_ssh_wait_ready() {
  local timeout_s=${1:-180}
  local waited=0
  while ! vm_ssh true 2>/dev/null; do
    waited=$((waited + 1))
    [[ $waited -lt $timeout_s ]] || return 1
    sleep 1
  done
  return 0
}
