# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine3.23 AS build
WORKDIR /src

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG appVersion="dev"
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOARM="${TARGETVARIANT#v}" CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.Version=${appVersion}" -o /out/libsql-bkup .

FROM alpine:3.23.4
ARG appVersion="dev"
ENV TZ=UTC
LABEL author="Jonas Kaninda" \
      version=${appVersion} \
      org.opencontainers.image.source="https://github.com/jkaninda/libsql-bkup" \
      org.opencontainers.image.version=${appVersion}

RUN apk add --no-cache tini=0.19.0-r3 tzdata ca-certificates && \
    mkdir -p /config /config/templates /backup /tmp/backup && \
    chmod 0777 /backup && chmod 1777 /tmp/backup

COPY --from=build /out/libsql-bkup /usr/local/bin/libsql-bkup


RUN ln -s /usr/local/bin/libsql-bkup /usr/local/bin/bkup && \
    printf '#!/bin/sh\nexec /usr/local/bin/libsql-bkup backup "$@"\n' > /usr/local/bin/backup && \
    printf '#!/bin/sh\nexec /usr/local/bin/libsql-bkup restore "$@"\n' > /usr/local/bin/restore && \
    chmod +x /usr/local/bin/backup /usr/local/bin/restore

WORKDIR /backup

HEALTHCHECK --interval=1m --timeout=10s --start-period=1m CMD ["/usr/local/bin/libsql-bkup", "health"]
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/libsql-bkup"]
