package conformance

import (
	"encoding/hex"
	"fmt"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

func checkCreatorAttribution(base, token string, call caller, problem problemCaller, nonce []byte) error {
	rootID := "test.creator.t" + hex.EncodeToString(nonce)
	childID := rootID + ".child"
	root := p.Meta{ID: rootID, Name: "Synthetic", Fields: []p.Field{
		{ID: "note", MetaID: rootID, Type: p.TextType},
	}}
	child := p.Meta{ID: childID, ParentID: &rootID, Name: "Synthetic"}
	for _, meta := range []p.Meta{root, child} {
		if err := call("POST", base+"/meta", token, map[string]any{"type": meta}, 201, nil); err != nil {
			return err
		}
	}
	objects := base + "/objects/" + childID
	insert := func() (string, error) {
		var result struct {
			Objects []p.Identity `json:"objects"`
		}
		if err := call("POST", objects, token, map[string]any{"fields": map[string]p.Value{}}, 201, &result); err != nil {
			return "", err
		}
		if len(result.Objects) != 1 {
			return "", fmt.Errorf("creator attribution insert did not return one identity")
		}
		return result.Objects[0].ID, nil
	}
	read := func(id string) (p.Value, error) {
		var result p.RecordSet
		if err := call("GET", objects+"/"+id, token, nil, 200, &result); err != nil {
			return p.Value{}, err
		}
		if len(result.Records) == 1 && len(result.Records[0].Values) == len(result.Meta.Fields) {
			for i, field := range result.Meta.Fields {
				if field.ID == "created_by" && field.Creator {
					return result.Records[0].Values[i], nil
				}
			}
		}
		return p.Value{}, fmt.Errorf("creator field missing from effective record metadata")
	}

	older, err := insert()
	if err != nil {
		return err
	}
	creator := p.Field{ID: "created_by", MetaID: rootID, Type: p.IntegerType, Creator: true}
	root.Fields = append(root.Fields, creator)
	metaPath := base + "/meta/" + rootID
	if err = problem("PUT", metaPath, token, map[string]any{"type": root}, 400, "meta_invalid_field"); err != nil {
		return err
	}
	root.Fields[1].Type = p.TextType
	if err = call("PUT", metaPath, token, map[string]any{"type": root}, 200, nil); err != nil {
		return err
	}
	if value, err := read(older); err != nil || value.Type() != p.NullType {
		return fmt.Errorf("adding a creator field must leave older records unattributed")
	}

	forged := map[string]any{"fields": map[string]p.Value{"created_by": {}}}
	if err = problem("POST", objects, token, forged, 400, "invalid_field"); err != nil {
		return err
	}
	id, err := insert()
	if err != nil {
		return err
	}
	value, err := read(id)
	if err != nil {
		return err
	}
	stamp, ok := value.Payload().(string)
	decoded, err := hex.DecodeString(stamp)
	if !ok || value.Type() != p.TextType || err != nil || len(stamp) != p.CreatorIDBytes || hex.EncodeToString(decoded) != stamp {
		return fmt.Errorf("creator attribution did not return an opaque creator identifier")
	}
	second, err := insert()
	if err != nil {
		return err
	}
	if other, err := read(second); err != nil || other.Payload() != stamp {
		return fmt.Errorf("creator identifier is unstable within a dataset")
	}
	filter := map[string]any{"field": map[string]any{"id": "created_by", "exact": value}}
	var projected p.RecordSet
	query := map[string]any{"query": map[string]any{
		"fields": []string{"created_by"}, "filter": filter,
		"result_sort": []map[string]any{{"field_id": "created_by", "ascending": true}},
	}}
	if err = call("POST", objects+":query", token, query, 200, &projected); err != nil {
		return err
	}
	if len(projected.Meta.Fields) != 1 || projected.Meta.Fields[0].ID != "created_by" ||
		!projected.Meta.Fields[0].Creator || len(projected.Records) != 2 {
		return fmt.Errorf("a creator projection did not select exactly the attributed records")
	}
	seen := map[string]bool{}
	for _, row := range projected.Records {
		if len(row.Values) != 1 || row.Values[0].Payload() != stamp || seen[row.ID.ID] {
			return fmt.Errorf("a projected row lost its creator or repeated an identity")
		}
		seen[row.ID.ID] = true
	}
	if !seen[id] || !seen[second] {
		return fmt.Errorf("creator equality selected the wrong records")
	}
	var grouped struct {
		Rows []aggregateRow `json:"aggregates"`
	}
	if err = call("POST", objects+":aggregate", token, map[string]any{"group_by": "created_by", "filter": filter}, 200, &grouped); err != nil {
		return err
	}
	if len(grouped.Rows) != 1 {
		return fmt.Errorf("grouping did not return exactly one creator")
	}
	if label, ok := grouped.Rows[0].text(); !ok || label != stamp || grouped.Rows[0].RowCount != 2 {
		return fmt.Errorf("grouping did not count the attributed records together")
	}
	if err = problem("PATCH", objects+"/"+older, token, forged, 400, "invalid_field"); err != nil {
		return err
	}
	if err = problem("PATCH", objects+"/"+id, token, map[string]any{"fields": map[string]p.Value{"created_by": value}}, 400, "invalid_field"); err != nil {
		return err
	}
	emptyUpdate := map[string]any{"operations": []any{map[string]any{
		"op": "update_where", "type": rootID, "filter": map[string]any{"object_ids": []string{}},
		"fields": map[string]p.Value{"created_by": value},
	}}}
	if err = problem("POST", base+":transact", token, emptyUpdate, 400, "invalid_field"); err != nil {
		return err
	}
	root.Fields[1].Creator = false
	if err = problem("PUT", metaPath, token, map[string]any{"type": root}, 400, "meta_schema_error"); err != nil {
		return err
	}
	root.Fields[0].Creator = true
	root.Fields[1].Creator = true
	if err = problem("PUT", metaPath, token, map[string]any{"type": root}, 400, "meta_schema_error"); err != nil {
		return err
	}
	return call("DELETE", metaPath+"?confirm="+rootID, token, nil, 200, nil)
}
