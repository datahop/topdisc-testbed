package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadYAML(t *testing.T, body string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	return err
}

func TestLoadSearchModes(t *testing.T) {
	for _, tc := range []struct {
		body    string
		wantErr string
	}{
		{"scenario:\n  search:\n    model: conn\n  conn_model:\n    enabled: true\n", ""},
		{"scenario:\n  search:\n    model: scheduled\n", ""},
		{"scenario:\n  search:\n    model: continuous\n    initial_results: 16\n    result_interval: 1m\n", ""},
		{"scenario:\n  search:\n    model: scheduled\n  conn_model:\n    enabled: true\n", "lookup-only"},
		{"scenario:\n  search:\n    model: continuous\n  conn_model:\n    enabled: true\n", "lookup-only"},
		{"scenario:\n  search:\n    model: sometimes\n", "want conn, scheduled or continuous"},
	} {
		err := loadYAML(t, tc.body)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%q: unexpected error %v", tc.body, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%q: error %v, want %q", tc.body, err, tc.wantErr)
		}
	}
}

func TestLoadLegacyBuild(t *testing.T) {
	all := "    legacy_frac: 1\n    legacy_bootnode: true\n"
	for _, tc := range []struct {
		body    string
		wantErr string
	}{
		{"scenario:\n  population:\n" + all + "    legacy_build: fork\n", ""},
		{"scenario:\n  population:\n    legacy_frac: 0.5\n    legacy_build: stock\n", ""},
		{"scenario:\n  population:\n    legacy_frac: 1\n    legacy_build: fork\n", "every node legacy"},
		{"scenario:\n  population:\n    legacy_frac: 0.5\n    legacy_bootnode: true\n    legacy_build: fork\n", "every node legacy"},
		{"scenario:\n  population:\n" + all + "    legacy_build: upstream\n", "want stock or fork"},
	} {
		err := loadYAML(t, tc.body)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%q: unexpected error %v", tc.body, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%q: error %v, want %q", tc.body, err, tc.wantErr)
		}
	}
}
