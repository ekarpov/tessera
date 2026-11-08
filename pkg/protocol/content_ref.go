package protocol

import "strings"

// ParseContentReference accepts only a qualified store//item reference. A
// caller resolves the named store, never the dataset's current replacement.
func ParseContentReference(value string) (storeID, itemID string, err error) {
	storeID, itemID, ok := strings.Cut(value, "//")
	if !ok || ValidateIdentifier(DatasetIdentifier, storeID) != nil || ValidateIdentifier(ContentIdentifier, itemID) != nil {
		return "", "", invalid("invalid_field_value")
	}
	return storeID, itemID, nil
}
