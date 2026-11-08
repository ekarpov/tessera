package store

import (
	"context"
	"encoding/json"
	"errors"
	p "github.com/ekarpov/tessera/pkg/protocol"
)

// Mutation is a parsed write plus the identity of the original HTTP request.
// Digest must use the original method, request target and received body bytes.
type Mutation struct {
	Dataset, Key  string
	Digest        [32]byte
	Transaction   p.Transaction
	Single        bool
	SuccessStatus int
	// Represent records that the caller asked for the stored row back. It is
	// part of the mutation rather than of the request that carried it, because
	// a replay must reproduce the original body exactly — a receipt answering
	// with less than the caller was given the first time would be a different
	// response to the same key.
	Represent bool
}
type Reply struct {
	Status   int
	Body     json.RawMessage
	Replayed bool
}
type Receipt struct {
	Watermark      string          `json:"watermark,omitempty"`
	State          string          `json:"state"`
	Result         json.RawMessage `json:"result,omitempty"`
	Error          json.RawMessage `json:"error,omitempty"`
	Objects        []p.Reference   `json:"objects,omitempty"`
	ResultWithheld bool            `json:"result_withheld,omitempty"`
}

// Recovery is provided by durable engines. Receipt lookups authenticate their
// owner independently of current dataset grants. False means no retained claim.
type Recovery interface {
	RecoveryLimits() p.Limits
	Replay(context.Context, Access, string, string, [32]byte) (Reply, bool, error)
	Status(context.Context, Access, string, string) (Receipt, error)
	Mutate(context.Context, Access, Mutation) (Reply, error)
}

func OperationGrant(op p.Operation) Grants {
	switch op.Op {
	case "define_type", "update_type", "delete_type":
		return Grants{Schema: true}
	case "expect", "expect_absent", "expect_type":
		return Grants{Read: true}
	// An insert is satisfied by Write or by Insert. Which types an Insert
	// grant reaches is the evaluator's to check, per operation, since the
	// grant alone does not say.
	case "insert":
		return Grants{Insert: true}
	default:
		return Grants{Write: true}
	}
}
func AuthorizeTransaction(tx Tx, t p.Transaction) error {
	for i, op := range t.Operations {
		if err := tx.Authorize(OperationGrant(op)); err != nil {
			return p.AtOperation(err, i)
		}
	}
	return nil
}
func SuccessReply(result p.TransactionResult, single bool, status int) (Reply, error) {
	var body any = result
	if single {
		if len(result.Results) != 1 {
			return Reply{}, Refuse("internal_error")
		}
		raw, err := p.MarshalWire(result.Results[0])
		if err != nil {
			return Reply{}, err
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(raw, &fields); err != nil {
			return Reply{}, err
		}
		fields["watermark"], _ = json.Marshal(result.Watermark)
		body = fields
	}
	raw, err := p.MarshalWire(body)
	return Reply{Status: status, Body: raw}, err
}
func FailureReply(err error) Reply {
	var problem *p.Error
	if !errors.As(err, &problem) {
		problem = &p.Error{Code: "internal_error", Message: "Operation could not be completed"}
	}
	raw, _ := p.MarshalWire(map[string]any{"error": problem})
	return Reply{Status: p.ErrorStatus(problem.Code), Body: raw}
}
