package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"example.org/crawler/manager/internal/catalog"
	"example.org/crawler/manager/internal/config"
	rt "example.org/crawler/manager/internal/runtime"
	"example.org/crawler/manager/internal/testutil"
	"example.org/crawler/manager/internal/web"
	"go.yaml.in/yaml/v3"
)

// Exercise the actual executable, without entrypoints or container environment.
func TestStandaloneDeployment(t *testing.T) {
	binary, wasm := os.Getenv("MANAGER_BIN"), os.Getenv("FIXTURE_WASM")
	if binary == "" || wasm == "" {
		t.Skip("MANAGER_BIN and FIXTURE_WASM required")
	}
	k := testutil.Kopia(t)
	work := filepath.Join(t.TempDir(), "standalone deployment")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "KOPIA_") || strings.HasPrefix(key, "CRAWLBOX_") || key == "PATH" {
			continue
		}
		env = append(env, value)
	}
	// No shell, jq, openssl or flock executable is available to the manager.
	env = append(env, "PATH="+filepath.Join(work, "empty-path"))
	cli := func(input string, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = work
		cmd.Env = env
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("manager command %s: %v", args[0], err)
		}
		return out
	}
	password := "standalone-admin-password"
	credentials := web.Credentials{Username: "admin", PasswordHash: strings.TrimSpace(string(cli(password, "hash-password")))}
	writeYAML := func(name string, value any) {
		t.Helper()
		b, err := yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(work, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeYAML("admin.yaml", credentials)
	address := func() string {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		return ln.Addr().String()
	}
	b, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Listen: address(), DataDir: "data", Credentials: "admin.yaml", KopiaBinary: k.Binary, KopiaConfig: k.Config,
		Sources: []config.Source{{ID: "standalone", Plugin: wasm, SHA256: rt.Sum(b), Config: map[string]any{"files": map[string]string{"file.txt": "standalone bytes"}}}}}
	writeYAML("config.yaml", cfg)
	log, err := os.Create(filepath.Join(work, "manager.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var process *exec.Cmd
	var exited chan error
	stop := func() {
		t.Helper()
		if process == nil {
			return
		}
		_ = process.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("manager exit: %v", err)
			}
		case <-time.After(15 * time.Second):
			_ = process.Process.Kill()
			<-exited
			t.Error("manager did not stop")
		}
		process = nil
	}
	t.Cleanup(stop)
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path, body, token string) (int, string, error) {
		r, err := http.NewRequest(method, "http://"+cfg.Listen+path, strings.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		} else {
			r.SetBasicAuth("admin", password)
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		response, err := client.Do(r)
		if err != nil {
			return 0, "", err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		return response.StatusCode, string(data), err
	}
	start := func() {
		t.Helper()
		process = exec.Command(binary, "-config", "config.yaml")
		process.Env = env
		process.Dir = work
		process.Stdout = log
		process.Stderr = log
		if err = process.Start(); err != nil {
			t.Fatal(err)
		}
		exited = make(chan error, 1)
		go func(cmd *exec.Cmd, ch chan error) { ch <- cmd.Wait() }(process, exited)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case err := <-exited:
				process = nil
				t.Fatalf("standalone manager exited: %v", err)
			default:
			}
			if status, _, err := request("GET", "/ui/", "", ""); err == nil && status == 200 {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("standalone manager readiness timed out")
	}
	start()
	if _, body, _ := request("GET", "/ui/", "", ""); strings.Contains(body, "Kopia Web UI proxy") {
		t.Fatal("unconfigured proxy shown")
	}
	if status, _, _ := request("POST", "/ui/kopia-ui-proxy", "enabled=true", ""); status != 404 {
		t.Fatal("unconfigured proxy enabled")
	}
	duplicateContext, cancelDuplicate := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDuplicate()
	duplicate := exec.CommandContext(duplicateContext, binary, "-config", "config.yaml")
	duplicate.Dir = work
	duplicate.Env = env
	if out, err := duplicate.CombinedOutput(); err == nil || !strings.Contains(string(out), "data directory in use") {
		t.Fatal("duplicate process was not rejected")
	}
	if status, _, _ := request("POST", "/ui/sources/standalone/trigger", "", ""); status != 303 {
		t.Fatal("trigger failed")
	}
	store, err := catalog.Open(filepath.Join(work, "data/catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err = store.Revision(context.Background(), "standalone", "latest"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("standalone collection did not publish")
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, token, err := store.CreateToken(context.Background(), "reader", []string{"standalone"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	readFile := func() {
		t.Helper()
		status, body, err := request("GET", "/api/v1/sources/standalone/revisions/latest/files/file.txt", "", token)
		if err != nil || status != 200 || body != "standalone bytes" {
			t.Fatalf("read standalone history: status=%d err=%v", status, err)
		}
	}
	readFile()
	oldPassword := password
	password = "standalone-new-password"
	cli(password, "reset-password", "admin.yaml")
	stop()
	if err = os.RemoveAll(filepath.Join(work, "data/current")); err != nil {
		t.Fatal(err)
	}
	// Optional proxy can be configured entirely in YAML, without Docker defaults.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "manual proxy") }))
	defer upstream.Close()
	cfg.KopiaUIProxy = config.KopiaUIProxy{Listen: address(), Target: upstream.URL}
	writeYAML("config.yaml", cfg)
	start()
	readFile() // Current files were restored over the original Kopia connection.
	password, oldPassword = oldPassword, password
	if status, _, _ := request("GET", "/ui/", "", ""); status != 401 {
		t.Fatal("old password still works")
	}
	password = oldPassword
	if status, _, _ := request("POST", "/ui/kopia-ui-proxy", "enabled=true", ""); status != 303 {
		t.Fatal("manual proxy enable failed")
	}
	response, err := client.Get("http://" + cfg.KopiaUIProxy.Listen)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || string(data) != "manual proxy" {
		t.Fatal("manual proxy failed")
	}
	stop()
	contents, err := os.ReadFile(filepath.Join(work, "manager.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{password, "standalone-admin-password", token} {
		if strings.Contains(string(contents), secret) {
			t.Fatal("secret in standalone log")
		}
	}
	var parsed map[string]any
	// Verify the configuration remains YAML written by the operator, not regenerated.
	contents, err = os.ReadFile(filepath.Join(work, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if yaml.Unmarshal(contents, &parsed) != nil || parsed["credentials"] != "admin.yaml" {
		t.Fatal("operator config changed")
	}
}
