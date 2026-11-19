package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	statusPass         = "PASS"
	statusFail         = "FAIL"
	statusNA           = "n/a"
	statusInfo         = "info"
	statusMissing      = "missing"
	statusNeedsControl = "needs control"
	statusNeedsM3b     = "needs M3b"
)

type check struct {
	ID     string  `json:"id"`
	Desc   string  `json:"desc"`
	Status string  `json:"status"`
	Detail string  `json:"detail,omitempty"`
	Sub    []check `json:"sub,omitempty"`
}

func passIf(ok bool) string {
	if ok {
		return statusPass
	}
	return statusFail
}

// overall is FAIL if any check fails, else the first status that is neither PASS nor
// n/a nor info, else PASS if any check passed.
func overall(cs []check) string {
	st := statusNA
	for _, c := range cs {
		switch c.Status {
		case statusFail:
			return statusFail
		case statusPass:
			if st == statusNA {
				st = statusPass
			}
		case statusNA, statusInfo:
		default:
			if st == statusPass || st == statusNA {
				st = c.Status
			}
		}
	}
	return st
}

// judge is FAIL on any violation, else missing when data is incomplete, else PASS.
func judge(c *check, fails, missing []string, ok string) {
	switch {
	case len(fails) > 0:
		c.Status = statusFail
		c.Detail = firstN(fails, 4)
		if len(missing) > 0 {
			c.Detail += "; incomplete: " + firstN(missing, 3)
		}
	case len(missing) > 0:
		c.Status, c.Detail = statusMissing, "incomplete: "+firstN(missing, 4)
	default:
		c.Status, c.Detail = statusPass, ok
	}
}

var httpClient = &http.Client{Timeout: 5 * time.Second}

// URL builders, replaced in tests.
var (
	elURL      = func(n node) string { return "http://" + n.IP + ":8545" }
	metricsURL = func(n node) string { return "http://" + n.IP + ":9001/debug/metrics" }
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func rpcCall(ctx context.Context, url, method string, result any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("%s: %v", method, err)
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %s", method, r.Error.Message)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(r.Result, result)
}

func httpGetJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("GET %s: %s %s", url, resp.Status, bytes.TrimSpace(b))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func parseHexUint(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
}

// forEach runs f for every index in parallel.
func forEach(n int, f func(i int)) {
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f(i)
		}()
	}
	wg.Wait()
}

type runFlags struct {
	run, out string
}

func addRunFlags(fs *flag.FlagSet) *runFlags {
	r := new(runFlags)
	fs.StringVar(&r.run, "run", "", "run directory (inventory.json, logs/, genesis.txt)")
	fs.StringVar(&r.out, "out", "", "output directory (default: -run)")
	return r
}

func (r *runFlags) check() error {
	if r.run == "" {
		return errors.New("-run is required")
	}
	if r.out == "" {
		r.out = r.run
	}
	return os.MkdirAll(r.out, 0o755)
}

func (r *runFlags) inventory() (*inventory, error) {
	return readInventory(filepath.Join(r.run, "inventory.json"))
}

var datePrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-`)

// enclave derives the enclave name from a run directory <date>-d79-<run>.
func (r *runFlags) enclave() string {
	return datePrefix.ReplaceAllString(filepath.Base(filepath.Clean(r.run)), "")
}

// genesis reads genesis.txt (unix seconds) from the run directory.
func (r *runFlags) genesis() (time.Time, error) {
	b, err := os.ReadFile(filepath.Join(r.run, "genesis.txt"))
	if err != nil {
		return time.Time{}, err
	}
	s, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("genesis.txt: %v", err)
	}
	return time.Unix(s, 0).UTC(), nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// docker runs the docker CLI; replaced in tests.
var docker = func(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("docker %s: %v: %s", args[0], err, bytes.TrimSpace(ee.Stderr))
		}
		return nil, fmt.Errorf("docker %s: %v", args[0], err)
	}
	return out, nil
}

// enclaveServices lists service name → private IP for the enclave's containers.
func enclaveServices(enclave string) (map[string]string, []string, error) {
	out, err := docker("ps", "--filter", "label=kurtosis_enclave_name="+enclave,
		"--format", `{{.ID}} {{.Label "kurtosis_service_name"}} {{.Label "com.kurtosistech.private-ip"}}`)
	if err != nil {
		return nil, nil, err
	}
	ips := make(map[string]string)
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		ids = append(ids, f[0])
		ips[f[1]] = f[2]
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("no containers in enclave %s", enclave)
	}
	return ips, ids, nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func short(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}
