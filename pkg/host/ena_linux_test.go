//go:build linux

package host

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// primary opens this machine's default-route interface, whatever its driver.
func primary(t *testing.T) *ethtool {
	t.Helper()
	b, err := os.ReadFile("/proc/net/route")
	iface := defaultRouteIface(b)
	if err != nil || iface == "" {
		t.Skip("no default route")
	}
	e := &ethtool{iface: iface}
	if err := e.load(); err != nil {
		t.Skipf("%s: %v", iface, err)
	}
	return e
}

// The ioctl reader agrees with `ethtool -S`.
func TestEthtoolMatchesCLI(t *testing.T) {
	e := primary(t)
	defer e.Close()
	before, err := e.values()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ethtool", "-S", e.iface).Output()
	if err != nil {
		t.Skipf("ethtool -S %s: %v", e.iface, err)
	}
	after, err := e.values()
	if err != nil {
		t.Fatal(err)
	}
	names, cli := parseEthtoolS(t, string(out))
	if !slices.Equal(names, e.names) {
		t.Fatalf("names differ:\nioctl %v\ncli   %v", e.names, names)
	}
	for i, n := range names {
		lo, hi := min(before[i], after[i]), max(before[i], after[i])
		if cli[i] < lo || cli[i] > hi {
			t.Errorf("%s: cli %d outside ioctl %d..%d", n, cli[i], lo, hi)
		}
	}
	t.Logf("%s (%s): %d stats match", e.iface, e.driver, len(names))
	if _, err := openPrimaryENA(); e.driver != "ena" && !hasENADevice() && err != errNotENA {
		t.Errorf("driver %s, no ENA device: err %v, want errNotENA", e.driver, err)
	}
}

// A request's count field always equals what its buffer holds, and the
// buffer ends at the guard page.
func TestEthtoolReqCapacity(t *testing.T) {
	pg := os.Getpagesize()
	for _, kind := range []struct {
		cmd        uint32
		hdr, entry int
	}{{ethtoolGStats, gstatsHdr, 8}, {ethtoolGStrings, gstringsHdr, 32}} {
		for _, n := range []int{0, 1, 67, pg / kind.entry, 5000} {
			r, err := newEthtoolReq(kind.cmd, kind.hdr, kind.entry, n)
			if err != nil {
				t.Fatal(err)
			}
			if r.count() != n || (len(r.buf)-kind.hdr)/kind.entry != n || len(r.buf) != kind.hdr+kind.entry*n || cap(r.buf) != len(r.buf) {
				t.Errorf("cmd %#x n %d: count %d, buffer %d (cap %d)", kind.cmd, n, r.count(), len(r.buf), cap(r.buf))
			}
			end := uintptr(unsafe.Pointer(&r.buf[0])) + uintptr(len(r.buf))
			if guard := uintptr(unsafe.Pointer(&r.mem[len(r.mem)-pg])); end != guard {
				t.Errorf("cmd %#x n %d: buffer ends at %#x, guard page at %#x", kind.cmd, n, end, guard)
			}
			r.reset()
			if r.count() != n {
				t.Errorf("reset: count %d, want %d", r.count(), n)
			}
			r.free()
		}
	}
}

// A request sized for fewer statistics than the driver has never writes past
// its buffer. A count of 0 asks for all of them, and older kernels copy out
// their own count whatever the request says: both fault at the guard page
// (EFAULT). Newer kernels refuse a count that differs, returning 0 entries.
func TestEthtoolOverrunFaults(t *testing.T) {
	e := primary(t)
	defer e.Close()
	n := len(e.names)
	if n < 2 {
		t.Skip("too few stats")
	}
	for _, short := range []int{0, 1, n - 1} {
		r, err := newEthtoolReq(ethtoolGStats, gstatsHdr, 8, short)
		if err != nil {
			t.Fatal(err)
		}
		err = ioctlEthtool(e.iface, r.buf)
		switch {
		case errors.Is(err, syscall.EFAULT):
		case err == nil && short > 0 && r.count() != short:
			// refused by the kernel; the reader reports the count mismatch
		default:
			t.Errorf("request for %d of %d stats: err %v, count %d", short, n, err, r.count())
		}
		t.Logf("request for %d of %d: err %v, count %d", short, n, err, r.count())
		r.free()
	}
}

