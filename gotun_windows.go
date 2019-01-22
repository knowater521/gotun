package tun

import (
	"encoding/binary"
	// "encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	TAPWIN32_MAX_REG_SIZE    = 256
	TUNTAP_COMPONENT_ID_0901 = "tap0901"
	TUNTAP_COMPONENT_ID_0801 = "tap0801"
	NETWORK_KEY              = `SYSTEM\\CurrentControlSet\\Control\\Network\\{4D36E972-E325-11CE-BFC1-08002BE10318}`
	ADAPTER_KEY              = `SYSTEM\CurrentControlSet\Control\Class\{4D36E972-E325-11CE-BFC1-08002BE10318}`
)

func ctl_code(device_type, function, method, access uint32) uint32 {
	return (device_type << 16) | (access << 14) | (function << 2) | method
}

func tap_control_code(request, method uint32) uint32 {
	return ctl_code(34, request, method, 0)
}

var (
	k32                               = windows.NewLazySystemDLL("kernel32.dll")
	procGetOverlappedResult           = k32.NewProc("GetOverlappedResult")
	TAP_IOCTL_GET_MTU                 = tap_control_code(3, 0)
	TAP_IOCTL_SET_MEDIA_STATUS        = tap_control_code(6, 0)
	TAP_IOCTL_CONFIG_TUN              = tap_control_code(10, 0)
	TAP_WIN_IOCTL_CONFIG_DHCP_MASQ    = tap_control_code(7, 0)
	TAP_WIN_IOCTL_CONFIG_DHCP_SET_OPT = tap_control_code(9, 0)
)

func decodeUTF16(b []byte) string {
	if len(b)%2 != 0 {
		return ""
	}

	l := len(b) / 2
	u16 := make([]uint16, l)
	for i := 0; i < l; i += 1 {
		u16[i] = uint16(b[2*i]) + (uint16(b[2*i+1]) << 8)
	}
	return windows.UTF16ToString(u16)
}

func getTuntapName(componentId string) (string, error) {
	keyName := fmt.Sprintf(NETWORK_KEY+"\\%s\\Connection", componentId)
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, keyName, registry.READ)
	if err != nil {
		key.Close()
		return "", err
	}
	var bufLength uint32 = TAPWIN32_MAX_REG_SIZE
	buf := make([]byte, bufLength)
	name, _ := windows.UTF16FromString("Name")
	var valtype uint32
	err = windows.RegQueryValueEx(
		windows.Handle(key),
		&name[0],
		nil,
		&valtype,
		&buf[0],
		&bufLength,
	)
	if err != nil {
		key.Close()
		return "", err
	}
	s := decodeUTF16(buf)
	return s, nil
}

func getTuntapComponentId() (string, error) {
	adapters, err := registry.OpenKey(registry.LOCAL_MACHINE, ADAPTER_KEY, registry.READ)
	if err != nil {
		return "", err
	}
	var i uint32
	for i = 0; i < 1000; i++ {
		var name_length uint32 = TAPWIN32_MAX_REG_SIZE
		buf := make([]uint16, name_length)
		if err = windows.RegEnumKeyEx(
			windows.Handle(adapters),
			i,
			&buf[0],
			&name_length,
			nil,
			nil,
			nil,
			nil); err != nil {
			return "", err
		}
		key_name := windows.UTF16ToString(buf[:])
		adapter, err := registry.OpenKey(adapters, key_name, registry.READ)
		if err != nil {
			continue
		}
		name, _ := windows.UTF16FromString("ComponentId")
		name2, _ := windows.UTF16FromString("NetCfgInstanceId")
		var valtype uint32
		var component_id = make([]byte, TAPWIN32_MAX_REG_SIZE)
		var componentLen = uint32(len(component_id))
		if err = windows.RegQueryValueEx(
			windows.Handle(adapter),
			&name[0],
			nil,
			&valtype,
			&component_id[0],
			&componentLen); err != nil {
			continue
		}

		id := decodeUTF16(component_id)
		if id == TUNTAP_COMPONENT_ID_0901 || id == TUNTAP_COMPONENT_ID_0801 {
			var valtype uint32
			var netCfgInstanceId = make([]byte, TAPWIN32_MAX_REG_SIZE)
			var netCfgInstanceIdLen = uint32(len(netCfgInstanceId))
			if err = windows.RegQueryValueEx(
				windows.Handle(adapter),
				&name2[0],
				nil,
				&valtype,
				&netCfgInstanceId[0],
				&netCfgInstanceIdLen); err != nil {
				return "", err
			}
			s := decodeUTF16(netCfgInstanceId)
			log.Debugf("device component id: %s", s)
			adapter.Close()
			adapters.Close()
			return s, nil
		}
		adapter.Close()
	}
	adapters.Close()
	return "", errors.New("not found component id")
}

