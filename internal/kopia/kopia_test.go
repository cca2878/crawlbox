package kopia_test

import (
	"context"
	"github.com/cca2878/crawlbox/internal/testutil"
	"os"
	"path/filepath"
	"testing"
)

func TestRealServerSnapshotRestore(t *testing.T) {
	k := testutil.Kopia(t)
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "hello"), []byte("historic bytes"), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := k.Create(context.Background(), dir, "fixture", "revision1")
	if e != nil {
		t.Fatal(e)
	}
	list, e := k.List(context.Background())
	if e != nil || len(list) != 1 || !list[0].Complete() || list[0].Tags["app"] != "manager-v1" {
		t.Fatalf("list %+v %v", list, e)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if e = k.Restore(context.Background(), s.ID, "hello", dst); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(dst)
	if e != nil || string(b) != "historic bytes" {
		t.Fatalf("%s %v", b, e)
	}
}
