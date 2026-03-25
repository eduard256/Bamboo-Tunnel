# Bamboo Tunnel

Reverse tunnel system that bypasses internet censorship by disguising VPN traffic as video call streaming.

Unlike traditional VPNs where you connect FROM a censored network TO a foreign server, Bamboo Tunnel inverts the model: the foreign VPS connects TO your home server. For censorship systems (DPI), this looks like someone abroad visiting a Russian website for video calls.

## How It Works

```
[Devices at home]        [Home Server (RU)]              [VPS Abroad]
                              |         |                      |
get DHCP IP          LAN port |  WAN port (Caddy)             |
10.98.10.x ------> enp6s19   |  -> HTTPS                     |
gw=10.98.10.1                |         |                      |
                         iptables FORWARD                     |
                         LAN -> TUN (172.29.0.1)              |
                              |                               |
                              |---- HTTP/2 tunnel ----------> TUN (172.29.0.2)
                              |    (looks like video call)    NAT -> internet
                              |                               |
                              |                        [Unrestricted internet]
```

**Server** runs at home where internet is censored. It accepts tunnel connections from the VPS, serves a fake video meeting landing page as cover, and runs a DHCP server on its LAN port. Any device connected to the LAN port gets internet through the VPS.

**Client** runs on a foreign VPS with unrestricted internet. It connects to the home server through Caddy reverse proxy, creates a TUN device, and provides NAT exit for all tunneled traffic.

## Traffic Disguise

For DPI and network monitoring, Bamboo Tunnel traffic looks like a video conferencing service:

- HTTP/2 bidirectional streaming (like WebRTC data channels)
- `Content-Type: application/x-protobuf` (same as Google Meet, Zoom Web)
- Chrome User-Agent with all `Sec-*` headers
- Fake video call handshake on connect (room.join, media.config, room.ready)
- Heartbeat packets disguised as audio silence frames (60-150 bytes, 2-5s interval)
- Random API paths per session (`/odr/v1/call/{random-hex}/stream`)
- Token auth via standard session cookie (`_sid`)
- Landing page looks like a real video meeting service

## Performance

Tested on 189 Mbit/s home connection with VPS in Europe:

| Metric | Value |
|--------|-------|
| Throughput (sustained) | 110-150 Mbit/s |
| Throughput (peak) | 190+ Mbit/s |
| Ping latency | 70-80 ms |
| Stability | 3+ GB without drops |

## Quick Start

### Prerequisites

- Home server with two network ports (one for internet, one for LAN devices)
- Foreign VPS with unrestricted internet
- Domain name pointing to your home IP (with Caddy or similar reverse proxy)
- Docker on both machines

### 1. Server (Home)

Create a directory and config files:

```bash
mkdir -p ~/bamboo-server/web
cd ~/bamboo-server
```

Create `.env`:

```env
BAMBOO_TOKEN=your_long_random_token_here
BAMBOO_PORT=8080
BAMBOO_WEB_DIR=./web
BAMBOO_LAN_IFACE=enp6s19
BAMBOO_LAN_SUBNET=10.98.10.0/24
BAMBOO_LAN_GATEWAY=10.98.10.1
BAMBOO_LAN_DHCP_START=10.98.10.100
BAMBOO_LAN_DHCP_END=10.98.10.200
BAMBOO_LOG_LEVEL=info
```

Create `docker-compose.yml`:

```yaml
services:
  bamboo-server:
    image: eduard256/bamboo-server:latest
    container_name: bamboo-server
    restart: always
    network_mode: host
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
    env_file:
      - .env
    volumes:
      - ./web:/app/web:ro
```

Ensure IP forwarding is enabled and the second network interface is up:

```bash
sudo sysctl -w net.ipv4.ip_forward=1
sudo ip link set enp6s19 up
```

Start:

```bash
docker compose up -d
```

### 2. Caddy (Reverse Proxy)

Add to your Caddyfile on the reverse proxy server:

```
bamboo.yourdomain.com {
    reverse_proxy your-home-server-ip:8080 {
        flush_interval -1
        transport http {
            read_timeout 0
            write_timeout 0
            response_header_timeout 0
        }
    }
}
```

Reload Caddy:

```bash
sudo systemctl reload caddy
```

### 3. Client (VPS)

```bash
mkdir -p ~/bamboo-client
cd ~/bamboo-client
```

Create `.env`:

```env
BAMBOO_SERVER_URL=https://bamboo.yourdomain.com
BAMBOO_TOKEN=your_long_random_token_here
BAMBOO_TUNNEL_PATH=/odr/v1/call
BAMBOO_LOG_LEVEL=info
```

Create `docker-compose.yml`:

```yaml
services:
  bamboo-client:
    image: eduard256/bamboo-client:latest
    container_name: bamboo-client
    restart: always
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
    env_file:
      - .env
    sysctls:
      - net.ipv4.ip_forward=1
```

Start:

```bash
docker compose up -d
```

### 4. Verify

On the home server:

```bash
# Check tunnel is up
ping 172.29.0.2

# Check traffic goes through VPS
ip route add <some-ip>/32 via 172.29.0.2 dev bamboo0
curl https://ipinfo.io/ip
# Should show VPS IP
```

Connect any device to the second network port of the home server. It should get a DHCP address (10.98.10.x) and all its traffic will go through the VPS.

## Architecture

```
Bamboo-Tunnel/
  pkg/                    # shared libraries
    protocol/             # frame format, coalescing, heartbeat, handshake
    crypto/               # token auth via cookie
    tunnel/               # TUN device, ring buffer
  server/                 # home server
    cmd/main.go
    internal/tunnel/      # HTTP handler, batch writer, download/upload streams
    internal/web/         # static files, fake API responses
    internal/tun/         # TUN device, DHCP (dnsmasq), iptables, policy routing
  client/                 # VPS client
    cmd/main.go
    internal/tunnel/      # session management, batch upload, browser mimicry
    internal/tun/         # TUN device, NAT masquerade
```

## Configuration Reference

### Server (.env)

| Variable | Default | Description |
|----------|---------|-------------|
| `BAMBOO_TOKEN` | (required) | Shared secret for tunnel auth |
| `BAMBOO_PORT` | `8080` | HTTP listen port |
| `BAMBOO_WEB_DIR` | `./web` | Path to static web files |
| `BAMBOO_LAN_IFACE` | `eth1` | Second network interface for LAN |
| `BAMBOO_LAN_SUBNET` | `192.168.99.0/24` | LAN subnet |
| `BAMBOO_LAN_GATEWAY` | `192.168.99.1` | LAN gateway (this server) |
| `BAMBOO_LAN_DHCP_START` | `192.168.99.100` | DHCP range start |
| `BAMBOO_LAN_DHCP_END` | `192.168.99.200` | DHCP range end |
| `BAMBOO_LOG_LEVEL` | `info` | Log level: debug, info, warn, error |

### Client (.env)

| Variable | Default | Description |
|----------|---------|-------------|
| `BAMBOO_SERVER_URL` | (required) | Server URL with HTTPS |
| `BAMBOO_TOKEN` | (required) | Shared secret, must match server |
| `BAMBOO_TUNNEL_PATH` | `/odr/v1/call` | Tunnel API path prefix |
| `BAMBOO_LOG_LEVEL` | `info` | Log level: debug, info, warn, error |

## Docker Images

```bash
docker pull eduard256/bamboo-server:latest
docker pull eduard256/bamboo-client:latest
```

Both require `--cap-add=NET_ADMIN` and `--device=/dev/net/tun`.

## License

MIT
