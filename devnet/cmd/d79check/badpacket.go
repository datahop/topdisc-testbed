package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/p2p/discover/v5wire"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func init() {
	register("badpacket-control", "E2 positive control: one REGTOPIC to an OLD node yields one unknown packet type line", runBadpacket)
}

type badpacketResult struct {
	Check     check     `json:"check"`
	Target    node      `json:"target"`
	Container string    `json:"container"`
	Probe     string    `json:"probe"`
	Sent      time.Time `json:"sent"`
	BadLines  []string  `json:"bad_lines"` // Bad discv5 packet lines from the probe's address
	Unknown   int       `json:"unknown_type_7"`
	Pass      bool      `json:"pass"`
}

func runBadpacket(args []string) error {
	var rf probeFlags
	fs := flag.NewFlagSet("badpacket-control", flag.ExitOnError)
	rf.register(fs)
	port := fs.Int("port", 31430, "UDP port of the probe on the bridge IP")
	target := fs.String("target", "", "inventory name of the OLD node (default: the first OLD node)")
	wait := fs.Duration("wait", 10*time.Second, "time for the line to reach the container log")
	fs.Parse(args)

	// The control leaves a bad-packet line in the target's log; keep it out of the runs judged on E2.
	judged := func(e string) bool { return e == "d79-control" || strings.HasPrefix(e, "d79-mixed-") }
	for _, e := range []string{rf.enclaveName, rf.enclave()} {
		if judged(e) {
			return fmt.Errorf("refusing to run for %s: E2 counts bad-packet lines there", e)
		}
	}
	inv, ip, err := rf.setup()
	if err != nil {
		return err
	}
	if err := checkPort(*port); err != nil {
		return err
	}
	var tn node
	if *target == "" {
		var ok bool
		if tn, ok = inv.first("old"); !ok {
			return errors.New("no OLD node")
		}
	} else {
		for _, n := range inv.Nodes {
			if n.Name == *target {
				tn = n
			}
		}
		if tn.Name == "" {
			return fmt.Errorf("no node %q in the inventory", *target)
		}
		if tn.Kind != "old" {
			return fmt.Errorf("target %s is a %s node, want an OLD one", tn.Name, strings.ToUpper(tn.Kind))
		}
	}
	container, err := containerOf(tn.Name)
	if err != nil {
		return err
	}
	dest, err := enrNode(tn)
	if err != nil {
		return err
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: *port})
	if err != nil {
		return err
	}
	defer conn.Close()
	stopped, err := recordProbe(rf.run, "badpacket", fixedKey("badpacket"))
	if err != nil {
		return err
	}
	sent := time.Now().UTC()
	err = sendRegtopic(conn, ip, dest)
	stopped()
	if err != nil {
		return err
	}
	time.Sleep(*wait)

	out, err := exec.Command("docker", "logs", "--since", sent.Add(-time.Second).Format(time.RFC3339Nano), container).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker logs %s: %v", container, err)
	}
	res := &badpacketResult{Target: tn, Container: container, Probe: conn.LocalAddr().String(), Sent: sent, BadLines: []string{}}
	res.BadLines, res.Unknown = badLines(string(out), res.Probe)
	res.Pass = len(res.BadLines) == 1 && res.Unknown == 1
	res.Check = check{ID: "E2 control", Desc: "one REGTOPIC to an OLD node yields one unknown packet type 7 line", Status: passIf(res.Pass),
		Detail: fmt.Sprintf("%s: %d lines, %d unknown packet type 7", tn.Name, len(res.BadLines), res.Unknown)}
	fmt.Printf("REGTOPIC to %s (%s): %d Bad discv5 packet lines from %s, %d with unknown packet type 7, pass %v\n",
		tn.Name, container, len(res.BadLines), res.Probe, res.Unknown, res.Pass)
	for _, l := range res.BadLines {
		fmt.Println("  " + l)
	}
	return writeJSON(filepath.Join(rf.out, "badpacket-control.json"), res)
}

func badLines(log, from string) (lines []string, unknown int) {
	lines = []string{}
	for _, l := range strings.Split(log, "\n") {
		if !strings.Contains(l, "Bad discv5 packet") || !strings.Contains(l, "addr="+from) {
			continue
		}
		lines = append(lines, l)
		if strings.Contains(l, "unknown packet type 7") {
			unknown++
		}
	}
	return lines, unknown
}

// sendRegtopic opens a session with a random packet, answers the node's WHOAREYOU with a
// handshake that carries a REGTOPIC, and returns without waiting for an answer.
func sendRegtopic(conn *net.UDPConn, ip net.IP, dest *enode.Node) error {
	key := fixedKey("badpacket")
	db, err := enode.OpenDB("")
	if err != nil {
		return err
	}
	defer db.Close()
	ln := enode.NewLocalNode(db, key)
	ln.SetStaticIP(ip)
	ln.SetFallbackUDP(conn.LocalAddr().(*net.UDPAddr).Port)
	codec := v5wire.NewCodec(ln, key, mclock.System{}, nil)
	to, _ := dest.UDPEndpoint()
	addr := to.String()
	reqID := make([]byte, 8)
	rand.Read(reqID)

	pkt, _, err := codec.Encode(dest.ID(), addr, &v5wire.Ping{ReqID: reqID, ENRSeq: ln.Node().Seq()}, nil)
	if err != nil {
		return err
	}
	if _, err := conn.WriteToUDPAddrPort(pkt, to); err != nil {
		return err
	}
	buf := make([]byte, 1280)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, from, err := conn.ReadFromUDPAddrPort(buf)
	if err != nil {
		return fmt.Errorf("no WHOAREYOU from %s: %v", addr, err)
	}
	_, _, p, err := codec.Decode(buf[:n], from.String())
	if err != nil {
		return fmt.Errorf("decode reply: %v", err)
	}
	challenge, ok := p.(*v5wire.Whoareyou)
	if !ok {
		return fmt.Errorf("got %s, want WHOAREYOU", p.Name())
	}
	challenge.Node = dest
	rand.Read(reqID)
	reg := &v5wire.Regtopic{ReqID: reqID, Topic: [32]byte{'d', '7', '9'}, ENR: ln.Node().Record()}
	pkt, _, err = codec.Encode(dest.ID(), addr, reg, challenge)
	if err != nil {
		return err
	}
	_, err = conn.WriteToUDPAddrPort(pkt, to)
	return err
}

// containerOf finds the running container of an inventory node: Kurtosis names it <name>--<uuid>.
func containerOf(name string) (string, error) {
	out, err := exec.Command("docker", "ps", "--filter", "name=^"+name+"--", "--format", "{{.Names}}").Output()
	if err != nil {
		return "", fmt.Errorf("docker ps: %v", err)
	}
	names := strings.Fields(string(out))
	if len(names) != 1 {
		return "", fmt.Errorf("want one container for %s, found %v", name, names)
	}
	return names[0], nil
}
