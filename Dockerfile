# syntax=docker/dockerfile:1.7
ARG GO_IMAGE=golang:1.24-alpine
ARG RUNTIME_IMAGE=alpine:3.22

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY ui.html ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/checker ./

FROM ${RUNTIME_IMAGE}
RUN apk add --no-cache ca-certificates tzdata wget && mkdir -p /data
COPY --from=build /out/checker /usr/local/bin/checker
ENV LISTEN=:8199 \
    STATE_DIR=/data \
    MAX_BATCH=64 \
    DEFAULT_TIMEOUT_MS=5000
EXPOSE 8199
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8199/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/usr/local/bin/checker"]
