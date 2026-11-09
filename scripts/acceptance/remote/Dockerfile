FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
RUN apt-get update \
    && apt-get install --no-install-recommends -y openssh-server python3 ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/sshd \
    && passwd -d root
ENTRYPOINT ["/usr/sbin/sshd", "-D", "-e", "-f", "/remote/sshd_config"]
