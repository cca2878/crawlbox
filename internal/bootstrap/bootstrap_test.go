package bootstrap

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.org/crawler/manager/internal/kopia"
)

func TestBootstrapRealServerRestart(t *testing.T) {
	binary := os.Getenv("KOPIA_BIN")
	if binary == "" {
		t.Skip("KOPIA_BIN required")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "https://" + listener.Addr().String()
	listener.Close()
	root := t.TempDir()
	server := Options{binary, filepath.Join(root, "server"), filepath.Join(root, "shared"), address}
	client := Options{binary, filepath.Join(root, "client"), server.Shared, address}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var firstSecrets, firstConnection []byte
	for attempt := range 2 {
		cmd, err := server.Server(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		stop := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
		t.Cleanup(stop)
		if err = client.Connect(ctx); err != nil {
			stop()
			t.Fatal(err)
		}
		cli := &kopia.CLI{Binary: binary, Config: filepath.Join(client.Data, "connection", "repository.config")}
		if attempt == 0 {
			dir := filepath.Join(root, "files")
			os.Mkdir(dir, 0700)
			if err = os.WriteFile(filepath.Join(dir, "hello"), []byte("survives restart"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = cli.Create(ctx, dir, "fixture", "first"); err != nil {
				t.Fatal(err)
			}
			firstSecrets, _ = os.ReadFile(filepath.Join(server.Data, "secrets.json"))
			firstConnection, _ = os.ReadFile(filepath.Join(server.Shared, "connection.json"))
		} else {
			snapshots, err := cli.List(ctx)
			if err != nil || len(snapshots) != 1 {
				t.Fatalf("snapshots %v: %v", snapshots, err)
			}
			b, _ := os.ReadFile(filepath.Join(server.Data, "secrets.json"))
			if string(b) != string(firstSecrets) {
				t.Fatal("secrets changed")
			}
			b, _ = os.ReadFile(filepath.Join(server.Shared, "connection.json"))
			if string(b) != string(firstConnection) {
				t.Fatal("connection changed")
			}
		}
		stop()
		// Simulate losing only derived connection configs, keeping durable secrets/repo.
		if attempt == 0 {
			os.Remove(filepath.Join(server.Data, "repository.config"))
			os.RemoveAll(filepath.Join(client.Data, "connection"))
		}
	}
	var s secrets
	json.Unmarshal(firstSecrets, &s)
	if string(firstConnection) == "" || s.Repository == s.Worker {
		t.Fatal("invalid independent secrets")
	}
	if err = os.Remove(filepath.Join(server.Data, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = server.Server(ctx); err == nil {
		t.Fatal("must not recreate secrets for existing repository")
	}
}
func TestConnectCancellation(t *testing.T) {
	o := Options{Data: t.TempDir(), Shared: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.Connect(ctx); err == nil {
		t.Fatal("expected cancellation")
	}
}
func TestExclusiveLock(t *testing.T) {
	dir := t.TempDir()
	f, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if second, err := Lock(dir); err == nil {
		second.Close()
		t.Fatal("duplicate lock accepted")
	}
}

func TestInitializeOfficialServerScript(t *testing.T) {
	binary := os.Getenv("KOPIA_BIN")
	if binary == "" {
		t.Skip("KOPIA_BIN required")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	o := Options{binary, filepath.Join(root, "server's data"), filepath.Join(root, "shared"), "https://0.0.0.0:51515"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = o.Initialize(ctx, ""); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(o.Data, "start-server.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "exec /bin/kopia") || strings.Contains(string(script), binary) {
		t.Fatal("script does not use official binary")
	}
	check := exec.Command("sh", "-n", filepath.Join(o.Data, "start-server.sh"))
	if err = check.Run(); err != nil {
		t.Fatal(err)
	}
	shared, err := os.ReadFile(filepath.Join(o.Shared, "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(o.Data, "secrets.json"))
	var s secrets
	json.Unmarshal(b, &s)
	if strings.Contains(string(shared), s.Repository) || strings.Contains(string(shared), s.Server) {
		t.Fatal("server secrets exposed to manager")
	}
	if err = o.Initialize(ctx, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(o.Data, "secrets.json"))
	if string(b) != string(after) {
		t.Fatal("secrets changed")
	}
}
