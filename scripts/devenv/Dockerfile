# The loop-device lab image (doc 06 §3, Q45). Built and run only through
# `make lab-up` / docker-compose.dev.yml — never `docker run` ad hoc, since
# starting it needs the (root-equivalent) Docker daemon.
FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        util-linux \
        xfsprogs \
        mergerfs \
        fuse3 \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/hoserva-lab-entrypoint
RUN chmod +x /usr/local/bin/hoserva-lab-entrypoint

ENTRYPOINT ["/usr/local/bin/hoserva-lab-entrypoint"]
CMD ["sleep", "infinity"]
