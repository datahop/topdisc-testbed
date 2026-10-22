// node is a full devp2p node whose only job is to register a topic and
// keep its peer slots filled from topic search: geth's p2p server, real RLPx
// connections, and the dialer fed by the TopicSearch iterator.
package main

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/discover/topicindex"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func main() {
	port := flag.Int("port", 30303, "TCP+UDP listen port")
	statusPort := flag.Int("status", 0, "status HTTP port (0 = port+10000)")
	boot := flag.String("bootnodes", "", "comma-separated enode URLs")
	topicName := flag.String("topic", "topdisc-test", "topic to register and search")
	maxPeers := flag.Int("maxpeers", 50, "peer slots")
	dialRatio := flag.Int("dialratio", 3, "1/N of slots are dialed")
	ip := flag.String("ip", "127.0.0.1", "address to listen on and advertise")
	verbosity := flag.Int("v", 2, "log verbosity")
	assignment := flag.String("assignment", "", "assignment JSON from the coordinator; overrides the other flags")
	flag.Parse()
	if *statusPort == 0 {
		*statusPort = *port + 10000
	}
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.FromLegacyLevel(*verbosity), false)))

	var (
		key  *ecdsa.PrivateKey
		err  error
		asg  assign.Assignment
		wait = func(int64) {}
	)
	if *assignment != "" {
		if asg, err = assign.Read(*assignment); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		key, err = crypto.HexToECDSA(asg.Key)
		*ip, *port, *statusPort, *maxPeers, *dialRatio = asg.IP, asg.Port, asg.StatusPort, asg.MaxPeers, asg.DialRatio
		*topicName, *boot = asg.Topics[0], strings.Join(asg.Bootnodes, ",")
		wait = func(atMs int64) {
			if d := time.Until(time.UnixMilli(atMs)); d > 0 {
				time.Sleep(d)
			}
		}
	} else {
		key, err = crypto.GenerateKey()
	}
	if err != nil {
		panic(err)
	}
	n := newNode(asg, key)
	n.ip, n.port, n.maxPeers, n.dialRatio, n.topicName = *ip, *port, *maxPeers, *dialRatio, *topicName
	n.topic = topicindex.TopicID(crypto.Keccak256Hash([]byte(*topicName)))
	for _, u := range strings.Split(*boot, ",") {
		if u = strings.TrimSpace(u); u != "" {
			bn, err := enode.Parse(enode.ValidSchemes, u)
			if err != nil {
				fmt.Fprintln(os.Stderr, "bad bootnode:", err)
				os.Exit(2)
			}
			n.bootnodes = append(n.bootnodes, bn)
		}
	}
	discover.EnableWireStats()
	discover.EnableWaitStats()

	// The status port carries the churn control, so it is up before the node
	// is: a departure can come before the node's own search start.
	control := func(w http.ResponseWriter, err error) {
		switch {
		case err == nil:
			fmt.Fprintln(w, "ok")
		case errors.Is(err, errUp), errors.Is(err, errDown), errors.Is(err, errStopped):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(n.status()) })
	http.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) { control(w, n.down()) })
	http.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) { control(w, n.up()) })
	go http.ListenAndServe(fmt.Sprintf("%s:%d", *ip, *statusPort), nil)

	// An arrival is not there before its first return: it waits for /up.
	arrival := len(asg.Churn) > 0 && asg.Churn[0].DownAt == 0
	wait(asg.Phases.StartAt) // spread startups
	if !arrival {
		if err := n.up(); err != nil {
			fmt.Fprintln(os.Stderr, "start:", err)
			os.Exit(1)
		}
	}
	if asg.Phases.StopAt > 0 {
		go func() { wait(asg.Phases.StopAt); n.finish(); os.Exit(0) }()
	}
	log.Info("node ready", "status", *statusPort, "arrival", arrival)
	go func() {
		t := time.NewTicker(30 * time.Second)
		for range t.C {
			s := n.status()
			log.Info("status", "up", s["up"], "peers", s["peers"], "out", s["outbound"], "in", s["inbound"], "table", s["table"], "ads", s["ads_held"])
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	n.finish()
}

// svcEntry is the "svc" ENR entry: the service a node provides, as a 32-bit
// hash of the topic name. Legacy nodes filter random-walk discovery on it.
type svcEntry uint32

func (svcEntry) ENRKey() string { return "svc" }

func svcOf(topic string) svcEntry {
	h := fnv.New32a()
	h.Write([]byte(topic))
	return svcEntry(h.Sum32())
}
