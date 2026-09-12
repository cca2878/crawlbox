// Package kopia isolates the documented command-line interface from application types.
package kopia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

type Snapshot struct {
	ID         string            `json:"id"`
	Incomplete string            `json:"incomplete"`
	Tags       map[string]string `json:"tags"`
	Pins       []string          `json:"pins"`
	Root       struct {
		Object string `json:"obj"`
	} `json:"rootEntry"`
	Stats struct {
		Errors       int `json:"errorCount"`
		Ignored      int `json:"ignoredErrorCount"`
		Excluded     int `json:"excludedFileCount"`
		ExcludedDirs int `json:"excludedDirCount"`
	} `json:"stats"`
}

func (s *Snapshot) normalize() {
	tags := map[string]string{}
	for k, v := range s.Tags {
		if strings.HasPrefix(k, "tag:") {
			tags[strings.TrimPrefix(k, "tag:")] = v
		}
	}
	s.Tags = tags
}

func (s Snapshot) Complete() bool {
	return s.ID != "" && s.Root.Object != "" && s.Incomplete == "" && s.Stats.Errors == 0 && s.Stats.Ignored == 0 && s.Stats.Excluded == 0 && s.Stats.ExcludedDirs == 0 && len(s.Pins) > 0
}

type Backend interface {
	Create(context.Context, string, string, string) (Snapshot, error)
	List(context.Context) ([]Snapshot, error)
	Restore(context.Context, string, string, string) error
}
type CLI struct {
	Binary, Config string
	mu             sync.Mutex
	cache          map[string]Snapshot
}

func (c *CLI) command(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Binary, append([]string{"--config-file", c.Config, "--no-progress", "--disable-file-logging"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, e := cmd.Output()
	if e != nil {
		return nil, fmt.Errorf("kopia %s failed: %w", args[0], e)
	}
	return out, nil
}
func (c *CLI) Create(ctx context.Context, dir, source, revision string) (Snapshot, error) {
	var s Snapshot
	b, e := c.command(ctx, "snapshot", "create", dir, "--json", "--fail-fast", "--force-hash=100", "--force-disable-actions", "--pin=business", "--tags=app:manager-v1", "--tags=source:"+source, "--tags=revision:"+revision)
	if e != nil {
		return s, e
	}
	if e = json.Unmarshal(b, &s); e != nil {
		return s, e
	}
	s.normalize()
	if !s.Complete() {
		return s, errors.New("snapshot is not complete and pinned")
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]Snapshot{}
	}
	c.cache[s.ID] = s
	c.mu.Unlock()
	return s, nil
}
func (c *CLI) List(ctx context.Context) ([]Snapshot, error) {
	b, e := c.command(ctx, "snapshot", "list", "--all", "--show-identical", "--json", "--tags=app:manager-v1")
	if e != nil {
		return nil, e
	}
	var list []Snapshot
	if e = json.Unmarshal(b, &list); e != nil {
		return nil, e
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]Snapshot{}
	}
	for i := range list {
		list[i].normalize()
		s := list[i]
		c.cache[s.ID] = s
	}
	c.mu.Unlock()
	return list, nil
}
func (c *CLI) Restore(ctx context.Context, id, relative, target string) error {
	c.mu.Lock()
	s, ok := c.cache[id]
	c.mu.Unlock()
	if !ok {
		if _, e := c.List(ctx); e != nil {
			return e
		}
		c.mu.Lock()
		s, ok = c.cache[id]
		c.mu.Unlock()
	}
	if !ok || !s.Complete() {
		return errors.New("complete snapshot not found")
	}
	ref := s.Root.Object
	if relative != "" {
		ref += "/" + relative
	}
	_, e := c.command(ctx, "snapshot", "restore", ref, target)
	return e
}
