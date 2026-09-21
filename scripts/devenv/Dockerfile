# The loop-device lab image (doc 06 §3, Q45). Built and run only through
# `make lab-up` / docker-compose.dev.yml — never `docker run` ad hoc, since
# starting it needs the (root-equivalent) Docker daemon.
FROM debian:trixie-slim

# snapraid, e2fsprogs and btrfs-progs are here rather than installed per-run
# (issue #121): doc 06 §3 describes L2 as the layer that covers SnapRAID's
# sync/diff/scrub/fix behaviour, and Q23 plans ext4 and single-device btrfs
# data disks, so every spike that needed them was installing them by hand
# inside its own container — and CI's lab job, which uses this image as-is,
# never exercised SnapRAID at all.
#
# samba and smbclient are here for the same reason (issue #219): doc 06 §3's
# L2 harness had no way to run smbd against a Hoserva-generated smb.conf, so
# #48's own Samba ownership/mask work had nothing to verify against in the
# lab. samba pulls in samba-common-bin (smbpasswd, pdbedit) as a dependency;
# smbclient is the client side scripts/devenv/smb-check.sh drives against it.
#
# The trade-off, stated so it isn't lost: a passing lab run now demonstrates
# the recipe on Hoserva's own image, not on a near-stock debian:trixie-slim.
# Versions still come from Debian 13 (Q7), never pinned here.
RUN apt-get update && apt-get install -y --no-install-recommends \
        util-linux \
        xfsprogs \
        e2fsprogs \
        btrfs-progs \
        mergerfs \
        snapraid \
        fuse3 \
        samba \
        smbclient \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/hoserva-lab-entrypoint
RUN chmod +x /usr/local/bin/hoserva-lab-entrypoint

ENTRYPOINT ["/usr/local/bin/hoserva-lab-entrypoint"]
CMD ["sleep", "infinity"]
