package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	kindOld     = "old"
	kindNew     = "new"
	kindUnknown = "unknown"
)

// node is one EL entry of inventory.json, written by run.sh.
type node struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	IP    string  `json:"ip"`
	ID    string  `json:"id"`
	ENR   string  `json:"enr"`
	TD    *string `json:"td"`
	Image string  `json:"image"`
}

type inventory struct {
	Nodes []node
	byID  map[string]int
}

var (
	nodeIDRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	bootNameRe = regexp.MustCompile(`^el-0*1-`)
)

func readInventory(path string) (*inventory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	inv := new(inventory)
	if err := json.Unmarshal(b, &inv.Nodes); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if errs := inv.validate(); len(errs) > 0 {
		return inv, fmt.Errorf("%s: %s", path, strings.Join(errs, "; "))
	}
	return inv, nil
}

func (inv *inventory) validate() []string {
	var errs []string
	inv.byID = make(map[string]int)
	names, ips := make(map[string]bool), make(map[string]bool)
	if len(inv.Nodes) == 0 {
		return []string{"no nodes"}
	}
	for i, n := range inv.Nodes {
		switch {
		case n.Kind != kindOld && n.Kind != kindNew:
			errs = append(errs, fmt.Sprintf("%s: kind %q", n.Name, n.Kind))
		case n.Kind == kindNew && (n.TD == nil || *n.TD == ""):
			errs = append(errs, fmt.Sprintf("%s: new without td", n.Name))
		case n.Kind == kindOld && n.TD != nil:
			errs = append(errs, fmt.Sprintf("%s: old with td=%s", n.Name, *n.TD))
		}
		if !nodeIDRe.MatchString(n.ID) {
			errs = append(errs, fmt.Sprintf("%s: bad id %q", n.Name, n.ID))
		}
		if !strings.HasPrefix(n.ENR, "enr:") {
			errs = append(errs, fmt.Sprintf("%s: bad enr", n.Name))
		}
		if names[n.Name] {
			errs = append(errs, fmt.Sprintf("duplicate name %s", n.Name))
		}
		if ips[n.IP] {
			errs = append(errs, fmt.Sprintf("%s: duplicate ip %s", n.Name, n.IP))
		}
		if _, ok := inv.byID[n.ID]; ok {
			errs = append(errs, fmt.Sprintf("%s: duplicate id %s", n.Name, n.ID))
		}
		names[n.Name], ips[n.IP] = true, true
		inv.byID[n.ID] = i
	}
	return errs
}

// kindOf maps a full node ID or a log-abbreviated prefix to the node's kind.
func (inv *inventory) kindOf(id string) string {
	id = strings.ToLower(id)
	if i, ok := inv.byID[id]; ok {
		return inv.Nodes[i].Kind
	}
	if len(id) >= 8 && len(id) < 64 {
		for _, n := range inv.Nodes {
			if strings.HasPrefix(n.ID, id) {
				return n.Kind
			}
		}
	}
	return kindUnknown
}

// boot is participant 0, the bootnode and supernode.
func (inv *inventory) boot() node { return inv.Nodes[0] }

func (inv *inventory) count(kind string) int {
	c := 0
	for _, n := range inv.Nodes {
		if n.Kind == kind {
			c++
		}
	}
	return c
}

type inventoryResult struct {
	Nodes    []node `json:"nodes"`
	Old      int    `json:"old"`
	New      int    `json:"new"`
	Boot     string `json:"boot"`
	BootKind string `json:"boot_kind"`
}

func init() {
	register("inventory", "validate inventory.json and print a summary", runInventory)
}

func runInventory(args []string) error {
	fs := flag.NewFlagSet("inventory", flag.ExitOnError)
	rf := addRunFlags(fs)
	fs.Parse(args)
	if err := rf.check(); err != nil {
		return err
	}
	inv, err := rf.inventory()
	if err != nil {
		return err
	}
	if !bootNameRe.MatchString(inv.boot().Name) {
		return errors.New("first inventory entry is not participant 0 (el-1)")
	}
	res := inventoryResult{Nodes: inv.Nodes, Old: inv.count(kindOld), New: inv.count(kindNew),
		Boot: inv.boot().Name, BootKind: inv.boot().Kind}
	fmt.Printf("%d ELs: %d old, %d new; participant 0 %s (%s)\n", len(inv.Nodes), res.Old, res.New, res.Boot, res.BootKind)
	for _, n := range inv.Nodes {
		fmt.Printf("  %-24s %-3s %-15s %s %s\n", n.Name, n.Kind, n.IP, short(n.ID), n.Image)
	}
	return writeJSON(filepath.Join(rf.out, "inventory-check.json"), res)
}
