package fileutil

import (
	"context"
	"io"
)

// ContextReader checks cancellation between file reads. Kernel I/O already in
// progress must return before cancellation can be observed.
type ContextReader struct {
	Context context.Context
	Reader  io.Reader
}

func (r ContextReader) Read(p []byte) (int, error) {
	if err := r.Context.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
