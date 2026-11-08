package store

import (
	"context"
	"encoding/json"
	p "github.com/ekarpov/tessera/pkg/protocol"
)

// Paging preserves bounded results and checks the current authorization before
// returning any page, including a replay.
type PageStart struct {
	Cursor    string `json:"cursor"`
	Watermark string `json:"watermark"`
}

type Paging interface {
	BeginPage(context.Context, Access, string, string, p.Query, int) (PageStart, error)
	NextPage(context.Context, Access, string, string) (json.RawMessage, error)
	EndPage(context.Context, Access, string, string) error
}
