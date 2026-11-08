package protocol

import "encoding/json"

type SortTerm struct {
	FieldID   string `json:"field_id"`
	Ascending bool   `json:"ascending"`
}
type TypeScope struct {
	Kind                   string   `json:"kind"`
	AllowedConcreteTypeIDs []string `json:"allowed_concrete_type_ids,omitempty"`
}
type Selection struct {
	Kind      string     `json:"kind"`
	Count     int        `json:"count,omitempty"`
	OrderedBy []SortTerm `json:"ordered_by,omitempty"`
}
type Query struct {
	TypeScope         *TypeScope      `json:"type_scope,omitempty"`
	Filter            json.RawMessage `json:"filter,omitempty"`
	Selection         *Selection      `json:"selection,omitempty"`
	ResultSort        []SortTerm      `json:"result_sort,omitempty"`
	Fields            []string        `json:"fields"`
	MaximumTextLength *int            `json:"maximum_text_length,omitempty"`
	IncludeVersions   bool            `json:"include_versions,omitempty"`
}
