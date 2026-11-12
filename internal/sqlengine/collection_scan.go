package sqlengine

import (
	"slices"
	"strconv"
	"strings"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

type collectionScan struct {
	view  store.View
	types map[string]p.Meta
}

// condition narrows the stream before fields are packed or rows are counted.
// The evaluator checks the same scope again using the decoded record.
func (c collectionScan) condition(types []string, values string, args *[]any) string {
	parameter := func(value any) string {
		*args = append(*args, value)
		return "$" + strconv.Itoa(len(*args))
	}
	var terms []string
	for _, id := range types {
		if c.view.AllowsType(id) {
			terms = append(terms, `o."type_id"=`+parameter(id))
		}
	}
	for _, scope := range c.view.ReadCollections() {
		if c.view.AllowsType(scope.TypeID) || !slices.Contains(types, scope.TypeID) {
			continue
		}
		field := recordstate.CollectionField(c.types, scope.TypeID)
		if field == "" {
			continue
		}
		typ, key, value := parameter(scope.TypeID), parameter(field), parameter([]byte(scope.ID))
		terms = append(terms, `(o."type_id"=`+typ+` AND EXISTS (SELECT 1 FROM `+values+` AS m
			WHERE m."dataset_id"=o."dataset_id" AND m."object_id"=o."object_id"
			AND m."kind"='text' AND m."field_id"=`+key+` AND m."data"=`+value+"))")
	}
	if len(terms) == 0 {
		return " AND FALSE"
	}
	return " AND (" + strings.Join(terms, " OR ") + ")"
}
