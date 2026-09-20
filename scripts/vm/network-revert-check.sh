#!/usr/bin/env bash
# L3 confirm-or-revert for host network settings (issue #114, Q75):
# compile-on-host / run-in-guest, the same pattern as
# scripts/vm/array-sequence-check.sh. Applies an unreachable static
# address through the product API and asserts the previous configuration
# returns within the 60-second window so the UI is reachable again.
#
# Never touches a domain whose name does not contain $HOSERVA_LAB_ID.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

TEST_BIN_LOCAL="$VM_STATE_DIR/hoserva-network-l3-test"
DAEMON_BIN_LOCAL="$VM_STATE_DIR/hoservad-l3"
TEST_BIN_REMOTE="/tmp/hoserva-network-l3-test"
DAEMON_BIN_REMOTE="/tmp/hoservad-l3"
STATE_REMOTE="/tmp/hoserva-net-l3"
SOCKET_REMOTE="$STATE_REMOTE/hoserva.sock"
TCP_PORT="18008"

echo "network-revert-check[$HOSERVA_LAB_ID]: recording the guest's network backend (Q75)"
BACKEND_REPORT="$(vm_ssh 'set -e
  PATH="/usr/sbin:/sbin:$PATH"
  nm="inactive"; nd="inactive"; ifup="missing"; interfaces="missing"; netplan="missing"
  systemctl is-active --quiet NetworkManager && nm="active" || true
  systemctl is-active --quiet systemd-networkd && nd="active" || true
  command -v ifup >/dev/null && ifup="present" || true
  test -f /etc/network/interfaces && interfaces="present" || true
  ls /etc/netplan/*.yaml >/dev/null 2>&1 && netplan="present" || true
  printf "NetworkManager=%s systemd-networkd=%s netplan=%s ifup=%s /etc/network/interfaces=%s\n" "$nm" "$nd" "$netplan" "$ifup" "$interfaces"
')"
echo "network-revert-check[$HOSERVA_LAB_ID]: guest backend: $BACKEND_REPORT"
printf '%s\n' "$BACKEND_REPORT" > "$VM_STATE_DIR/network-backend.txt"

echo "network-revert-check[$HOSERVA_LAB_ID]: ensuring ifupdown is installed"
vm_ssh 'export PATH="/usr/sbin:/sbin:$PATH"; command -v ifup >/dev/null 2>&1 || (sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ifupdown)'

# Debian 13 generic cloud (this harness's base image) uses netplan →
# systemd-networkd. Detect prefers networkd, so the product path is
# read-only until we hand the NIC to ifupdown for this test. Stopping
# networkd drops SLIRP SSH; the switch runs detached and the host waits
# until SSH returns on DHCP via ifupdown.
echo "network-revert-check[$HOSERVA_LAB_ID]: switching the guest NIC to ifupdown (Q75 native backend already recorded)"
vm_ssh 'sudo rm -f /tmp/ifupdown-switched /tmp/ifupdown-switch.log'
vm_scp "$script_dir/network-ifupdown-switch-guest.sh" "hoserva@127.0.0.1:/tmp/network-ifupdown-switch-guest.sh"
vm_ssh 'chmod +x /tmp/network-ifupdown-switch-guest.sh'
vm_ssh 'nohup sudo /tmp/network-ifupdown-switch-guest.sh >/dev/null 2>&1 </dev/null &'

echo "network-revert-check[$HOSERVA_LAB_ID]: waiting for SSH after the ifupdown switch"
switched=""
for _ in $(seq 1 60); do
  if switched="$(vm_ssh 'cat /tmp/ifupdown-switched' 2>/dev/null)"; then
    break
  fi
  sleep 2
done
[[ -n "$switched" ]] || {
  echo "network-revert-check[$HOSERVA_LAB_ID]: switch log:" >&2
  vm_ssh "sudo cat /tmp/ifupdown-switch.log" || true
  die "guest did not come back on ifupdown DHCP"
}
echo "network-revert-check[$HOSERVA_LAB_ID]: $switched"

# Product writes one managed file (interfaces.d/hoserva). A leftover seed
# stanza would duplicate the iface, so move it aside now — the NIC stays
# up until the product's own ifdown/ifup.
vm_ssh 'if [ -f /etc/network/interfaces.d/00-l3-seed ]; then sudo mv /etc/network/interfaces.d/00-l3-seed /tmp/00-l3-seed.bak; fi'

echo "network-revert-check[$HOSERVA_LAB_ID]: building hoservad and the L3 test binary (compiling touches no device)"
(cd "$VM_REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -tags l3 -c -o "$TEST_BIN_LOCAL" ./internal/api/)
(cd "$VM_REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$DAEMON_BIN_LOCAL" ./cmd/hoservad)

vm_scp "$TEST_BIN_LOCAL" "hoserva@127.0.0.1:$TEST_BIN_REMOTE"
vm_scp "$DAEMON_BIN_LOCAL" "hoserva@127.0.0.1:$DAEMON_BIN_REMOTE"
vm_ssh "chmod +x $TEST_BIN_REMOTE $DAEMON_BIN_REMOTE"

echo "network-revert-check[$HOSERVA_LAB_ID]: starting hoservad in the guest"
vm_ssh "sudo mkdir -p $STATE_REMOTE && sudo rm -f $SOCKET_REMOTE"
# The daemon is started in the background inside the guest; its PID is
# written to a file so we can kill exactly that process later — never by
# name or pattern.
vm_ssh "sudo sh -c 'nohup $DAEMON_BIN_REMOTE --state-dir $STATE_REMOTE --socket-path $SOCKET_REMOTE --machine-key-path $STATE_REMOTE/secret.key --config-root /etc --tcp-addr :$TCP_PORT --allow-all-sources >/tmp/hoservad-l3.log 2>&1 & echo \$! > $STATE_REMOTE/hoservad.pid'"

cleanup() {
  pid="$(vm_ssh "sudo cat $STATE_REMOTE/hoservad.pid 2>/dev/null || true" || true)"
  if [[ -n "$pid" ]]; then
    vm_ssh "sudo kill $pid 2>/dev/null || true" || true
  fi
  vm_ssh 'if [ -f /tmp/00-l3-seed.bak ]; then sudo mv /tmp/00-l3-seed.bak /etc/network/interfaces.d/00-l3-seed; fi' || true
}
trap cleanup EXIT

echo "network-revert-check[$HOSERVA_LAB_ID]: waiting for hoservad"
ready=0
for _ in $(seq 1 30); do
  if vm_ssh "sudo test -S $SOCKET_REMOTE"; then
    ready=1
    break
  fi
  sleep 1
done
[[ "$ready" -eq 1 ]] || {
  vm_ssh "sudo cat /tmp/hoservad-l3.log" || true
  die "hoservad did not create $SOCKET_REMOTE"
}

echo "network-revert-check[$HOSERVA_LAB_ID]: creating the first admin (setup gate)"
vm_ssh "curl -sk -X POST https://127.0.0.1:$TCP_PORT/api/v1/setup/admin -H 'Content-Type: application/json' -d '{\"username\":\"hoserva-l3\",\"password\":\"hoserva-l3-network-password\"}' >/dev/null || true"

# Applying an unreachable address takes the guest NIC off 10.0.2.15, which
# drops this host's SLIRP SSH forward. The test must outlive that session
# and write an exit file the host can read after SSH returns when
# confirm-or-revert restores the previous config.
echo "network-revert-check[$HOSERVA_LAB_ID]: running the confirm-or-revert test (detached; SSH will drop during the window)"
vm_ssh "sudo rm -f /tmp/network-l3.log /tmp/network-l3.exit"
vm_ssh "nohup sudo sh -c 'env HOSERVA_LAB_ID=$HOSERVA_LAB_ID HOSERVA_L3_SOCKET=$SOCKET_REMOTE HOSERVA_L3_TCP=https://127.0.0.1:$TCP_PORT $TEST_BIN_REMOTE -test.v -test.run TestL3UnreachableAddressReverts -test.timeout 3m > /tmp/network-l3.log 2>&1; echo \$? > /tmp/network-l3.exit' >/dev/null 2>&1 </dev/null &"

echo "network-revert-check[$HOSERVA_LAB_ID]: waiting for the guest test (SSH is expected to fail while the address is unreachable)"
exit_code=""
for _ in $(seq 1 90); do
  if exit_code="$(vm_ssh 'sudo cat /tmp/network-l3.exit' 2>/dev/null)"; then
    break
  fi
  sleep 4
done
[[ -n "$exit_code" ]] || {
  echo "network-revert-check[$HOSERVA_LAB_ID]: timed out waiting for the guest test; last SSH/log follows" >&2
  vm_ssh "sudo cat /tmp/network-l3.log /tmp/hoservad-l3.log /tmp/ifupdown-switch.log" || true
  die "guest test did not finish within 6 minutes"
}

echo "network-revert-check[$HOSERVA_LAB_ID]: guest test log"
vm_ssh "sudo cat /tmp/network-l3.log" || true
[[ "$exit_code" == "0" ]] || die "guest TestL3UnreachableAddressReverts exited $exit_code"

echo "network-revert-check[$HOSERVA_LAB_ID]: PASS"
