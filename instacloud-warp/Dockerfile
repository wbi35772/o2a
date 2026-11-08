# syntax=docker/dockerfile:1.7

ARG GO_IMAGE=golang:1.26-alpine
ARG RUNTIME_IMAGE=alpine:3.20
ARG APK_MIRROR_HOST=dl-cdn.alpinelinux.org

FROM ${GO_IMAGE} AS build

ARG WGCF_REPO=https://github.com/ViRb3/wgcf.git
ARG WGCF_REF=v2.3.1
ARG WIREPROXY_REPO=https://github.com/windtf/wireproxy.git
ARG WIREPROXY_REF=35cce290db2f32d5e4316bd54102186f486d0af4
ARG APK_MIRROR_HOST
ARG GOPROXY_URL=https://proxy.golang.org,direct

ENV GOPROXY=${GOPROXY_URL}

RUN if [ "${APK_MIRROR_HOST}" != "dl-cdn.alpinelinux.org" ]; then \
      sed -i "s#dl-cdn.alpinelinux.org#${APK_MIRROR_HOST}#g" /etc/apk/repositories; \
    fi && \
    apk add --no-cache git ca-certificates
WORKDIR /src

RUN git clone --depth 1 --branch "${WGCF_REF}" "${WGCF_REPO}" wgcf
COPY patches/wgcf-socks5-proxy.patch /tmp/wgcf-socks5-proxy.patch
RUN mkdir -p /out && cd wgcf && git apply --verbose /tmp/wgcf-socks5-proxy.patch && \
    CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /out/wgcf .

RUN mkdir -p /out && git clone "${WIREPROXY_REPO}" wireproxy && \
    cd wireproxy && git checkout "${WIREPROXY_REF}" && \
    CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /out/wireproxy ./cmd/wireproxy

COPY socksmux /src/socksmux
RUN cd /src/socksmux && CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /out/socksmux .

FROM ${RUNTIME_IMAGE}

ARG APK_MIRROR_HOST

RUN if [ "${APK_MIRROR_HOST}" != "dl-cdn.alpinelinux.org" ]; then \
      sed -i "s#dl-cdn.alpinelinux.org#${APK_MIRROR_HOST}#g" /etc/apk/repositories; \
    fi && \
    apk add --no-cache bash curl ca-certificates tzdata jq && \
    addgroup -S warp && adduser -S -G warp -h /data warp && \
    mkdir -p /data/instances /usr/local/share/warp-pool && \
    chown -R warp:warp /data

COPY --from=build /out/wgcf /usr/local/bin/wgcf
COPY --from=build /out/wireproxy /usr/local/bin/wireproxy
COPY --from=build /out/socksmux /usr/local/bin/socksmux
COPY endpoints.txt /usr/local/share/warp-pool/endpoints.txt
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY healthcheck.sh /usr/local/bin/healthcheck.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh /usr/local/bin/healthcheck.sh

ENV STATE_DIR=/data \
    START_PORT=1081 \
    POOL_SIZE=16 \
    INSTANCE_COUNT=16 \
    REROLL_INTERVAL=900 \
    REROLL_BATCH=4 \
    DEDUP_RETRIES=4 \
    PROBE_URL=https://opencode.ai/cdn-cgi/trace \
    PROBE_TIMEOUT=6 \
    HEALTH_INTERVAL=300

USER warp
VOLUME ["/data"]
EXPOSE 1081-1096

HEALTHCHECK --interval=60s --timeout=15s --start-period=180s --retries=3 \
  CMD ["/usr/local/bin/healthcheck.sh"]

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