func OpenTunDevice(name, addr, gw, mask string, dns []string) (TUNDevice, error) {
	componentId, err := getTuntapComponentId()
	if err != nil {
		return nil, err
	}

	devId, _ := windows.UTF16FromString(fmt.Sprintf(`\\.\Global\%s.tap`, componentId))
	devName, err := getTuntapName(componentId)
	log.Debugf("device name: %s", devName)
	// set dhcp with netsh
	cmd := exec.Command("netsh", "interface", "ip", "set", "address", devName, "dhcp")
	cmd.Run()
	cmd = exec.Command("netsh", "interface", "ip", "set", "dns", devName, "dhcp")
	cmd.Run()
	// open
	fd, err := windows.CreateFile(
		&devId[0],
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_SYSTEM|windows.FILE_FLAG_OVERLAPPED,
		//windows.FILE_ATTRIBUTE_SYSTEM,
		0,
	)
	if err != nil {
		return nil, err
	}
	// set addresses with dhcp
	var returnLen uint32
	tunAddr := parseIPv4(addr)
	tunMask := parseIPv4(mask)
	gwAddr := parseIPv4(gw)
	addrParam := append(tunAddr, tunMask...)
	addrParam = append(addrParam, gwAddr...)
	lease := make([]byte, 4)
	binary.BigEndian.PutUint32(lease[:], 86400)
	addrParam = append(addrParam, lease...)
	err = windows.DeviceIoControl(
		fd,
		TAP_WIN_IOCTL_CONFIG_DHCP_MASQ,
		&addrParam[0],
		uint32(len(addrParam)),
		&addrParam[0],
		uint32(len(addrParam)),
		&returnLen,
		nil,
	)
	if err != nil {
		windows.Close(fd)
		return nil, err
	} else {
		log.Debugf("set %s with net/mask: %s/%s through DHCP", devName, addr, mask)
	}

	// set dns with dncp
	dnsParam := []byte{6, 4}
	primaryDNS := parseIPv4(dns[0])
	dnsParam = append(dnsParam, primaryDNS...)
	if len(dns) >= 2 {
		secondaryDNS := parseIPv4(dns[1])
		dnsParam = append(dnsParam, secondaryDNS...)
		dnsParam[1] += 4
	}
	err = windows.DeviceIoControl(
		fd,
		TAP_WIN_IOCTL_CONFIG_DHCP_SET_OPT,
		&dnsParam[0],
		uint32(len(dnsParam)),
		&addrParam[0],
		uint32(len(dnsParam)),
		&returnLen,
		nil,
	)
	if err != nil {
		windows.Close(fd)
		return nil, err
	} else {
		log.Debugf("set %s with dns: %s through DHCP", devName, strings.Join(dns, ","))
	}

	// set connect.
	inBuffer := []byte("\x01\x00\x00\x00")
	err = windows.DeviceIoControl(
		fd,
		TAP_IOCTL_SET_MEDIA_STATUS,
		&inBuffer[0],
		uint32(len(inBuffer)),
		&inBuffer[0],
		uint32(len(inBuffer)),
		&returnLen,
		nil,
	)
	if err != nil {
		windows.Close(fd)
		return nil, err
	}
	return newWinTapDev(fd, addr, gw), nil
}

