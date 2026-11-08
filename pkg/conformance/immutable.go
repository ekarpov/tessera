package conformance

import (
	"encoding/hex"
	"fmt"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// An immutable field keeps the value it was first given. Required of every
// server, because the guarantee is about writers the client does not control:
// a client can check its own writes and cannot check anybody else's.
//
// Each step is a way a server could look conformant and not be. Refusing every
// update to the field fails the first value arriving by update and the
// restatement; treating null as written fails the first; accepting a clear
// lets a value be removed and replaced in two steps; and a subtype that loses
// the flag is a door around it, since an update through the subtype reaches
// the same stored value.
func checkImmutable(base, token string, call caller, nonce []byte) error {
	suffix := hex.EncodeToString(nonce)
	rootID, childID := "test.immutable.t"+suffix, "test.immutable.t"+suffix+".child"
	root := p.Meta{ID: rootID, Name: "Immutable", Fields: []p.Field{
		{ID: "given", MetaID: rootID, Name: "Given", Type: p.IntegerType, Immutable: true},
		{ID: "note", MetaID: rootID, Name: "Note", Type: p.TextType},
	}}
	child := p.Meta{ID: childID, ParentID: &rootID, Name: "Immutable child", Fields: []p.Field{}}
	for _, meta := range []p.Meta{root, child} {
		if err := call("POST", base+"/meta", token, map[string]any{"type": meta}, 201, nil); err != nil {
			return fmt.Errorf("a type declaring an immutable field was refused: %w", err)
		}
	}
	// The flag is returned, and returned for the subtype too: a client
	// deciding whether to trust it reads it from the effective schema.
	for _, id := range []string{rootID, childID} {
		var read struct {
			Type p.Meta `json:"type"`
		}
		if err := call("GET", base+"/meta/"+id, token, nil, 200, &read); err != nil {
			return err
		}
		flagged := false
		for _, f := range read.Type.Fields {
			if f.ID == "given" {
				flagged = f.Immutable
			}
		}
		if !flagged {
			return fmt.Errorf("the effective schema of %s does not report the field immutable", id)
		}
	}
	integer := func(n int64) p.Value {
		v, _ := p.NewValue(p.IntegerType, n)
		return v
	}
	insert := func(typeID string, fields map[string]p.Value) (string, error) {
		var inserted struct {
			Objects []p.Identity `json:"objects"`
		}
		if err := call("POST", base+"/objects/"+typeID, token, map[string]any{"fields": fields}, 201, &inserted); err != nil {
			return "", err
		}
		if len(inserted.Objects) != 1 {
			return "", fmt.Errorf("an insert returned no identity")
		}
		return inserted.Objects[0].ID, nil
	}
	patch := func(typeID, id string, fields map[string]p.Value) error {
		return call("PATCH", base+"/objects/"+typeID+"/"+id, token, map[string]any{"fields": fields}, 200, nil)
	}
	refused := func(typeID, id string, fields map[string]p.Value) error {
		var envelope struct {
			Error p.Error `json:"error"`
		}
		if err := call("PATCH", base+"/objects/"+typeID+"/"+id, token, map[string]any{"fields": fields}, 409, &envelope); err != nil {
			return err
		}
		named, _ := envelope.Error.Details["fields"].([]any)
		if envelope.Error.Code != "conflict" || envelope.Error.Details["reason"] != "immutable_field" || len(named) != 1 || named[0] != "given" {
			return fmt.Errorf("the refusal is not conflict/immutable_field naming the field")
		}
		return nil
	}

	written, err := insert(rootID, map[string]p.Value{"given": integer(5)})
	if err != nil {
		return err
	}
	if err = patch(rootID, written, map[string]p.Value{"given": integer(5), "note": {}}); err != nil {
		return fmt.Errorf("restating an immutable field's value was refused: %w", err)
	}
	if err = refused(rootID, written, map[string]p.Value{"given": integer(6)}); err != nil {
		return fmt.Errorf("changing an immutable value: %w", err)
	}
	if err = refused(rootID, written, map[string]p.Value{"given": {}}); err != nil {
		return fmt.Errorf("clearing an immutable value: %w", err)
	}

	empty, err := insert(rootID, map[string]p.Value{})
	if err != nil {
		return err
	}
	if err = patch(rootID, empty, map[string]p.Value{"given": integer(1)}); err != nil {
		return fmt.Errorf("the first value of an immutable field was refused by update: %w", err)
	}
	if err = refused(rootID, empty, map[string]p.Value{"given": integer(2)}); err != nil {
		return fmt.Errorf("changing an immutable value first written by update: %w", err)
	}

	inherited, err := insert(childID, map[string]p.Value{"given": integer(7)})
	if err != nil {
		return err
	}
	if err = refused(childID, inherited, map[string]p.Value{"given": integer(8)}); err != nil {
		return fmt.Errorf("changing an inherited immutable value: %w", err)
	}
	// It constrains update, never deletion.
	if err = call("DELETE", base+"/objects/"+rootID+"/"+written, token, nil, 200, nil); err != nil {
		return fmt.Errorf("a record holding an immutable value could not be deleted: %w", err)
	}
	cleanup := map[string]any{"operations": []any{map[string]any{"op": "delete_type", "type": rootID, "cascade": true}}}
	return call("POST", base+":transact", token, cleanup, 200, nil)
}
