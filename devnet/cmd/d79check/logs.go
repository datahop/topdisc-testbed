package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Saved logs: "<RFC3339 docker timestamp> LEVEL[mm-dd|hh:mm:ss.mmm] msg  key=value ...";
// a five-letter level touches the bracket.
var (
	logLineRe = regexp.MustCompile(`^(\S+) (TRACE|DEBUG|INFO|WARN|ERROR|CRIT) ?\[[^\]]*\] (.*)$`)
	logCtxRe  = regexp.MustCompile(`\s+[A-Za-z_][A-Za-z0-9_./-]*=`)
	logIDRe   = regexp.MustCompile(`\bid=([0-9a-f]{8,64})\b`)
	logNRe    = regexp.MustCompile(`\bn=(\d+)\b`)
	logErrRe  = regexp.MustCompile(`\berr="?([^"]*)"?$`)
)

type logsNode struct {
	Name              string         `json:"name"`
	Kind              string         `json:"kind"`
	Error             string         `json:"error,omitempty"`
	Lines             int            `json:"lines"`
	First             time.Time      `json:"first"`
	Last              time.Time      `json:"last"`
	Levels            map[string]int `json:"levels"`
	BadPacket         int            `json:"bad_packet"`
	BadPacketErrs     map[string]int `json:"bad_packet_errs,omitempty"`
	Removed           map[string]int `json:"removed_by_kind"`
	Templates         map[string]int `json:"templates"`
	Reorgs            int            `json:"reorgs"`
	ReorgsAfterWarmup int            `json:"reorgs_after_warmup"`
	TopicRegOK        int            `json:"topic_reg_ok"`
	TopicRegProbe     int            `json:"topic_reg_probe_ignored"`
	TopicRegFailed    int            `json:"topic_reg_failed"`
	TopicQuery        int            `json:"topic_query"`
	TopicQueryNonzero int            `json:"topic_query_nonzero"`
}

type logsResult struct {
	Warmup  time.Time  `json:"warmup_end"`
	Probes  []string   `json:"probes,omitempty"`
	Nodes   []logsNode `json:"nodes"`
	Missing []string   `json:"missing,omitempty"`
}

func init() {
	register("logs", "pattern counts per EL from the saved logs/*.log", runLogs)
}

func runLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	rf := addRunFlags(fs)
	warmup := fs.Duration("warmup", 10*time.Minute, "reorgs before genesis+warmup do not count (all count without genesis.txt)")
	fs.Parse(args)
	if err := rf.check(); err != nil {
		return err
	}
	inv, err := rf.inventory()
	if err != nil {
		return err
	}
	var res logsResult
	if g, err := rf.genesis(); err == nil {
		res.Warmup = g.Add(*warmup)
	}
	if res.Probes, err = readProbes(filepath.Join(rf.run, "probes.json")); err != nil {
		return err
	}
	ignored := 0
	for _, n := range inv.Nodes {
		ln := scanLog(filepath.Join(rf.run, "logs", n.Name+".log"), n, inv, res.Warmup, res.Probes)
		ignored += ln.TopicRegProbe
		if ln.Error != "" {
			res.Missing = append(res.Missing, n.Name)
		}
		fmt.Printf("%-24s %-3s lines=%-6d bad=%d removed old/new/unknown=%d/%d/%d reorgs=%d/%d topic ok/failed/query/nonzero=%d/%d/%d/%d probe-ignored=%d warn+error templates=%d %s\n",
			n.Name, n.Kind, ln.Lines, ln.BadPacket, ln.Removed[kindOld], ln.Removed[kindNew], ln.Removed[kindUnknown],
			ln.Reorgs, ln.ReorgsAfterWarmup, ln.TopicRegOK, ln.TopicRegFailed, ln.TopicQuery, ln.TopicQueryNonzero,
			ln.TopicRegProbe, len(ln.Templates), ln.Error)
		res.Nodes = append(res.Nodes, ln)
	}
	fmt.Printf("ignored %d Topic registration lines for %d host probes (probes.json)\n", ignored, len(res.Probes))
	return writeJSON(filepath.Join(rf.out, "logs.json"), res)
}

// readProbes returns the node IDs of the host probes listed by M3b in probes.json
// ([{name, id, started, stopped}]); no file means no probes.
func readProbes(path string) ([]string, error) {
	var probes []struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	if err := readJSON(path, &probes); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("probes.json: %v", err)
	} else if probes == nil {
		return nil, errors.New("probes.json: not a JSON list")
	}
	var ids []string
	for _, p := range probes {
		if !nodeIDRe.MatchString(strings.ToLower(p.ID)) {
			return nil, fmt.Errorf("probes.json: %s has bad id %q", p.Name, p.ID)
		}
		ids = append(ids, strings.ToLower(p.ID))
	}
	return ids, nil
}

// isProbe reports whether a log-abbreviated node ID belongs to a host probe.
func isProbe(id string, probes []string) bool {
	for _, p := range probes {
		if len(id) >= 8 && strings.HasPrefix(p, id) {
			return true
		}
	}
	return false
}

// logTemplate strips the key=value context from a geth message.
func logTemplate(msg string) string {
	if loc := logCtxRe.FindStringIndex(msg); loc != nil {
		msg = msg[:loc[0]]
	}
	return strings.TrimSpace(msg)
}

func scanLog(path string, n node, inv *inventory, warmup time.Time, probes []string) logsNode {
	ln := logsNode{Name: n.Name, Kind: n.Kind, Levels: map[string]int{}, Removed: map[string]int{},
		Templates: map[string]int{}, BadPacketErrs: map[string]int{}}
	f, err := os.Open(path)
	if err != nil {
		ln.Error = err.Error()
		return ln
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		m := logLineRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, m[1])
		level, rest := m[2], m[3]
		msg := logTemplate(rest)
		if ln.Lines == 0 {
			ln.First = ts
		}
		ln.Last = ts
		ln.Lines++
		ln.Levels[level]++
		switch level {
		case "WARN", "ERROR", "CRIT":
			ln.Templates[level+" "+msg]++
		}
		if strings.HasPrefix(msg, "Topic registration ") {
			if im := logIDRe.FindStringSubmatch(rest); im != nil && isProbe(im[1], probes) {
				ln.TopicRegProbe++
				continue
			}
		}
		switch {
		case msg == "Bad discv5 packet":
			ln.BadPacket++
			e := ""
			if em := logErrRe.FindStringSubmatch(rest); em != nil {
				e = em[1]
			}
			ln.BadPacketErrs[e]++
		case msg == "Removing p2p peer":
			kind := kindUnknown
			if im := logIDRe.FindStringSubmatch(rest); im != nil {
				kind = inv.kindOf(im[1])
			}
			ln.Removed[kind]++
		case msg == "Chain reorg detected" || msg == "Large chain reorg detected":
			ln.Reorgs++
			if warmup.IsZero() || ts.After(warmup) {
				ln.ReorgsAfterWarmup++
			}
		case msg == "Topic registration successful":
			ln.TopicRegOK++
		case msg == "Topic registration failed":
			ln.TopicRegFailed++
		case msg == "Topic query answered":
			ln.TopicQuery++
			if nm := logNRe.FindStringSubmatch(rest); nm != nil {
				if v, _ := strconv.Atoi(nm[1]); v > 0 {
					ln.TopicQueryNonzero++
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		ln.Error = fmt.Sprintf("%s: %v", path, err)
	}
	return ln
}
