package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// Host probes bind only UDP ports in this range, on the enclave bridge IP.
const (
	probePortMin = 31400
	probePortMax = 31499
)

// oldDevp2pVersion matches OLD_DEVP2P_VERSION in lib.sh.
const oldDevp2pVersion = "v1.17.10"

// probeFlags are the flags every discv5 subcommand shares.
type probeFlags struct {
	*runFlags
	enclaveName string
	bridge      string
	devnet      string
}

func (f *probeFlags) register(fs *flag.FlagSet) {
	f.runFlags = addRunFlags(fs)
	fs.StringVar(&f.enclaveName, "enclave", "", "Kurtosis enclave (default: run directory name without the date)")
	fs.StringVar(&f.bridge, "bridge", "", "enclave bridge IP; must be the gateway of docker network kt-<enclave> (default)")
	fs.StringVar(&f.devnet, "devnet", "", "devnet directory with bin/ (default: found from the binary or the working directory)")
}

// setup loads the inventory and resolves the output directory, bridge IP and devnet directory.
func (f *probeFlags) setup() (*inventory, net.IP, error) {
	if err := f.check(); err != nil {
		return nil, nil, err
	}
	inv, err := f.inventory()
	if err != nil {
		return nil, nil, err
	}
	if f.devnet == "" {
		if f.devnet, err = findDevnet(); err != nil {
			return nil, nil, err
		}
	}
	if f.enclaveName == "" {
		f.enclaveName = f.enclave()
	}
	gw, err := bridgeIP(f.enclaveName)
	if err != nil {
		return nil, nil, err
	}
	if f.bridge == "" {
		f.bridge = gw
	}
	ip := net.ParseIP(f.bridge).To4()
	if ip == nil || ip.IsUnspecified() || f.bridge != gw {
		return nil, nil, fmt.Errorf("-bridge %q is not the gateway %s of docker network kt-%s", f.bridge, gw, f.enclaveName)
	}
	return inv, ip, nil
}

func bridgeIP(enclave string) (string, error) {
	out, err := docker("network", "inspect", "kt-"+enclave, "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		return "", err
	}
	for _, s := range strings.Fields(string(out)) {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
			return s, nil
		}
	}
	return "", fmt.Errorf("network kt-%s has no IPv4 gateway", enclave)
}

// first returns the first node of the given kind, participant 0 when it has that kind.
func (inv *inventory) first(kind string) (node, bool) {
	for _, n := range inv.Nodes {
		if n.Kind == kind {
			return n, true
		}
	}
	return node{}, false
}

func (inv *inventory) nodesByID() map[string]node {
	m := make(map[string]node, len(inv.Nodes))
	for _, n := range inv.Nodes {
		m[n.ID] = n
	}
	return m
}

func enrNode(n node) (*enode.Node, error) {
	return enode.Parse(enode.ValidSchemes, n.ENR)
}

func findDevnet() (string, error) {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(filepath.Dir(exe)))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd, filepath.Join(wd, "devnet"))
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "lib.sh")); err == nil {
			return d, nil
		}
	}
	return "", errors.New("devnet directory not found, use -devnet")
}

func checkPort(port int) error {
	if port < probePortMin || port > probePortMax {
		return fmt.Errorf("port %d outside %d-%d", port, probePortMin, probePortMax)
	}
	return nil
}

// fixedKey derives the probe key of one role, so a probe has the same node ID in every run.
func fixedKey(role string) *ecdsa.PrivateKey {
	h := sha256.Sum256([]byte("d79check/" + role))
	key, err := crypto.ToECDSA(h[:])
	if err != nil {
		panic(err)
	}
	return key
}

// freshFile fails if path exists: the tools append to or read back their output files.
func freshFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists, use a fresh -out", path)
	}
	return nil
}

// startProbe starts a fork discv5 node on ip:port with a static ENR IP, no TCP port
// and no td key, so TopDisc nodes do not register their ads with it.
func startProbe(ip net.IP, port int, key *ecdsa.PrivateKey, bootnodes []*enode.Node, logger log.Logger) (*discover.UDPv5, error) {
	return listenV5(ip, port, key, bootnodes, logger, true)
}