// keptENA reads a real interface and reports success, so the monitor keeps it.
type keptENA struct{ *ethtool }

func (k keptENA) ena() (ENASample, error) {
	_, err := k.values()
	return ENASample{ENAIface: k.iface}, err
}

// mapped reports whether a stats mapping of the given start and size is in
// /proc/self/maps.
func mapped(t *testing.T, start uintptr, size int) bool {
	b, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), fmt.Sprintf("%x-%x rw-p", start, start+uintptr(size)))
}

func fds(t *testing.T) int {
	es, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

// Every reader the monitor drops is closed, and so is the one it holds at
// shutdown: no stats mapping or descriptor outlives it. This machine's NIC has
// no allowance counters, so each sample fails and reopens, as an ENA host
// whose counters are unavailable would.
func TestENAReaderNoLeak(t *testing.T) {
	primary(t).Close()
	type region struct {
		start uintptr
		size  int
	}
	var seen []region
	open := func() (*ethtool, error) {
		b, _ := os.ReadFile("/proc/net/route")
		e := &ethtool{iface: defaultRouteIface(b)}
		if err := e.load(); err != nil {
			e.Close()
			return nil, err
		}
		r := region{uintptr(unsafe.Pointer(&e.stats.mem[0])), len(e.stats.mem) - os.Getpagesize()}
		if !mapped(t, r.start, r.size) { // the check below would see a leak
			t.Fatalf("live mapping %#x not found in /proc/self/maps", r.start)
		}
		seen = append(seen, r)
		return e, nil
	}
	defer func(f func() (enaReader, error)) { openENA = f }(openENA)
	openENA = func() (enaReader, error) { return open() }

	reqs, fd0 := liveReqs.Load(), fds(t)
	// an open that fails once its mapping exists releases it
	iface := primary(t)
	iface.Close()
	if _, err := openDriver(iface.iface, "ena"); err == nil || iface.driver == "ena" {
		t.Fatalf("%s (%s) opened as ena: %v", iface.iface, iface.driver, err)
	}
	if n := liveReqs.Load(); n != reqs {
		t.Fatalf("after a refused open: %d live mappings, want %d", n, reqs)
	}
	m := &monitor{}
	for range 100 {
		var s Sample
		m.sampleENA(&s)
		if !strings.Contains(s.ENAError, errNoAllowance.Error()) {
			t.Fatalf("sample: %+v", s)
		}
		if n := liveReqs.Load(); n != reqs {
			t.Fatalf("after a failed sample: %d live mappings, want %d", n, reqs)
		}
	}
	if len(seen) != 100 {
		t.Fatalf("opened %d readers, want 100", len(seen))
	}
	for _, r := range seen {
		if mapped(t, r.start, r.size) {
			t.Fatalf("mapping %#x still in /proc/self/maps", r.start)
		}
	}
	if n := fds(t); n != fd0 {
		t.Fatalf("%d descriptors after 100 failed samples, %d before", n, fd0)
	}

	// a reader kept by a running monitor is released by StopMonitor
	openENA = func() (enaReader, error) {
		e, err := open()
		if err != nil {
			return nil, err
		}
		return keptENA{e}, nil
	}
	seen = nil
	r := &Runner{}
	r.Monitor(time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for len(r.Samples()) < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := liveReqs.Load(); n != reqs+1 {
		t.Fatalf("running monitor: %d live mappings, want %d", n, reqs+1)
	}
	r.StopMonitor()
	if n := liveReqs.Load(); n != reqs || len(seen) != 1 || mapped(t, seen[0].start, seen[0].size) {
		t.Fatalf("after StopMonitor: %d live mappings (want %d), %d readers opened (want 1)", n, reqs, len(seen))
	}
	if n := fds(t); n != fd0 {
		t.Fatalf("%d descriptors after shutdown, %d before", n, fd0)
	}
}
