// Package distem drives a Distem coordinator (https://distem.gitlabpages.inria.fr)
// to turn reserved physical nodes into one virtual node per TopDisc node, each
// with its own address and per-peer latency.
package distem

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client speaks Distem's REST API: form-encoded parameters, hashes and
// arrays JSON-encoded inside them, as Distem's own Ruby client does.
type Client struct {
	Base string // http://coordinator:4567
	HTTP *http.Client
}

func New(coordinator string) *Client {
	if !strings.HasPrefix(coordinator, "http") {
		coordinator = "http://" + coordinator
	}
	if !strings.Contains(coordinator[7:], ":") {
		coordinator += ":4567"
	}
	return &Client{Base: coordinator, HTTP: &http.Client{Timeout: 15 * time.Minute}}
}

type params map[string]any

func (c *Client) do(method, route string, p params) ([]byte, error) {
	form := url.Values{}
	for k, v := range p {
		switch x := v.(type) {
		case string:
			form.Set(k, x)
		case bool:
			form.Set(k, fmt.Sprint(x))
		case int, int64, float64:
			form.Set(k, fmt.Sprint(x))
		case nil:
		default:
			b, err := json.Marshal(x)
			if err != nil {
				return nil, err
			}
			form.Set(k, string(b))
		}
	}
	req, err := http.NewRequest(method, c.Base+route, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, route, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// PnodeInit initialises physical nodes (what distem-bootstrap does); idempotent.
func (c *Client) PnodeInit(targets []string) error {
	_, err := c.do("POST", "/pnodes", params{"target": targets, "desc": map[string]any{}, "async": false})
	return err
}

func (c *Client) VnetworkCreate(name, cidr string) error {
	_, err := c.do("POST", "/vnetworks", params{"name": name, "address": cidr})
	return err
}

// VnodeDesc is the subset of Distem's vnode description we use.
type VnodeDesc struct {
	Host        string `json:"host,omitempty"`
	VFilesystem struct {
		Image  string `json:"image"`
		Shared bool   `json:"shared"`
		Cow    bool   `json:"cow,omitempty"`
	} `json:"vfilesystem"`
	VIfaces []map[string]string `json:"vifaces"`
}

// VnodesCreate creates several vnodes with one description (same host, same
// image, one interface on vnetwork; Distem assigns the addresses).
func (c *Client) VnodesCreate(names []string, desc VnodeDesc) error {
	_, err := c.do("POST", "/vnodes", params{"names": names, "desc": desc, "async": false})
	return err
}

func (c *Client) VnodesStart(names []string) error {
	_, err := c.do("PUT", "/vnodes", params{"names": names, "desc": map[string]string{"status": "RUNNING"}, "async": false, "type": "update"})
	return err
}

func (c *Client) VnodesRemove(names []string) error {
	p := params{"type": "remove"}
	if names != nil {
		p["names"] = names
	}
	_, err := c.do("PUT", "/vnodes", p)
	return err
}

func (c *Client) VnetworksRemove() error {
	_, err := c.do("DELETE", "/vnetworks", nil)
	return err
}

// WaitVnodes blocks until every named vnode accepts TCP on port, or timeout.
func (c *Client) WaitVnodes(names []string, port int, timeout time.Duration) (bool, error) {
	b, err := c.do("POST", "/wait_vnodes", params{"opts": map[string]any{"vnodes": names, "port": port, "timeout": int(timeout.Seconds())}})
	if err != nil {
		return false, err
	}
	return strings.Contains(string(b), "true"), nil
}

// SetPeersLatencies installs one-way delays (ms) from every vnode to every
// other; matrix[i][j] is the delay from names[i] to names[j].
func (c *Client) SetPeersLatencies(names []string, matrix [][]float64) error {
	_, err := c.do("POST", "/peers_matrix_latencies", params{"vnodes": names, "matrix": matrix})
	return err
}

// SetRate caps a vnode's egress and ingress, e.g. "160kbps".
func (c *Client) SetRate(vnode, iface, rate string) error {
	for _, dir := range []string{"output", "input"} {
		_, err := c.do("PUT", fmt.Sprintf("/vnodes/%s/ifaces/%s/%s", url.PathEscape(vnode), url.PathEscape(iface), dir),
			params{"desc": map[string]any{"bandwidth": map[string]string{"rate": rate}}})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) Execute(vnode, command string) (string, error) {
	b, err := c.do("POST", fmt.Sprintf("/vnodes/%s/commands", url.PathEscape(vnode)), params{"command": command})
	return string(b), err
}

// VnodeInfo is what we read back: name, host, status and the first
// interface's address (CIDR).
type VnodeInfo struct {
	Name    string
	Host    string
	Status  string
	Address string
}

func (c *Client) Vnodes() ([]VnodeInfo, error) {
	b, err := c.do("GET", "/vnodes", nil)
	if err != nil {
		return nil, err
	}
	var raw []map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("vnodes: %w", err)
	}
	out := make([]VnodeInfo, 0, len(raw))
	for _, v := range raw {
		vi := VnodeInfo{Name: str(v["name"]), Status: str(v["status"])}
		if h, ok := v["host"].(map[string]any); ok {
			vi.Host = str(h["address"])
		} else {
			vi.Host = str(v["host"])
		}
		if ifs, ok := v["vifaces"].([]any); ok && len(ifs) > 0 {
			if i0, ok := ifs[0].(map[string]any); ok {
				vi.Address = str(i0["address"])
			}
		}
		out = append(out, vi)
	}
	return out, nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
