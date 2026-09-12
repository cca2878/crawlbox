//go:build wasip1

package main

import (
	"encoding/json"
	"errors"
	"example.org/crawler/manager/internal/wire"
	"github.com/extism/go-pdk"
)

//go:wasmimport extism:host/user call
func hostCall(uint64) uint64
func call(q wire.Request) (wire.Response, error) {
	var r wire.Response
	m, e := pdk.AllocateJSON(q)
	if e != nil {
		return r, e
	}
	defer m.Free()
	out := pdk.FindMemory(hostCall(m.Offset()))
	defer out.Free()
	e = json.Unmarshal(out.ReadBytes(), &r)
	if e == nil && r.Error != "" {
		e = errors.New(r.Error)
	}
	return r, e
}

//go:wasmexport describe
func describe() int32 {
	_ = pdk.OutputJSON(wire.Descriptor{ID: "fixture", Version: "1.0.0", ABI: 1, ConfigSchema: json.RawMessage(`{"type":"object"}`)})
	return 0
}

//go:wasmexport validate_config
func validateConfig() int32 { return 0 }

//go:wasmexport run
func run() int32 {
	var in wire.Input
	if e := pdk.InputJSON(&in); e != nil {
		pdk.SetError(e)
		return 1
	}
	var cfg struct {
		Files    map[string]string `json:"files"`
		Deletes  []string          `json:"deletes"`
		NoChange bool              `json:"no_change"`
		Fail     bool              `json:"fail"`
	}
	if e := json.Unmarshal(in.Config, &cfg); e != nil {
		pdk.SetError(e)
		return 1
	}
	if cfg.NoChange {
		_ = pdk.OutputJSON(wire.Result{Status: "no_change"})
		return 0
	}
	for path, content := range cfg.Files {
		r, e := call(wire.Request{Op: "create"})
		if e == nil {
			_, e = call(wire.Request{Op: "write", Handle: r.Handle, Data: []byte(content)})
		}
		if e == nil {
			_, e = call(wire.Request{Op: "close", Handle: r.Handle})
		}
		if e == nil {
			_, e = call(wire.Request{Op: "publish_file", Path: path, Handle: r.Handle})
		}
		if e != nil {
			pdk.SetError(e)
			return 1
		}
	}
	for _, path := range cfg.Deletes {
		if _, e := call(wire.Request{Op: "delete_file", Path: path}); e != nil {
			pdk.SetError(e)
			return 1
		}
	}
	if cfg.Fail {
		pdk.SetError(errors.New("fixture failure after candidate"))
		return 1
	}
	_ = pdk.OutputJSON(wire.Result{Status: "candidate", State: json.RawMessage(`{"cursor":"committed"}`), Metadata: json.RawMessage(`{"version":"fixture-v1"}`)})
	return 0
}
func main() {}
