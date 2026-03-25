package tunnel

import (
	"fmt"
	"net"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	tunDevice = "/dev/net/tun"
	ifnamsiz  = 16
)

// TUN represents a TUN network device
type TUN struct {
	fd   int
	name string
	mtu  int
}

// NewTUN creates and configures a TUN device with given IP and MTU.
// Returns ready-to-use TUN device with routing configured.
func NewTUN(ip string, mask string, mtu int) (*TUN, error) {
	fd, err := unix.Open(tunDevice, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("tun: open %s: %w", tunDevice, err)
	}

	// allocate TUN device
	var ifr [ifnamsiz + 16]byte
	copy(ifr[:], "bamboo0")
	// IFF_TUN | IFF_NO_PI
	*(*uint16)(unsafe.Pointer(&ifr[ifnamsiz])) = unix.IFF_TUN | unix.IFF_NO_PI

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TUNSETIFF, uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("tun: TUNSETIFF: %w", errno)
	}

	name := string(ifr[:len("bamboo0")])

	t := &TUN{fd: fd, name: name, mtu: mtu}

	if err := t.configure(ip, mask, mtu); err != nil {
		t.Close()
		return nil, err
	}

	return t, nil
}

func (t *TUN) configure(ip string, mask string, mtu int) error {
	// set IP address
	if err := run("ip", "addr", "add", ip+"/"+mask, "dev", t.name); err != nil {
		return fmt.Errorf("tun: set addr: %w", err)
	}
	// set MTU
	if err := run("ip", "link", "set", t.name, "mtu", fmt.Sprint(mtu)); err != nil {
		return fmt.Errorf("tun: set mtu: %w", err)
	}
	// bring up
	if err := run("ip", "link", "set", t.name, "up"); err != nil {
		return fmt.Errorf("tun: bring up: %w", err)
	}
	// disable TCP offloading to avoid TCP-in-TCP meltdown
	_ = run("ethtool", "-K", t.name, "tx", "off", "rx", "off")
	return nil
}

// Read reads one IP packet from TUN. Returns bytes read.
func (t *TUN) Read(buf []byte) (int, error) {
	return unix.Read(t.fd, buf)
}

// Write writes one IP packet to TUN. Returns bytes written.
func (t *TUN) Write(buf []byte) (int, error) {
	return unix.Write(t.fd, buf)
}

// Close shuts down the TUN device
func (t *TUN) Close() error {
	return unix.Close(t.fd)
}

func (t *TUN) Name() string { return t.name }
func (t *TUN) MTU() int     { return t.mtu }

// SetNonBlock enables non-blocking I/O on the TUN fd
func (t *TUN) SetNonBlock(nonblock bool) error {
	return unix.SetNonblock(t.fd, nonblock)
}

// Fd returns the file descriptor for polling
func (t *TUN) Fd() int { return t.fd }

func run(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// EnableForwarding enables IP forwarding on the system
func EnableForwarding() error {
	return run("sysctl", "-w", "net.ipv4.ip_forward=1")
}

// SetupNAT configures iptables masquerade for traffic from TUN subnet
func SetupNAT(subnet string, iface string) error {
	return run("iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", subnet, "-o", iface, "!", "-d", subnet,
		"-j", "MASQUERADE")
}

// AddRoute adds a route via the TUN device
func AddRoute(dst string, via net.IP, dev string) error {
	args := []string{"route", "add", dst}
	if via != nil {
		args = append(args, "via", via.String())
	}
	args = append(args, "dev", dev)
	return run("ip", args...)
}
