# syntax=docker/dockerfile:1.7
# Xray VLESS + WebSocket node with a small Node.js front door.
#
# One image covers every node-* service:
#   node-us / node-eu / node-sg   PORT=8443 (volume optional)
#   node-sg-ws                    PORT=8080, volume /data
#
# The front door terminates plain HTTP/WS; TLS is provided by the platform
# ingress (the generated vless:// link therefore advertises :443 + security=tls).

ARG NODE_IMAGE=node:22-alpine
FROM ${NODE_IMAGE}

ARG XRAY_VERSION=v26.11.9
ARG TARGETARCH=amd64

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) xray_arch=64 ;; \
      arm64) xray_arch=arm64-v8a ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    apk add --no-cache ca-certificates curl unzip tzdata; \
    curl -fsSL --retry 3 -o /tmp/xray.zip \
      "https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}/Xray-linux-${xray_arch}.zip"; \
    unzip -q /tmp/xray.zip -d /usr/local/bin xray; \
    chmod +x /usr/local/bin/xray; \
    rm -f /tmp/xray.zip; \
    mkdir -p /app /data; \
    xray version | head -n 2

WORKDIR /app
COPY package.json server.js index.html ./

ENV PORT=8080 \
    XRAY_PORT=8081 \
    WS_PATH=/proxy \
    STATE_DIR=/data \
    PUBLIC_PORT=443 \
    PUBLIC_TLS=1

EXPOSE 8080 8443
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1 || exit 1

CMD ["node", "server.js"]
