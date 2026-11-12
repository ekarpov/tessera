package protocol

// AttachmentTarget binds an import to an exact collection insert or one
// existing record. An empty ObjectID names a collection insert.
type AttachmentTarget struct {
	TypeID       string `json:"type_id"`
	CollectionID string `json:"collection_id"`
	ObjectID     string `json:"object_id,omitempty"`
}

func (t AttachmentTarget) Validate() error {
	if err := ValidateIdentifier(MetaIdentifier, t.TypeID); err != nil {
		return err
	}
	if err := ValidateIdentifier(ObjectIdentifier, t.CollectionID); err != nil {
		return err
	}
	if t.ObjectID != "" {
		return ValidateIdentifier(ObjectIdentifier, t.ObjectID)
	}
	return nil
}
