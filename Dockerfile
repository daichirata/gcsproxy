FROM debian:buster-slim AS build

WORKDIR /tmp
ENV GCSPROXY_VERSION=0.3.1

RUN apt-get update \
    && apt-get install --no-install-suggests --no-install-recommends --yes ca-certificates wget \
    && wget https://github.com/daichirata/gcsproxy/releases/download/v${GCSPROXY_VERSION}/gcsproxy-${GCSPROXY_VERSION}-linux-amd64.tar.gz \
    && tar zxf gcsproxy-${GCSPROXY_VERSION}-linux-amd64.tar.gz \
    && cp ./gcsproxy-${GCSPROXY_VERSION}-linux-amd64/gcsproxy .

FROM gcr.io/distroless/base
COPY --from=build /tmp/gcsproxy /gcsproxy
CMD ["/gcsproxy"]
