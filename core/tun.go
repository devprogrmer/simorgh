package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	ifNameSize = 16
	iffTUN     = 0x0001
	iffTAP     = 0x0002
	iffNoPI    = 0x1000
	tunSetIff  = 0x400454ca // TUNSETIFF, stable across Linux architectures
)

// ifreqFlags mirrors the parts of struct ifreq the kernel reads for
// TUNSETIFF. The kernel copies sizeof(struct ifreq) (40 bytes on Linux) out
// of user memory regardless of our declared struct size, so we pad to 40.
type ifreqFlags struct {
	Name  [ifNameSize]byte
	Flags uint16
	_     [22]byte
}

// openTunTap opens /dev/net/tun and binds it to a TUN (L3) or TAP (L2)
// interface with the requested name.
func openTunTap(name string, tap bool) (*os.File, string, error) {
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/net/tun: %w", err)
	}

	var req ifreqFlags
	copy(req.Name[:], name)
	if tap {
		req.Flags = iffTAP | iffNoPI
	} else {
		req.Flags = iffTUN | iffNoPI
	}

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(tunSetIff), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		syscall.Close(fd)
		return nil, "", fmt.Errorf("ioctl TUNSETIFF: %v", errno)
	}

	assigned := req.Name[:]
	if i := bytes.IndexByte(assigned, 0); i >= 0 {
		assigned = assigned[:i]
	}

	f := os.NewFile(uintptr(fd), "/dev/net/tun")
	return f, string(assigned), nil
}

func runIP(args ...string) error {
	cmd := exec.Command("ip", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %v: %v: %s", args, err, string(out))
	}
	return nil
}

func setLinkMAC(iface, mac string) error {
	if mac == "" {
		return nil
	}
	return runIP("link", "set", "dev", iface, "address", mac)
}

func setLinkMTU(iface string, mtu int) error {
	return runIP("link", "set", "dev", iface, "mtu", fmt.Sprintf("%d", mtu))
}

func setLinkUp(iface string) error {
	return runIP("link", "set", "dev", iface, "up")
}
