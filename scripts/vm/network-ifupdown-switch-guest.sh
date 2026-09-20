#!/bin/sh
# Runs as root inside the L3 guest (copied by network-revert-check.sh).
# Hands the primary NIC from systemd-networkd to ifupdown DHCP so Hoserva's
# ifupdown-only product path can apply a confirm-or-revert. Native backend
# has already been recorded on the host.
set -eu
PATH=/usr/sbin:/sbin:/usr/bin:/bin
exec >/tmp/ifupdown-switch.log 2>&1

IFACE=$(ip -o route show default | awk '{print $5; exit}')
[ -n "$IFACE" ]

cat >/etc/network/interfaces <<EOF
auto lo
iface lo inet loopback

source /etc/network/interfaces.d/*
EOF
mkdir -p /etc/network/interfaces.d
cat >/etc/network/interfaces.d/00-l3-seed <<EOF
auto $IFACE
iface $IFACE inet dhcp
EOF

systemctl stop systemd-networkd.socket systemd-networkd || true
systemctl mask systemd-networkd.socket systemd-networkd || true
ip addr flush dev "$IFACE" || true
ip link set "$IFACE" down || true
systemctl start networking || true
ifup --force "$IFACE" || true
i=0
while [ "$i" -lt 30 ]; do
  if ip -4 addr show dev "$IFACE" | grep -q 10.0.2.15; then
    break
  fi
  i=$((i + 1))
  sleep 1
done
echo "IFACE=$IFACE" > /tmp/ifupdown-switched
