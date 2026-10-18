# The Jepsen control node: a JDK, Leiningen, gnuplot and graphviz for the
# reports, and the suite's dependencies fetched at build time.

FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive LEIN_ROOT=true
ARG LEIN_VERSION=2.11.2
RUN apt-get -qy update && \
    apt-get -qy install --no-install-recommends \
        ca-certificates curl git gnuplot-nox graphviz libjna-java openjdk-25-jdk-headless openssh-client && \
    rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /usr/local/bin/lein https://raw.githubusercontent.com/technomancy/leiningen/${LEIN_VERSION}/bin/lein && \
    chmod +x /usr/local/bin/lein && \
    lein version
WORKDIR /jepsen
COPY project.clj /jepsen/project.clj
RUN lein deps
COPY --chmod=0755 docker/control/entrypoint.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
