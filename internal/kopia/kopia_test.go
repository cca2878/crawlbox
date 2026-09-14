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

// Candidate roots change per run. The stable source must still let Kopia reuse
// unchanged file objects rather than reading and hashing their bytes again.
func TestIncrementalSnapshotAcrossCandidateDirectories(t *testing.T) {
	k := testutil.Kopia(t)
	first := t.TempDir()
	p := filepath.Join(first, "unchanged")
	if err := os.WriteFile(p, []byte("unchanged bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Create(t.Context(), first, "incremental", "r1"); err != nil {
		t.Fatal(err)
	}
	second := t.TempDir()
	if err := os.Link(p, filepath.Join(second, "unchanged")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "added"), []byte("new bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := k.Create(t.Context(), second, "incremental", "r2")
	if err != nil {
		t.Fatal(err)
	}
	if s.Stats.CachedFiles != 1 {
		t.Fatalf("unchanged file was not reused: %+v", s.Stats)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if err := k.Restore(t.Context(), s.ID, "", dst); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"unchanged": "unchanged bytes", "added": "new bytes"} {
		b, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
}
