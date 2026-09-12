package model

import (
	"encoding/json"
	"example.org/crawler/manager/internal/wire"
	"time"
)

type Change struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}
type Revision struct {
	ID              string          `json:"id"`
	Run             string          `json:"run,omitempty"`
	Source          string          `json:"source"`
	Parent          string          `json:"parent,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	Plugin          wire.Descriptor `json:"plugin"`
	State           json.RawMessage `json:"state"`
	Metadata        json.RawMessage `json:"metadata"`
	Files           []wire.Entry    `json:"files"`
	Artifacts       []wire.Entry    `json:"artifacts"`
	Changes         []Change        `json:"changes"`
	Tags            []string        `json:"tags,omitempty"`
	ArtifactChanges []Change        `json:"artifact_changes"`
	Snapshot        string          `json:"snapshot,omitempty"`
}
type Run struct {
	ID       string     `json:"id"`
	Source   string     `json:"source"`
	Status   string     `json:"status"`
	Progress string     `json:"progress"`
	Error    string     `json:"error"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
}
