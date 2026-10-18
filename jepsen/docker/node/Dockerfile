# A Jepsen DB node for fix2: sshd, the tools the nemeses use, and LazyFS
# built where jepsen.lazyfs looks for it. Nothing is installed at test time:
# the suite mounts LazyFS without jepsen.lazyfs/install!, which would fetch
# and rebuild it.

FROM debian:trixie-slim AS lazyfs
ARG LAZYFS_VERSION=0.3.0
RUN apt-get -qy update && \
    apt-get -qy install --no-install-recommends ca-certificates cmake g++ git libfuse3-dev make pkg-config && \
    rm -rf /var/lib/apt/lists/*
RUN git clone --branch ${LAZYFS_VERSION} --depth 1 https://github.com/dsrhaslab/lazyfs.git /opt/jepsen/lazyfs
RUN cd /opt/jepsen/lazyfs/libs/libpcache && ./build.sh
RUN cd /opt/jepsen/lazyfs/lazyfs && ./build.sh

FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get -qy update && \
    apt-get -qy install --no-install-recommends \
        ca-certificates curl e2fsprogs fuse3 iproute2 iptables iputils-ping libfuse3-4 \
        netcat-openbsd openssh-server procps psmisc sqlite3 sudo tar util-linux && \
    rm -rf /var/lib/apt/lists/*
COPY --from=lazyfs /opt/jepsen/lazyfs /opt/jepsen/lazyfs
RUN sed -i '/\s*user_allow_other/s/^#//g' /etc/fuse.conf && \
    mkdir -p /run/sshd /root/.ssh && chmod 700 /root/.ssh
COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
