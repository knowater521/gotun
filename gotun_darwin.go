package tun

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	bufSize = MaximumIPPacketSize + 4
)

const (
	appleUTUNCtl     = "com.apple.net.utun_control"
	appleCTLIOCGINFO = (0x40000000 | 0x80000000) | ((100 & 0x1fff) << 16) | uint32(byte('N'))<<8 | 3
)

type sockaddrCtl struct {
	scLen      uint8
	scFamily   uint8
	ssSysaddr  uint16
	scID       uint32
	scUnit     uint32
	scReserved [5]uint32
}

type utunDev struct {
	addr   string
	addrIP net.IP
	gw     string
	gwIP   net.IP

	rBuf [bufSize]byte
	wBuf [bufSize]byte

	baseDevice
}

func (dev *utunDev) Read(data []byte) (int, error) {
	n, err := dev.f.Read(dev.rBuf[:])
	if n > 0 {
		copy(data, dev.rBuf[4:n])
		n -= 4
	}
	if err != nil && dev.isClosed() {
		err = io.EOF
	}
	return n, err
}

// one packet, no more than MTU
func (dev *utunDev) Write(data []byte) (int, error) {
	_n := copy(dev.wBuf[4:], data)
	n, err := dev.f.Write(dev.wBuf[:_n+4])
	return n - 4, err
}

var sockaddrCtlSize uintptr = 32

func OpenTunDevice(name, addr, gw, mask string, mtu int) (io.ReadWriteCloser, error) {
	fd, err := OpenAndRegisterTunDevice(name, addr, gw, mask, mtu)
	if err != nil {
		return nil, err
	}

	return WrapTunDevice(fd, addr, gw)
}

func OpenAndRegisterTunDevice(name, addr, gw, mask string, mtu int) (int, error) {
	fd, err := syscall.Socket(syscall.AF_SYSTEM, syscall.SOCK_DGRAM, 2)
	if err != nil {
		return 0, err
	}

	var ctlInfo = &struct {
		ctlID   uint32
		ctlName [96]byte
	}{}
	copy(ctlInfo.ctlName[:], []byte(appleUTUNCtl))
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(appleCTLIOCGINFO), uintptr(unsafe.Pointer(ctlInfo)))
	if errno != 0 {
		return 0, fmt.Errorf("error in syscall.Syscall(syscall.SYS_IOTL, ...): %v", errno)
	}
	addrP := unsafe.Pointer(&sockaddrCtl{
		scLen:    uint8(sockaddrCtlSize),
		scFamily: syscall.AF_SYSTEM,
		/* #define AF_SYS_CONTROL 2 */
		ssSysaddr: 2,
		scID:      ctlInfo.ctlID,
		scUnit:    0,
	})
	_, _, errno = syscall.RawSyscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(addrP), uintptr(sockaddrCtlSize))
	if errno != 0 {
		return 0, fmt.Errorf("error in syscall.RawSyscall(syscall.SYS_CONNECT, ...): %v", errno)
	}

	ifName, err := getInterfaceName(fd)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command("ifconfig", ifName, "inet", addr, gw, "netmask", mask, "mtu", strconv.Itoa(mtu), "up")
	err = cmd.Run()
	if err != nil {
		syscall.Close(fd)
		return 0, err
	}

	return fd, nil
}

func WrapTunDevice(fd int, addr, gw string) (io.ReadWriteCloser, error) {
	ifName, err := getInterfaceName(fd)
	if err != nil {
		return nil, err
	}

	dev := &utunDev{
		baseDevice: baseDevice{
			f: os.NewFile(uintptr(fd), ifName),
		},
		addr:   addr,
		addrIP: net.ParseIP(addr),
		gw:     gw,
		gwIP:   net.ParseIP(gw),
	}
	copy(dev.wBuf[:], []byte{0, 0, 0, 2})
	return dev, nil
}

func getInterfaceName(fd int) (string, error) {
	var ifName struct {
		name [16]byte
	}
	ifNameSize := uintptr(16)
	_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(fd),
		2, /* #define SYSPROTO_CONTROL 2 */
		2, /* #define UTUN_OPT_IFNAME 2 */
		uintptr(unsafe.Pointer(&ifName)),
		uintptr(unsafe.Pointer(&ifNameSize)), 0)
	if errno != 0 {
		return "", fmt.Errorf("error in syscall.Syscall6(syscall.SYS_GETSOCKOPT, ...): %v", errno)
	}
	return string(ifName.name[:ifNameSize-1]), nil
}

func (dev *utunDev) Close() error {
	return dev.closeIfNecessary(func() error {
		return syscall.Close(int(dev.f.Fd()))
	})
}
