# Simorgh Gaming Tunnel - core image
# Built entirely from the Go source in this directory. install.sh runs
# `docker build` against this folder; nothing is ever pulled from a registry.

FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /out/simorgh-core .

FROM alpine:3.20
RUN apk add --no-cache iproute2 iptables
COPY --from=builder /out/simorgh-core /usr/local/bin/simorgh-core
# CAP_NET_ADMIN (interface/route config) and CAP_NET_RAW (raw ICMP socket)
# are granted at `docker run` time by install.sh.
ENTRYPOINT ["/usr/local/bin/simorgh-core"]
