package scenario

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SpecDir holds workloads/, profiles/ and models/; the CLI runs from the repo
// root, where every driver cds.
const SpecDir = "scenarios"

// Compose builds a run from a workload (scenario: only) and a backend profile
// (testbed: only) over Default(), then applies each key=value in set to the
// scenario. The key sets are disjoint, so the merge has no precedence.
// Relative model paths resolve against dir.
func Compose(dir, workload, profile string, set []string, name string) (Config, error) {
	c := Default()
	wpath, ppath := specPath(dir, "workloads", workload), specPath(dir, "profiles", profile)
	if err := decodeOnly(ppath, "testbed", &c); err != nil {
		return c, err
	}
	if err := decodeOnly(wpath, "scenario", &c); err != nil {
		return c, err
	}
	for _, kv := range set {
		if err := c.set(kv); err != nil {
			return c, err
		}
	}
	if name == "" {
		name = baseName(ppath) + "-" + baseName(wpath)
	}
	if strings.ContainsRune(name, '/') {
		return c, fmt.Errorf("name %q: no / allowed, it names the run directory", name)
	}
	c.Name = name
	c.SourcePath = filepath.Join(dir, c.Name+".yaml")
	c.Overrides = set
	if err := c.validate(wpath+" + "+ppath, dir); err != nil {
		return c, err
	}
	return c, nil
}

// A value with a path separator or a .yaml suffix is a path, anything else a
// name under dir/sub.
func specPath(dir, sub, n string) string {
	if strings.ContainsRune(n, filepath.Separator) || strings.HasSuffix(n, ".yaml") {
		return n
	}
	return filepath.Join(dir, sub, n+".yaml")
}

func baseName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".yaml")
}

func decodeOnly(path, key string, c *Config) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var top map[string]any
	if err := yaml.Unmarshal(b, &top); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if _, ok := top[key]; !ok || len(top) != 1 {
		return fmt.Errorf("%s: want exactly one top-level key %s:, have [%s]", path, key, strings.Join(sortedKeys(top), " "))
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: no %s: key", path, key)
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// loadStub composes a stub file (name, workload, profile). ok is false for
// anything else, which Load then decodes as a full scenario file.
func loadStub(path string, b []byte) (c Config, ok bool, err error) {
	var top map[string]any
	if yaml.Unmarshal(b, &top) != nil {
		return c, false, nil
	}
	_, w := top["workload"]
	_, p := top["profile"]
	if !w && !p {
		return c, false, nil
	}
	var stub struct{ Name, Workload, Profile string }
	for _, k := range sortedKeys(top) {
		switch k {
		case "name", "workload", "profile":
		default:
			return c, true, fmt.Errorf("%s: a stub holds only name, workload and profile, not %s:", path, k)
		}
	}
	if err := yaml.Unmarshal(b, &stub); err != nil {
		return c, true, fmt.Errorf("%s: %w", path, err)
	}
	if stub.Workload == "" || stub.Profile == "" {
		return c, true, fmt.Errorf("%s: a stub needs both workload and profile", path)
	}
	if stub.Name == "" {
		stub.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	c, err = Compose(filepath.Dir(path), stub.Workload, stub.Profile, nil, stub.Name)
	return c, true, err
}

// set applies key=value: key is a dotted path to a leaf under scenario:, and
// the YAML value replaces the field.
func (c *Config) set(kv string) error {
	k, v, _ := strings.Cut(kv, "=")
	if k == "" || v == "" {
		return fmt.Errorf("--set %q: want key=value", kv)
	}
	if k == "name" || k == "testbed" || strings.HasPrefix(k, "testbed.") {
		return fmt.Errorf("--set %s: only scenario keys can be set; the profile owns testbed", k)
	}
	rv := reflect.ValueOf(&c.Scenario).Elem()
	for _, part := range strings.Split(k, ".") {
		if rv.Kind() != reflect.Struct {
			return fmt.Errorf("--set %s: unknown key", k)
		}
		f, ok := fieldByTag(rv, part)
		if !ok {
			return fmt.Errorf("--set %s: unknown key", k)
		}
		rv = f
	}
	if rv.Kind() == reflect.Struct {
		return fmt.Errorf("--set %s: a section, not a single value", k)
	}
	// Zeroed first: yaml.v3 merges into a non-nil map.
	rv.Set(reflect.Zero(rv.Type()))
	if err := yaml.Unmarshal([]byte(v), rv.Addr().Interface()); err != nil {
		return fmt.Errorf("--set %s: %w", k, err)
	}
	return nil
}

func fieldByTag(rv reflect.Value, tag string) (reflect.Value, bool) {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).Tag.Get("yaml") == tag {
			return rv.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// Spec is a run as given on the command line: a scenario file, or a workload
// and a profile with overrides.
type Spec struct {
	File              string
	Workload, Profile string
	Set               []string
	Name              string
}

// ScanSpec reads the leading spec arguments and returns how many it took. It
// stops at the first argument that is not one of its flags, so terraform's
// -var never reaches it.
func ScanSpec(args []string) (s Spec, n int, err error) {
	if len(args) == 0 {
		return s, 0, errors.New("missing scenario: a file, or --workload W --profile P")
	}
	if !strings.HasPrefix(args[0], "--") {
		s.File = args[0]
		return s, 1, nil
	}
	for n < len(args) {
		a := args[n]
		if !strings.HasPrefix(a, "--") {
			break
		}
		k, v, eq := strings.Cut(a[2:], "=")
		var dst *string
		switch k {
		case "workload":
			dst = &s.Workload
		case "profile":
			dst = &s.Profile
		case "name":
			dst = &s.Name
		case "set":
			dst = new(string)
		}
		if dst == nil {
			break
		}
		if !eq {
			if n+1 >= len(args) {
				return s, n, fmt.Errorf("--%s needs a value", k)
			}
			v = args[n+1]
			n++
		}
		n++
		*dst = v
		if k == "set" {
			s.Set = append(s.Set, v)
		}
	}
	if s.Workload == "" || s.Profile == "" {
		return s, n, errors.New("--workload and --profile are both required")
	}
	return s, n, nil
}

// Load resolves the spec from SpecDir.
func (s Spec) Load() (Config, error) {
	if s.File != "" {
		return Load(s.File)
	}
	return Compose(SpecDir, s.Workload, s.Profile, s.Set, s.Name)
}

// ParseArgs resolves a spec that must use every argument.
func ParseArgs(args []string) (Config, error) {
	s, n, err := ScanSpec(args)
	if err != nil {
		return Config{}, err
	}
	if n < len(args) {
		return Config{}, fmt.Errorf("unexpected argument %q after the scenario", args[n])
	}
	return s.Load()
}

// ComposeLine records an ad-hoc composition in run.log; empty for a file.
func (s Spec) ComposeLine() string {
	if s.File != "" {
		return ""
	}
	return fmt.Sprintf("compose: workload=%s profile=%s set=%v", s.Workload, s.Profile, s.Set)
}

// ComposedYAML is the run as one self-contained scenario file: what the
// drivers ship to the coordinator.
func (c Config) ComposedYAML() ([]byte, error) {
	return yaml.Marshal(struct {
		Name      string         `yaml:"name"`
		Overrides []string       `yaml:"overrides,omitempty"`
		Scenario  ScenarioConfig `yaml:"scenario"`
		Testbed   TestbedConfig  `yaml:"testbed"`
	}{c.Name, c.Overrides, c.Scenario, c.Testbed})
}
