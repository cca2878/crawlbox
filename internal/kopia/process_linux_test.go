//go:build linux

package kopia

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestKopiaChildDiesWithManager(t *testing.T) {
	if binary := os.Getenv("CRAWLBOX_DEATH_TEST_BINARY"); binary != "" {
		c := CLI{Binary: binary, Config: "unused"}
		_, _ = c.command(context.Background(), "snapshot", "create")
		return
	}
	root := t.TempDir()
	script, pidFile := filepath.Join(root, "kopia"), filepath.Join(root, "pid")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho $$ > \"$CRAWLBOX_DEATH_TEST_PID\"\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(os.Args[0], "-test.run=^TestKopiaChildDiesWithManager$")
	parent.Env = append(os.Environ(), "CRAWLBOX_DEATH_TEST_BINARY="+script, "CRAWLBOX_DEATH_TEST_PID="+pidFile)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
	var pid int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			if pid > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child did not start")
	}
	child, _ := os.FindProcess(pid)
	defer child.Kill()
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) {
			return
		}
		if err == nil {
			_, rest, _ := strings.Cut(string(b), ") ")
			if strings.HasPrefix(rest, "Z ") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Kopia child survived parent SIGKILL")
}
