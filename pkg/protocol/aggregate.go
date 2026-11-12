package protocol

import (
	"bytes"
	"encoding/json"
)

// OptionalReadCapabilities are the optional read features a composition may
// advertise. Required operations are never toggles: only these may be withheld,
// and withholding one means refusing it, never answering it approximately.
var OptionalReadCapabilities = []string{}

// RetiredReadCapabilities are names that were once advertised and are now
// required operations. A deployment naming one is refused rather than humoured:
// it is asking to switch off something this protocol no longer lets it decline,
// and starting anyway would leave the configuration saying one thing and the
// server doing another.
var RetiredReadCapabilities = []string{"aggregation", "distinct_references", "text_matching"}

// CreatorAttributionCapability enables server-populated creator fields.
const CreatorAttributionCapability = "creator_attribution"

const CollectionAccessCapability = "collection_access"

// CreatorIDBytes is the encoded size of a dataset-scoped creator ID.
const CreatorIDBytes = 32

// DatasetCapabilities are every capability name a server may advertise for a dataset.
//
// It exists so there is one list rather than one per surface. The
// administration page kept a copy of its own, the three retired names stayed
// in that copy when they became required operations, and the page went on
// telling an operator this deployment withheld three things the protocol no
// longer lets anyone decline.
func DatasetCapabilities() []string {
	names := make([]string, 0, len(OptionalReadCapabilities)+4)
	names = append(names, OptionalReadCapabilities...)
	return append(names, "content", "sharing", CreatorAttributionCapability, CollectionAccessCapability)
}

// DatasetCreationCapability says that the creation route exists on this
// server. A client MUST NOT read it as permission: it reports that the route
// is here, not that this caller may use it.
const DatasetCreationCapability = "dataset_creation"

// ServerCapabilities are the capability names that describe the server rather
// than a dataset. Each is advertised on its own condition, so this is the
// vocabulary rather than a list to emit: what it fixes is that a name exists
// in one place, and that the specification and the code can be compared.
var ServerCapabilities = []string{DatasetCreationCapability, SharedReadsCapability}

// SharedReadsCapability says that an authorization request here may ask to
// read everything shared with the account instead of naming a dataset.
const SharedReadsCapability = "shared_reads"

// Aggregate is one row of an aggregation answer.
//
// Sum, Minimum and Maximum are raw JSON numbers rather than float64 so that an
// integer statistic keeps the spelling the engine computed. A nil member is the
// specified null, which is not the same answer as zero: it means no row carried
// a value, and a client that reads it as zero reports an average of nothing.
type Aggregate struct {
	// Grouped distinguishes an ungrouped answer, which omits the member
	// entirely, from the group of rows whose field is null, which reports it
	// as null and counts toward the group limit.
	Grouped bool
	Group   *string
	// GroupReference carries the group of a reference-valued grouping, as the
	// bare qualified identity distinct references already returns. At most one
	// of Group and GroupReference is set; both nil in a grouped answer is the
	// null group, which is a group like any other.
	//
	// One representation of a reference rather than two, because a client that
	// has to decode an identity differently depending on which read produced
	// it is a client with two decoders and one of them under-tested.
	GroupReference *Reference
	RowCount       int
	ValueCount     int
	Sum            json.RawMessage
	Minimum        json.RawMessage
	Maximum        json.RawMessage
}

func (a Aggregate) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	if a.Grouped {
		out.WriteString(`"group":`)
		var group any
		switch {
		case a.Group != nil:
			group = *a.Group
		case a.GroupReference != nil:
			group = *a.GroupReference
		}
		encoded, err := json.Marshal(group)
		if err != nil {
			return nil, err
		}
		out.Write(encoded)
		out.WriteByte(',')
	}
	counts, err := json.Marshal(map[string]int{"row_count": a.RowCount, "value_count": a.ValueCount})
	if err != nil {
		return nil, err
	}
	out.Write(counts[1 : len(counts)-1])
	for _, member := range []struct {
		name string
		raw  json.RawMessage
	}{{"sum", a.Sum}, {"minimum", a.Minimum}, {"maximum", a.Maximum}} {
		out.WriteString(`,"` + member.name + `":`)
		if len(member.raw) == 0 {
			out.WriteString("null")
			continue
		}
		out.Write(member.raw)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
