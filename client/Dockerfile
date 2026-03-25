FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

# copy shared pkg first for better layer caching
COPY pkg/ ./pkg/
COPY client/ ./client/

WORKDIR /build/client
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bamboo-client ./cmd/

FROM alpine:3.21

RUN apk add --no-cache iptables iproute2 ethtool

COPY --from=builder /bamboo-client /usr/local/bin/bamboo-client

# TUN device requires NET_ADMIN capability
# run with: docker run --cap-add=NET_ADMIN --device=/dev/net/tun

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD pgrep bamboo-client || exit 1

ENTRYPOINT ["bamboo-client"]