func listenV5(ip net.IP, port int, key *ecdsa.PrivateKey, bootnodes []*enode.Node, logger log.Logger, noTD bool) (*discover.UDPv5, error) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return nil, err
	}
	conn := &gatedConn{UDPConn: udp, open: make(chan struct{})}
	db, err := enode.OpenDB("")
	if err != nil {
		udp.Close()
		return nil, err
	}
	ln := enode.NewLocalNode(db, key)
	ln.SetStaticIP(ip)
	ln.SetFallbackUDP(udp.LocalAddr().(*net.UDPAddr).Port)
	disc, err := discover.ListenV5(conn, ln, discover.Config{PrivateKey: key, Bootnodes: bootnodes, Log: logger})
	if err != nil {
		udp.Close()
		db.Close()
		return nil, err
	}
	// ListenV5 sets td; nothing is sent until it is gone.
	if noTD {
		ln.Delete(topicindex.TopicDiscoveryVersion)
	}
	conn.release()
	return disc, nil
}

// gatedConn holds every write until release, so no packet carries the record ListenV5
// starts with.
type gatedConn struct {
	*net.UDPConn
	open chan struct{}
	once sync.Once
}

func (c *gatedConn) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	<-c.open
	return c.UDPConn.WriteToUDPAddrPort(b, addr)
}

func (c *gatedConn) release() { c.once.Do(func() { close(c.open) }) }

func (c *gatedConn) Close() error {
	c.release()
	return c.UDPConn.Close()
}

func fileLogger(path string) (log.Logger, *os.File, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return log.NewLogger(log.NewTerminalHandlerWithLevel(f, log.LevelDebug, false)), f, nil
}

// probeRecord is one host probe in <run>/probes.json, so log checks can tell its
// node ID apart from the devnet's.
type probeRecord struct {
	Name    string     `json:"name"`
	ID      string     `json:"id"`
	Started time.Time  `json:"started"`
	Stopped *time.Time `json:"stopped"`
}

// recordProbe adds a probe to <run>/probes.json, or marks an earlier entry of the same
// ID as running again, and returns a function that sets its stop time. Entries are
// never removed and keep their first start; the file is locked for each update, so
// probes may run in parallel.
func recordProbe(run, name string, key *ecdsa.PrivateKey) (stopped func(), err error) {
	id := enode.PubkeyToIDV4(&key.PublicKey).String()
	err = updateProbes(run, func(ps []probeRecord) []probeRecord {
		return mergeProbe(ps, probeRecord{Name: name, ID: id, Started: time.Now().UTC()})
	})
	if err != nil {
		return nil, err
	}
	return func() {
		t := time.Now().UTC()
		err := updateProbes(run, func(ps []probeRecord) []probeRecord {
			return mergeProbe(ps, probeRecord{Name: name, ID: id, Stopped: &t})
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "d79check: probes.json:", err)
		}
	}, nil
}

// mergeProbe folds p into the entry with its ID: the earliest start and the latest
// stop are kept, and a start without a stop (the probe runs again) clears stopped.
func mergeProbe(ps []probeRecord, p probeRecord) []probeRecord {
	for i := range ps {
		e := &ps[i]
		if e.ID != p.ID {
			continue
		}
		if !p.Started.IsZero() && (e.Started.IsZero() || p.Started.Before(e.Started)) {
			e.Started = p.Started
		}
		switch {
		case p.Stopped == nil && !p.Started.IsZero():
			e.Stopped = nil
		case p.Stopped != nil && (e.Stopped == nil || p.Stopped.After(*e.Stopped)):
			e.Stopped = p.Stopped
		}
		return ps
	}
	return append(ps, p)
}

// updateProbes rewrites <run>/probes.json under probes.json.lock, through a temporary
// file and a rename. A file that exists but is empty or unparsable is an error.
func updateProbes(run string, f func([]probeRecord) []probeRecord) error {
	path := filepath.Join(run, "probes.json")
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	var ps []probeRecord
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(data, &ps); err != nil || ps == nil {
			return fmt.Errorf("%s: not a JSON list of probes (%v)", path, err)
		}
	}
	// Fold duplicate IDs of older files into one entry each.
	var merged []probeRecord
	for _, p := range ps {
		merged = mergeProbe(merged, p)
	}
	merged = f(merged)
	if len(merged) < len(unique(ps)) {
		return fmt.Errorf("%s: update would drop entries", path)
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func unique(ps []probeRecord) map[string]bool {
	m := make(map[string]bool, len(ps))
	for _, p := range ps {
		m[p.ID] = true
	}
	return m
}
