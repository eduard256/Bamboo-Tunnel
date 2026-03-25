FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

# copy shared pkg first for better layer caching
COPY pkg/ ./pkg/
COPY server/ ./server/

WORKDIR /build/server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bamboo-server ./cmd/

FROM alpine:3.21

RUN apk add --no-cache iptables iproute2 ethtool

COPY --from=builder /bamboo-server /usr/local/bin/bamboo-server
COPY server/web/ /app/web/

WORKDIR /app

# TUN device requires NET_ADMIN capability
# run with: docker run --cap-add=NET_ADMIN --device=/dev/net/tun

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD wget -q --spider http://localhost:${BAMBOO_PORT:-8080}/ || exit 1

ENTRYPOINT ["bamboo-server"]
