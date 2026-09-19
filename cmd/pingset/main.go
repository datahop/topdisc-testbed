// pingset pings a list of ENRs a few times from its own discv5 endpoint and
// prints how many answers each gave. It exists to compare a fresh endpoint
// (new NAT flows) against a running crawler.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func main() {
	listen := flag.String("listen", ":9000", "UDP listen address")
	extIP := flag.String("extip", "", "public IP to advertise")
	file := flag.String("enrs", "", "JSON array of enr strings")
	tries := flag.Int("tries", 3, "pings per node")
	gap := flag.Duration("gap", 2*time.Second, "gap between tries")
	flag.Parse()

	var enrs []string
	b, err := os.ReadFile(*file)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(b, &enrs); err != nil {
		panic(err)
	}
	key, _ := crypto.GenerateKey()
	addr, _ := net.ResolveUDPAddr("udp", *listen)
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		panic(err)
	}
	db, _ := enode.OpenDB("")
	ln := enode.NewLocalNode(db, key)
	if *extIP != "" {
		ln.SetStaticIP(net.ParseIP(*extIP))
	}
	ln.SetFallbackUDP(addr.Port)
	disc, err := discover.ListenV5(conn, ln, discover.Config{PrivateKey: key})
	if err != nil {
		panic(err)
	}
	defer disc.Close()

	total, ok := 0, 0
	hist := map[int]int{}
	for _, s := range enrs {
		n, err := enode.Parse(enode.ValidSchemes, s)
		if err != nil {
			continue
		}
		got := 0
		for i := 0; i < *tries; i++ {
			if _, err := disc.Ping(n); err == nil {
				got++
			}
			time.Sleep(*gap)
		}
		hist[got]++
		total += *tries
		ok += got
	}
	fmt.Printf("nodes %d tries %d answered %d (%.0f%%) histogram %v\n", len(enrs), total, ok, 100*float64(ok)/float64(total), hist)
}
