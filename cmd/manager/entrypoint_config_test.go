package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cca2878/crawlbox/internal/config"
)

func TestEntrypointGeneratesEditableYAML(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "flock", "sync", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("entrypoint requires %s", tool)
		}
	}
	// Exercise YAML-significant characters and Unicode without shell interpolation.
	storage := filepath.Join(t.TempDir(), "data: # \"quoted\" \\ 中文\nline")
	client := filepath.Join(storage, "connection", "custom: # \\"+"\".config")
	kopia := "/opt/custom: # \"quoted\"/kopia"
	run := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", "../../docker/manager-entrypoint.sh")
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "CRAWLBOX_") && !strings.HasPrefix(entry, "KOPIA_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "CRAWLBOX_DATA="+storage, "CRAWLBOX_BOOTSTRAP=", "KOPIA_CONFIG_PATH="+client, "KOPIA_BINARY="+kopia)
		output, err := cmd.CombinedOutput()
		// Preparation writes the config before detecting the deliberately absent connection.
		if err == nil || !strings.Contains(string(output), "No Kopia client configuration") {
			t.Fatalf("unexpected preparation result: %v %s", err, output)
		}
	}
	run()
	path := filepath.Join(storage, "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "listen:") || json.Valid(raw) {
		t.Fatalf("expected block YAML, got %s", raw)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != storage || c.Credentials != filepath.Join(storage, "admin.yaml") || c.KopiaConfig != client || c.KopiaBinary != kopia || c.Listen != ":8080" {
		t.Fatalf("configuration values changed: %+v", c)
	}
	// A documented block-style sources entry must be directly editable into the file.
	edited := strings.Replace(string(raw), "sources: []", "sources:\n  - id: example\n    plugin: /extensions/example/plugin.wasm\n    sha256: \""+strings.Repeat("a", 64)+"\"\n    schedule: \"\"", 1)
	if err := os.WriteFile(path, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = config.Load(path)
	if err != nil || len(c.Sources) != 1 {
		t.Fatalf("edited YAML did not load: %v", err)
	}
	run()
	after, err := os.ReadFile(path)
	if err != nil || string(after) != edited {
		t.Fatal("existing YAML was overwritten")
	}
	legacy := `{"listen":":8080","sources":[]}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	run()
	after, err = os.ReadFile(path)
	if err != nil || string(after) != legacy {
		t.Fatal("existing JSON was overwritten")
	}
}
