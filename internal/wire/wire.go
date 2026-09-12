package wire

import "encoding/json"

const ABI = 1
const Chunk = 1 << 20

type Descriptor struct {
	ID           string          `json:"id"`
	Version      string          `json:"version"`
	ABI          int             `json:"abi"`
	Hosts        []string        `json:"hosts"`
	ConfigSchema json.RawMessage `json:"config_schema"`
}
type Input struct {
	ABI              int             `json:"abi"`
	Source           string          `json:"source"`
	Run              string          `json:"run"`
	Config           json.RawMessage `json:"config"`
	State            json.RawMessage `json:"state"`
	PreviousRevision string          `json:"previous_revision"`
}
type Result struct {
	Status   string          `json:"status"`
	Tags     []string        `json:"tags,omitempty"`
	State    json.RawMessage `json:"state,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}
type Entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Request struct {
	Op      string            `json:"op"`
	Path    string            `json:"path,omitempty"`
	Handle  string            `json:"handle,omitempty"`
	Data    []byte            `json:"data,omitempty"`
	Offset  int64             `json:"offset,omitempty"`
	Limit   int               `json:"limit,omitempty"`
	URL     string            `json:"url,omitempty"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}
type Response struct {
	Error   string            `json:"error,omitempty"`
	Handle  string            `json:"handle,omitempty"`
	Data    []byte            `json:"data,omitempty"`
	Size    int64             `json:"size,omitempty"`
	SHA256  string            `json:"sha256,omitempty"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Entries []Entry           `json:"entries,omitempty"`
}
type Caller interface {
	Call(Request) (Response, error)
}
