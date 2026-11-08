package protocol

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type IdentifierKind string

const (
	DatasetIdentifier IdentifierKind = "dataset"
	MetaIdentifier    IdentifierKind = "meta"
	FieldIdentifier   IdentifierKind = "field"
	ObjectIdentifier  IdentifierKind = "object"
	ContentIdentifier IdentifierKind = "content"
	AliasIdentifier   IdentifierKind = "alias"
)

var (
	aliasID   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	datasetID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	metaID    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	fieldID   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reserved  = strings.Fields(`add all alter and as autoincrement between case check collate commit constraint create default
deferrable delete distinct drop else escape except exists foreign from group having if in index
insert intersect into is isnull join limit not nothing notnull null on or order primary references
returning select set table then to transaction union unique update using values when where`)
)

func ValidateIdentifier(kind IdentifierKind, id string) error {
	valid := utf8.ValidString(id) && len(id) > 0 && len(id) <= 255
	if !valid {
		return invalid("invalid_identifier")
	}
	switch kind {
	case AliasIdentifier:
		valid = aliasID.MatchString(id)
	case DatasetIdentifier:
		valid = datasetID.MatchString(id)
	case MetaIdentifier:
		valid = metaID.MatchString(id)
	case FieldIdentifier:
		valid = fieldID.MatchString(id) && id != "id" && id != "meta_id"
	case ObjectIdentifier, ContentIdentifier:
		valid = !strings.Contains(id, "//")
		for _, r := range id {
			if unicode.IsControl(r) {
				valid = false
				break
			}
		}
	default:
		valid = false
	}
	if kind == MetaIdentifier || kind == FieldIdentifier {
		for _, word := range reserved {
			if strings.EqualFold(id, word) {
				valid = false
				break
			}
		}
	}
	if !valid {
		return invalid("invalid_identifier")
	}
	return nil
}
