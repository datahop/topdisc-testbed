// hostagent runs node processes on one cloud host under the coordinator's
// control. Runs as root on every host; the coordinator is the only client.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/datahop/topdisc-testbed/pkg/assign"
	"github.com/datahop/topdisc-testbed/pkg/host"
	"github.com/datahop/topdisc-testbed/pkg/wan"
)

// PrepareRequest is what the coordinator posts before starting nodes.
type PrepareRequest struct {
	Host           int                 `json:"host"`
	Peers          map[int]string      `json:"peers"`
	Wan            *wan.Star           `json:"wan"`
	Seed           int64               `json:"seed"`
	Verbosity      int                 `json:"verbosity"`
	SamplePeriodMs int64               `json:"sample_period_ms"`
	Assignments    []assign.Assignment `json:"assignments"`
}

func main() {
	addr := flag.String("serve", ":9000", "listen address")
	bin := flag.String("node-binary", "/opt/topdisc/topdisc-node", "node binary")
	legacyBin := flag.String("legacy-binary", "/opt/topdisc/topdisc-node-legacy", "stock-geth node binary for legacy assignments")
	work := flag.String("workdir", "/opt/topdisc/run", "assignments, logs and traces")
	flag.Parse()
	var mu sync.Mutex
	var r *host.Runner
	fail := func(w http.ResponseWriter, err error) { http.Error(w, err.Error(), 500) }
	idxOf := func(req *http.Request) int { i, _ := strconv.Atoi(req.URL.Query().Get("idx")); return i }

	http.HandleFunc("/prepare", func(w http.ResponseWriter, req *http.Request) {
		var p PrepareRequest
		if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
			fail(w, err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r != nil {
			r.StopMonitor()
			r.StopAll()
		}
		os.RemoveAll(*work)
		r = &host.Runner{NodeBinary: *bin, LegacyBinary: *legacyBin, Verbosity: p.Verbosity, AsgDir: filepath.Join(*work, "assignments"),
			LogDir: filepath.Join(*work, "logs"), Wan: p.Wan, Host: p.Host, Seed: p.Seed}
		os.MkdirAll(filepath.Join(*work, "traces"), 0o755)
		for i := range p.Assignments {
			p.Assignments[i].TraceFile = filepath.Join(*work, "traces", fmt.Sprintf("node%d.json", p.Assignments[i].Idx))
		}
		if err := r.Prepare(p.Assignments, p.Peers); err != nil {
			fail(w, err)
			return
		}
		if p.SamplePeriodMs > 0 {
			r.Monitor(time.Duration(p.SamplePeriodMs) * time.Millisecond)
		}
		fmt.Fprintf(w, "prepared %d nodes\n", len(p.Assignments))
	})
	http.HandleFunc("/start", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r == nil {
			fail(w, fmt.Errorf("not prepared"))
			return
		}
		if err := r.Start(idxOf(req)); err != nil {
			fail(w, err)
		}
	})
	http.HandleFunc("/kill", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r != nil {
			fmt.Fprint(w, r.Kill(idxOf(req)))
		}
	})
	http.HandleFunc("/stop", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r != nil {
			r.StopMonitor()
			r.StopAll()
		}
	})
	http.HandleFunc("/hostmetrics", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r == nil {
			fail(w, fmt.Errorf("not prepared"))
			return
		}
		json.NewEncoder(w).Encode(r.Samples())
	})
	http.HandleFunc("/trace", func(w http.ResponseWriter, req *http.Request) {
		http.ServeFile(w, req, filepath.Join(*work, "traces", fmt.Sprintf("node%d.json", idxOf(req))))
	})
	http.HandleFunc("/log", func(w http.ResponseWriter, req *http.Request) {
		http.ServeFile(w, req, filepath.Join(*work, "logs", fmt.Sprintf("node%d.log", idxOf(req))))
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) { fmt.Fprintln(w, "ok") })
	fmt.Fprintln(os.Stderr, "hostagent listening on", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
