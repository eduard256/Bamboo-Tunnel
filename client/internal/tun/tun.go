package tun

import (
	"log/slog"
	"os/exec"
	"strings"

	"github.com/eduard256/Bamboo-Tunnel/pkg/tunnel"
	tunnelmod "github.com/eduard256/Bamboo-Tunnel/client/internal/tunnel"
)

const (
	clientIP = "172.29.0.2"
	mask     = "30"
	subnet   = "172.29.0.0/30"
	tunMTU   = 1400
)

var (
	log *slog.Logger
	dev *tunnel.TUN
)

func Init() {
	log = slog.Default().With("module", "tun")

	var err error
	dev, err = tunnel.NewTUN(clientIP, mask, tunMTU)
	if err != nil {
		log.Error("failed to create TUN device", "error", err)
		return
	}

	log.Info("TUN device created", "name", dev.Name(), "ip", clientIP, "mtu", tunMTU)

	// enable IP forwarding
	if err := tunnel.EnableForwarding(); err != nil {
		log.Error("failed to enable IP forwarding", "error", err)
	}

	// setup NAT masquerade on default interface
	iface := defaultInterface()
	if iface != "" {
		if err := tunnel.SetupNAT(subnet, iface); err != nil {
			log.Error("failed to setup NAT", "error", err, "iface", iface)
		} else {
			log.Info("NAT configured", "iface", iface, "subnet", subnet)
		}
	}

	// set tunnel module to write incoming packets to TUN
	tunnelmod.SetTUNWriter(func(pkt []byte) {
		// drop IPv6
		if len(pkt) > 0 && pkt[0]>>4 == 6 {
			return
		}
		dev.Write(pkt)
	})

	// read packets from TUN and send through tunnel
	go readLoop()
}

func readLoop() {
	buf := make([]byte, tunMTU+100)
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

// defaultInterface returns the name of the default network interface
func defaultInterface() string {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return "eth0"
	}
	// parse "default via X.X.X.X dev ethN ..."
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return "eth0"
}
