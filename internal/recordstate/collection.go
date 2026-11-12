package recordstate

import p "github.com/ekarpov/tessera/pkg/protocol"

// CollectionField resolves the inherited marker against the current schema.
func CollectionField(types map[string]p.Meta, typeID string) string {
	for range len(types) {
		meta, ok := types[typeID]
		if !ok {
			return ""
		}
		for _, field := range meta.Fields {
			if field.Collection {
				return field.ID
			}
		}
		if meta.ParentID == nil {
			return ""
		}
		typeID = *meta.ParentID
	}
	return ""
}

// CollectionID reads membership only from a marked field.
func CollectionID(types map[string]p.Meta, typeID string, fields map[string]p.Value) string {
	value := fields[CollectionField(types, typeID)]
	if value.Type() == p.TextType {
		return value.Payload().(string)
	}
	return ""
}
