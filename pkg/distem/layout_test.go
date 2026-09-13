package distem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/datahop/topdisc-testbed/pkg/scenario"
	"github.com/datahop/topdisc-testbed/pkg/wan"
)

func table() *wan.RTTTable {
	return &wan.RTTTable{Intra: 2, RTT: map[string]map[string]float64{
		"us-east-1": {"eu-central-1": 90, "ap-southeast-1": 220}, "eu-central-1": {"ap-southeast-1": 160}}}
}

func TestLayout(t *testing.T) {
	cfg := scenario.Default()
	cfg.Scenario.Population.Nodes = 100
	cfg.Scenario.Network.Regions = map[string]float64{"us-east-1": 0.4, "eu-central-1": 0.3, "ap-southeast-1": 0.3}
	cfg.Scenario.Network.NodeRegions = map[int]string{0: "ap-southeast-1"}
	p, err := Layout(cfg, []string{"pn1", "pn2", "pn3"}, table())
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	per := map[string]int{}
	for _, v := range p.Vnodes {
		count[v.Region]++
		per[v.Pnode]++
	}
	want := cfg.Fleet()
	for r, n := range want {
		if count[r] != n {
			t.Fatalf("region %s: %d vnodes, fleet says %d", r, count[r], n)
		}
	}
	if per["pn1"] != 34 || per["pn2"] != 33 || per["pn3"] != 33 {
		t.Fatalf("round robin: %v", per)
	}
	if p.Vnodes[0].Region != "us-east-1" {
		t.Fatalf("home region first, got %s", p.Vnodes[0].Region)
	}
	// us-east-1 <-> eu-central-1: 45 ms one way; same region 1 ms; diagonal 0.
	var us, eu int
	for i, v := range p.Vnodes {
		if v.Region == "us-east-1" && us == 0 {
			us = i
		}
		if v.Region == "eu-central-1" {
			eu = i
		}
	}
	if p.Matrix[us][eu] != 45 || p.Matrix[eu][us] != 45 || p.Matrix[us][us] != 0 || p.Matrix[us][us+1] != 1 {
		t.Fatalf("matrix: us->eu %v eu->us %v self %v intra %v", p.Matrix[us][eu], p.Matrix[eu][us], p.Matrix[us][us], p.Matrix[us][us+1])
	}
}

// The wire format: form-encoded, hashes/arrays as JSON strings, as Distem's
// Ruby client sends them.
func TestClientEncoding(t *testing.T) {
	var got url.Values
	var route, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got, route, method = r.PostForm, r.URL.Path, r.Method
		if r.URL.Path == "/vnodes" && r.Method == "GET" {
			w.Write([]byte(`[{"name":"vn0","status":"RUNNING","host":{"address":"pn1"},"vifaces":[{"name":"if0","address":"10.144.0.2/22"}]}]`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	var d VnodeDesc
	d.Host, d.VFilesystem.Image, d.VFilesystem.Shared = "pn1", "file:///img.tgz", true
	d.VIfaces = []map[string]string{{"name": "if0", "vnetwork": "topdisc"}}
	if err := c.VnodesCreate([]string{"vn0", "vn1"}, d); err != nil {
		t.Fatal(err)
	}
	if method != "POST" || route != "/vnodes" || got.Get("names") != `["vn0","vn1"]` {
		t.Fatalf("create: %s %s names=%q", method, route, got.Get("names"))
	}
	var desc map[string]any
	if err := json.Unmarshal([]byte(got.Get("desc")), &desc); err != nil || desc["host"] != "pn1" {
		t.Fatalf("desc not JSON with host: %q", got.Get("desc"))
	}
	if err := c.SetPeersLatencies([]string{"vn0", "vn1"}, [][]float64{{0, 45}, {45, 0}}); err != nil {
		t.Fatal(err)
	}
	if got.Get("matrix") != `[[0,45],[45,0]]` || !strings.HasSuffix(route, "peers_matrix_latencies") {
		t.Fatalf("matrix: %q %s", got.Get("matrix"), route)
	}
	infos, err := c.Vnodes()
	if err != nil || len(infos) != 1 || infos[0].Address != "10.144.0.2/22" || infos[0].Host != "pn1" {
		t.Fatalf("vnodes: %v %v", infos, err)
	}
	if err := c.SetOutputRate("vn0", "if0", "160kbps"); err != nil {
		t.Fatal(err)
	}
	if route != "/vnodes/vn0/ifaces/if0/output" || !strings.Contains(got.Get("desc"), `"rate":"160kbps"`) {
		t.Fatalf("rate: %s %q", route, got.Get("desc"))
	}
}