type winTapDev struct {
	fd          windows.Handle
	addr        string
	addrIP      net.IP
	gw          string
	gwIP        net.IP
	rBuf        [2048]byte
	wBuf        [2048]byte
	wInitiated  bool
	rOverlapped windows.Overlapped
	wOverlapped windows.Overlapped
	stopped     int64
}

func newWinTapDev(fd windows.Handle, addr string, gw string) *winTapDev {
	rOverlapped := windows.Overlapped{}
	rEvent, _ := windows.CreateEvent(nil, 0, 0, nil)
	rOverlapped.HEvent = windows.Handle(rEvent)

	wOverlapped := windows.Overlapped{}
	wEvent, _ := windows.CreateEvent(nil, 0, 0, nil)
	wOverlapped.HEvent = windows.Handle(wEvent)

	dev := &winTapDev{
		fd:          fd,
		rOverlapped: rOverlapped,
		wOverlapped: wOverlapped,
		wInitiated:  false,

		addr:   addr,
		addrIP: parseIPv4(addr),
		gw:     gw,
		gwIP:   parseIPv4(gw),
	}
	return dev
}

func (dev *winTapDev) Read(data []byte) (int, error) {
	for {
		var done uint32
		var nr int

		err := windows.ReadFile(dev.fd, dev.rBuf[:], &done, &dev.rOverlapped)
		if err != nil {
			if err != windows.ERROR_IO_PENDING {
				return 0, err
			} else {
				windows.WaitForSingleObject(dev.rOverlapped.HEvent, windows.INFINITE)
				nr, err = getOverlappedResult(dev.fd, &dev.rOverlapped)
				if err != nil {
					return 0, err
				}
			}
		} else {
			nr = int(done)
		}
		if nr > 14 {
			if isStopMarker(dev.rBuf[14:nr], dev.addrIP, dev.gwIP) {
				return 0, errStopMarkerReceived
			}

			// discard IPv6 packets
			if dev.rBuf[14]&0xf0 == 0x60 {
				log.Debug("ipv6 packet")
				continue
			} else if dev.rBuf[14]&0xf0 == 0x40 {
				if !dev.wInitiated {
					// copy ether header for writing
					copy(dev.wBuf[:], dev.rBuf[6:12])
					copy(dev.wBuf[6:], dev.rBuf[0:6])
					copy(dev.wBuf[12:], dev.rBuf[12:14])
					dev.wInitiated = true
				}
				copy(data, dev.rBuf[14:nr])
				return nr - 14, nil
			}
		}
	}
}

func (dev *winTapDev) Write(data []byte) (int, error) {
	var done uint32
	var nw int

	payloadL := copy(dev.wBuf[14:], data)
	packetL := payloadL + 14
	err := windows.WriteFile(dev.fd, dev.wBuf[:packetL], &done, &dev.wOverlapped)
	if err != nil {
		if err != windows.ERROR_IO_PENDING {
			return 0, err
		} else {
			windows.WaitForSingleObject(dev.wOverlapped.HEvent, windows.INFINITE)
			nw, err = getOverlappedResult(dev.fd, &dev.wOverlapped)
			if err != nil {
				return 0, err
			}
		}
	} else {
		nw = int(done)
	}
	if nw != packetL {
		return 0, fmt.Errorf("write %d packet (%d bytes payload), return %d", packetL, payloadL, nw)
	} else {
		return payloadL, nil
	}
}

func getOverlappedResult(h windows.Handle, overlapped *windows.Overlapped) (int, error) {
	var n int
	r, _, err := syscall.Syscall6(procGetOverlappedResult.Addr(), 4,
		uintptr(h),
		uintptr(unsafe.Pointer(overlapped)),
		uintptr(unsafe.Pointer(&n)), 1, 0, 0)
	if r == 0 {
		return n, err
	}
	return n, nil
}

func (dev *winTapDev) Stop() error {
	if atomic.CompareAndSwapInt64(&dev.stopped, 0, 1) {
		sendStopMarker(dev.addr, dev.gw)
		return nil
	}
	return errAlreadyStopped
}

func (dev *winTapDev) Close() error {
	return windows.Close(dev.fd)
}
