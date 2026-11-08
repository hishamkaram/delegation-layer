FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install --no-install-recommends -y openssh-server python3 ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/sshd \
    && passwd -d root
ENTRYPOINT ["/usr/sbin/sshd", "-D", "-e", "-f", "/remote/sshd_config"]
