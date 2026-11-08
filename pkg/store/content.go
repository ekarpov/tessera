package store

import (
	"context"
	"encoding/json"
	"io"
)

type Content interface {
	ContentControl(context.Context, Access, string, string, string, json.RawMessage) (any, error)
	ContentTransfer(context.Context, string, string, string, io.Reader) (ContentRead, error)
}
type ContentRead struct {
	Body        io.ReadCloser
	Size        int64
	ContentType string
}
