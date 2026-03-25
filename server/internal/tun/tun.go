package tun

import (
	"log/slog"

	"github.com/eduard256/Bamboo-Tunnel/pkg/tunnel"
	tunnelmod "github.com/eduard256/Bamboo-Tunnel/server/internal/tunnel"
)

const (
	serverIP  = "10.0.0.1"
	clientIP  = "10.0.0.2"
	subnet    = "10.0.0.0/24"
	mask      = "24"
	tunMTU    = 1400 // leave room for HTTP/2 + TLS overhead
)

var (
	log *slog.Logger
	dev *tunnel.TUN
)

func Init() {
	log = slog.Default().With("module", "tun")

	var err error
	dev, err = tunnel.NewTUN(serverIP, mask, tunMTU)
	if err != nil {
		log.Error("failed to create TUN device", "error", err)
		return
	}

	log.Info("TUN device created", "name", dev.Name(), "ip", serverIP, "mtu", tunMTU)

	// set tunnel module to write incoming packets to TUN
	tunnelmod.SetTUNWriter(func(pkt []byte) {
		// drop IPv6 packets (first nibble = 6)
		if len(pkt) > 0 && pkt[0]>>4 == 6 {
			return
		}
		dev.Write(pkt)
	})

	// read packets from TUN and send through tunnel
	go readLoop()
}

func readLoop() {
	buf := make([]byte, tunMTU+100) // slightly larger than MTU
	for {
		n, err := dev.Read(buf)
		if err != nil {
			log.Error("TUN read error", "error", err)
			continue
		}
		if n == 0 {
			continue
		}

		pkt := buf[:n]

		// drop IPv6
		if pkt[0]>>4 == 6 {
			continue
		}

		tunnelmod.SendPacket(pkt)
	}
}
