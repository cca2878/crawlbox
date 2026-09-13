package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cca2878/crawlbox/internal/config"
	"github.com/cca2878/crawlbox/internal/testutil"
	"github.com/cca2878/crawlbox/internal/web"
	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"
)

// The server is started solely with the standard Kopia CLI, never the wrapper.
// CI also executes this test using the real manager image, without a Kopia image.
func TestExternalManagerEntrypoint(t *testing.T) {
	binary := os.Getenv("MANAGER_BIN")
	image := os.Getenv("DOCKER_MANAGER_IMAGE")
	if binary == "" && image == "" {
		t.Skip("MANAGER_BIN or DOCKER_MANAGER_IMAGE required")
	}
	k := testutil.Kopia(t)
	work := t.TempDir()
	script, err := filepath.Abs("../../docker/manager-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	ln.Close()
	cfg := config.Config{Listen: address, DataDir: work, Credentials: filepath.Join(work, "admin.yaml"), KopiaBinary: k.Binary, KopiaConfig: k.Config}
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "KOPIA_") || strings.HasPrefix(key, "CRAWLBOX_") {
			continue
		}
		env = append(env, value)
	}
	env = append(env, "CRAWLBOX_DATA="+work, "KOPIA_CONFIG_PATH="+k.Config, "KOPIA_BINARY="+k.Binary, "MANAGER_BINARY="+binary)
	name := "crawlbox-external-" + fmt.Sprint(os.Getpid())
	dockerBase := []string{"run", "--rm", "--network=host", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--mount", "type=bind,src=" + work + ",dst=/data"}
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", args...)
		if err := cmd.Run(); err != nil {
			t.Fatalf("docker operation: %v", err)
		}
	}
	if image != "" {
		// Create a client profile with container paths using Kopia's documented CLI.
		var profile struct {
			APIServer struct {
				URL string `json:"url"`
				Pin string `json:"serverCertFingerprint"`
			} `json:"apiServer"`
		}
		data, err := os.ReadFile(k.Config)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &profile); err != nil {
			t.Fatal(err)
		}
		args := append(append([]string{}, dockerBase...), "-e", "KOPIA_PASSWORD=integration-only", "--entrypoint", "kopia", image,
			"--config-file", "/data/client.config", "--disable-file-logging", "repository", "connect", "server", "--url", profile.APIServer.URL,
			"--server-cert-fingerprint", profile.APIServer.Pin, "--override-username=worker", "--override-hostname=manager",
			"--cache-directory", "/data/kopia-cache", "--persist-credentials")
		run(args...)
		cfg.DataDir = "/data"
		cfg.Credentials = "/data/admin.yaml"
		cfg.KopiaBinary = "kopia"
		cfg.KopiaConfig = "/data/client.config"
	}
	password := "external-manager-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for filename, value := range map[string]any{"config.yaml": cfg, "admin.yaml": web.Credentials{Username: "admin", PasswordHash: string(hash)}} {
		data, err := yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(work, filename), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	client := &http.Client{Timeout: time.Second}
	for attempt := 0; attempt < 2; attempt++ {
		var cmd *exec.Cmd
		if image != "" {
			args := append(append([]string{}, dockerBase...), "--name", name, "-e", "KOPIA_CONFIG_PATH=/data/client.config", image)
			cmd = exec.Command("docker", args...)
		} else {
			cmd = exec.Command("bash", script)
			cmd.Env = env
		}
		log, err := os.Create(filepath.Join(work, fmt.Sprintf("run-%d.log", attempt)))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = log
		cmd.Stderr = log
		if err = cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		cleanup := func() {
			if image != "" {
				_ = exec.Command("docker", "rm", "-f", name).Run()
			} else {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				_ = cmd.Process.Kill()
				t.Error("external manager did not stop")
			}
			log.Close()
		}
		ready := false
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
			r, _ := http.NewRequest("GET", "http://"+address+"/ui/", nil)
			r.SetBasicAuth("admin", password)
			response, err := client.Do(r)
			if err == nil {
				response.Body.Close()
				if response.StatusCode == 200 {
					ready = true
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		cleanup()
		if !ready {
			t.Fatal("manager required wrapper bootstrap or failed external startup")
		}
		data, err := os.ReadFile(log.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "Kopia UI proxy listening") || strings.Contains(string(data), password) {
			t.Fatal("unexpected proxy defaults or secret logging")
		}
	}
	if _, err = os.Stat(filepath.Join(work, "connection.json")); !os.IsNotExist(err) {
		t.Fatal("unexpected wrapper handoff")
	}
}
