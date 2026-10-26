//go:build linux

package host

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	siocEthtool     = 0x8946
	ethtoolGDrvinfo = 0x3
	ethtoolGStrings = 0x1b
	ethtoolGStats   = 0x1d
	ethSSStats      = 1
	drvinfoLen      = 196 // struct ethtool_drvinfo
	drvinfoNStats   = 180
	gstringsHdr     = 12 // struct ethtool_gstrings: cmd, string_set, len
	gstatsHdr       = 8  // struct ethtool_stats: cmd, n_stats
)

// ethtool reads an interface's driver statistics through SIOCETHTOOL, the
// ioctl behind `ethtool -S`, so a sample costs no fork.
type ethtool struct {
	iface, driver string
	names         []string
	stats         *ethtoolReq // GSTATS, sized for len(names)
}

// openPrimaryENA opens the interface of the default route. A host with no
// ENA device at all is errNotENA; anything else may pass and is retried.
func openPrimaryENA() (*ethtool, error) {
	if !hasENADevice() {
		return nil, errNotENA
	}
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil, err
	}
	iface := defaultRouteIface(b)
	if iface == "" {
		return nil, errors.New("no default route")
	}
	return openDriver(iface, "ena")
}

// openDriver opens iface if its driver is want, releasing what it took if not.
func openDriver(iface, want string) (*ethtool, error) {
	e := &ethtool{iface: iface}
	if err := e.load(); err != nil {
		e.Close()
		return nil, err
	}
	if e.driver != want {
		e.Close()
		return nil, fmt.Errorf("default route via %s (%s), not an %s device", iface, e.driver, want)
	}
	return e, nil
}

// Close releases the stats mapping; the ioctl socket lives for one call.
func (e *ethtool) Close() {
	if e.stats != nil {
		e.stats.free()
		e.stats = nil
	}
}

func hasENADevice() bool {
	links, _ := filepath.Glob("/sys/class/net/*/device/driver")
	for _, l := range links {
		if d, err := os.Readlink(l); err == nil && filepath.Base(d) == "ena" {
			return true
		}
	}
	return false
}

// count asks the driver how many statistics it has now.
func (e *ethtool) count() (driver string, n int, err error) {
	ne := binary.NativeEndian
	info := make([]byte, drvinfoLen) // fixed size: the kernel copies exactly this much
	ne.PutUint32(info, ethtoolGDrvinfo)
	if err := ioctlEthtool(e.iface, info); err != nil {
		return "", 0, err
	}
	return string(bytes.TrimRight(info[4:36], "\x00")), int(ne.Uint32(info[drvinfoNStats:])), nil
}

// load reads the driver's name and its statistics' names.
func (e *ethtool) load() error {
	driver, n, err := e.count()
	if err != nil {
		return err
	}
	req, err := newEthtoolReq(ethtoolGStrings, gstringsHdr, 32, n)
	if err != nil {
		return err
	}
	defer req.free()
	binary.NativeEndian.PutUint32(req.buf[4:], ethSSStats)
	if err := ioctlEthtool(e.iface, req.buf); err != nil {
		return err
	}
	if got := req.count(); got != n {
		return fmt.Errorf("%s: %d stat names, expected %d", e.iface, got, n)
	}
	e.driver, e.names = driver, parseGStrings(req.buf[gstringsHdr:], n)
	if e.stats == nil || e.stats.n != n {
		if e.stats != nil {
			e.stats.free()
		}
		if e.stats, err = newEthtoolReq(ethtoolGStats, gstatsHdr, 8, n); err != nil {
			return err
		}
	}
	return nil
}

// values returns the statistics in the order of e.names, reloading the names
// first if the driver's count changed since.
func (e *ethtool) values() ([]uint64, error) {
	_, n, err := e.count()
	if err != nil {
		return nil, err
	}
	if n != len(e.names) {
		if err := e.load(); err != nil {
			return nil, err
		}
	}
	req := e.stats
	req.reset()
	if err := ioctlEthtool(e.iface, req.buf); err != nil {
		return nil, err
	}
	if got := req.count(); got != req.n {
		return nil, fmt.Errorf("%s: %d stats, expected %d", e.iface, got, req.n)
	}
	vals := make([]uint64, req.n)
	for i := range vals {
		vals[i] = binary.NativeEndian.Uint64(req.buf[gstatsHdr+8*i:])
	}
	return vals, nil
}

func (e *ethtool) ena() (ENASample, error) {
	vals, err := e.values()
	if err != nil {
		return ENASample{}, err
	}
	return enaSample(e.iface, e.names, vals)
}

// ethtoolReq is a GSTRINGS or GSTATS request for n entries in its own
// mapping, ending right before a PROT_NONE page. The kernel copies out as
// many entries as the driver has at that moment, whatever the request says,
// so a table that grew after count() makes the ioctl fail with EFAULT at the
// guard page instead of writing past the buffer.
type ethtoolReq struct {
	cmd      uint32
	hdr, n   int
	mem, buf []byte
}

func newEthtoolReq(cmd uint32, hdr, entry, n int) (*ethtoolReq, error) {
	size, pg := hdr+entry*n, os.Getpagesize()
	data := (size + pg - 1) / pg * pg
	mem, err := syscall.Mmap(-1, 0, data+pg, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, err
	}
	if err := syscall.Mprotect(mem[data:], syscall.PROT_NONE); err != nil {
		syscall.Munmap(mem)
		return nil, err
	}
	r := &ethtoolReq{cmd: cmd, hdr: hdr, n: n, mem: mem, buf: mem[data-size : data : data]}
	r.reset()
	liveReqs.Add(1)
	return r, nil
}

// liveReqs counts the mappings not yet freed, for the tests.
var liveReqs atomic.Int64

// reset writes the header: the command and, where the kernel reads it, n.
func (r *ethtoolReq) reset() {
	ne := binary.NativeEndian
	ne.PutUint32(r.buf, r.cmd)
	ne.PutUint32(r.buf[r.hdr-4:], uint32(r.n))
}

// count is the entry count the kernel wrote back.
func (r *ethtoolReq) count() int { return int(binary.NativeEndian.Uint32(r.buf[r.hdr-4:])) }

func (r *ethtoolReq) free() {
	if r.mem != nil {
		syscall.Munmap(r.mem)
		r.mem, r.buf = nil, nil
		liveReqs.Add(-1)
	}
}

// ioctlEthtool runs one SIOCETHTOOL command on iface; the kernel finds data
// through ifr_data, so a Go buffer is pinned for the call.
func ioctlEthtool(iface string, data []byte) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var ifr [40]byte // struct ifreq: name, then ifr_data
	copy(ifr[:15], iface)
	var pin runtime.Pinner // a no-op on a request's mapping
	pin.Pin(&data[0])
	defer pin.Unpin()
	*(*uintptr)(unsafe.Pointer(&ifr[16])) = uintptr(unsafe.Pointer(&data[0]))
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), siocEthtool, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return fmt.Errorf("%s: ethtool %#x: %w", iface, binary.NativeEndian.Uint32(data), errno)
	}
	return nil
}
