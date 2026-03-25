package tun

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"github.com/eduard256/Bamboo-Tunnel/pkg/tunnel"
	tunnelmod "github.com/eduard256/Bamboo-Tunnel/server/internal/tunnel"
)

const (
	// TUN tunnel endpoint (server <-> VPS client)
	tunIP  = "172.29.0.1"
	tunMask = "30"
	tunMTU = 1400
)

var (
	log *slog.Logger
	dev *tunnel.TUN
)

func Init() {
	log = slog.Default().With("module", "tun")

	// DHCP network for local devices (second interface)
	dhcpSubnet := os.Getenv("BAMBOO_LAN_SUBNET")
	if dhcpSubnet == "" {
		dhcpSubnet = "192.168.99.0/24"
	}
	dhcpGateway := os.Getenv("BAMBOO_LAN_GATEWAY")
	if dhcpGateway == "" {
		dhcpGateway = "192.168.99.1"
	}
	dhcpRangeStart := os.Getenv("BAMBOO_LAN_DHCP_START")
	if dhcpRangeStart == "" {
		dhcpRangeStart = "192.168.99.100"
	}
	dhcpRangeEnd := os.Getenv("BAMBOO_LAN_DHCP_END")
	if dhcpRangeEnd == "" {
		dhcpRangeEnd = "192.168.99.200"
	}
	lanIface := os.Getenv("BAMBOO_LAN_IFACE")
	if lanIface == "" {
		lanIface = "eth1"
	}

	// Step 1: create TUN device for tunnel to VPS
	var err error
	dev, err = tunnel.NewTUN(tunIP, tunMask, tunMTU)
	if err != nil {
		log.Error("failed to create TUN device", "error", err)
		return
	}
	log.Info("TUN device created", "name", dev.Name(), "ip", tunIP, "mtu", tunMTU)

	// Step 2: configure LAN interface for DHCP network
	if err := configureLAN(lanIface, dhcpGateway, dhcpSubnet); err != nil {
		log.Error("failed to configure LAN", "error", err)
	} else {
		log.Info("LAN interface configured", "iface", lanIface, "gateway", dhcpGateway, "subnet", dhcpSubnet)
	}

	// Step 3: enable IP forwarding
	if err := tunnel.EnableForwarding(); err != nil {
		log.Error("failed to enable IP forwarding", "error", err)
	}

	// Step 4: route LAN traffic through TUN (to VPS)
	if err := routeLANThroughTUN(dhcpSubnet, dev.Name()); err != nil {
		log.Error("failed to setup routing", "error", err)
	} else {
		log.Info("routing configured", "from", dhcpSubnet, "via", dev.Name())
	}

	// Step 5: start DHCP server on LAN interface
	go startDHCP(lanIface, dhcpGateway, dhcpSubnet, dhcpRangeStart, dhcpRangeEnd)

	// Step 6: connect TUN read/write to tunnel module
	tunnelmod.SetTUNWriter(func(pkt []byte) {
		if len(pkt) > 0 && pkt[0]>>4 == 6 {
			return
		}
		dev.Write(pkt)
	})

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
		if pkt[0]>>4 == 6 {
			continue
		}

		log.Info("TUN read", "bytes", n, "dst", fmt.Sprintf("%d.%d.%d.%d", pkt[16], pkt[17], pkt[18], pkt[19]))
		tunnelmod.SendPacket(pkt)
	}
}

func configureLAN(iface, gateway, subnet string) error {
	// assign IP to LAN interface
	if err := run("ip", "addr", "add", gateway+"/24", "dev", iface); err != nil {
		return fmt.Errorf("set addr: %w", err)
	}
	if err := run("ip", "link", "set", iface, "up"); err != nil {
		return fmt.Errorf("bring up: %w", err)
	}
	return nil
}

func routeLANThroughTUN(lanSubnet, tunDev string) error {
	// traffic from LAN devices goes through TUN to VPS
	// iptables FORWARD: allow LAN -> TUN
	if err := run("iptables", "-A", "FORWARD", "-i", "eth1", "-o", tunDev, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("forward out: %w", err)
	}
	// iptables FORWARD: allow TUN -> LAN (return traffic)
	if err := run("iptables", "-A", "FORWARD", "-i", tunDev, "-o", "eth1", "-m", "state",
		"--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("forward in: %w", err)
	}
	// NAT: masquerade LAN traffic going through TUN
	if err := run("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", tunDev, "-j", "MASQUERADE"); err != nil {
		return fmt.Errorf("masquerade: %w", err)
	}
	return nil
}

// startDHCP runs dnsmasq as DHCP server on the LAN interface.
// dnsmasq is lightweight and perfect for this -- single binary, no config files needed.
func startDHCP(iface, gateway, subnet, rangeStart, rangeEnd string) {
	// dnsmasq arguments:
	// --no-daemon: foreground
	// --interface: listen only on LAN
	// --dhcp-range: IP range with 12h lease
	// --dhcp-option=3: gateway (this server)
	// --dhcp-option=6: DNS servers (use public DNS via tunnel)
	// --no-resolv: don't read /etc/resolv.conf
	// --log-dhcp: log DHCP transactions
	args := []string{
		"--no-daemon",
		"--interface=" + iface,
		"--bind-interfaces",
		"--dhcp-range=" + rangeStart + "," + rangeEnd + ",255.255.255.0,12h",
		"--dhcp-option=3," + gateway,
		"--dhcp-option=6,8.8.8.8,1.1.1.1",
		"--no-resolv",
		"--log-dhcp",
	}

	log.Info("starting DHCP server", "iface", iface, "range", rangeStart+"-"+rangeEnd, "gateway", gateway)

	cmd := exec.Command("dnsmasq", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Error("DHCP server exited", "error", err)
	}
}

func run(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}
