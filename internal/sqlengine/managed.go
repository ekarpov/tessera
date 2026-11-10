package sqlengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const maxManagedLayouts = 128

type managedField struct {
	ID   string
	Kind p.Type
}

type managedLayout struct {
	TypeID string
	Fields []managedField
}

func (l managedLayout) identity() (string, []byte, error) {
	body, err := json.Marshal(l)
	if err != nil {
		return "", nil, err
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:16]), body, nil
}

type installedLayout struct {
	id string
	managedLayout
}

func (l installedLayout) table() string { return `"tessera_m_` + l.id + `"` }

func managedColumn(index int) string { return `"c` + strconv.Itoa(index) + `"` }

type managedMapping map[string][]installedLayout

type managedPlan struct {
	mapping managedMapping
	values  string
	source  string
}

type managedCache struct {
	mu      sync.Mutex
	entries map[string]managedPlan
}

func (c *managedCache) get(key string) (managedPlan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.entries[key]
	return value, ok
}

func (c *managedCache) put(key string, value managedPlan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= 128 || c.entries == nil {
		c.entries = make(map[string]managedPlan)
	}
	c.entries[key] = value
}

// InstallManagedTypes provisions server-owned tables for existing declarations
// before those types or their descendants have records. This is a host method,
// outside the v1 data API.
func (s *Store) InstallManagedTypes(ctx context.Context, a store.Access, dataset string, typeIDs []string) error {
	if len(typeIDs) == 0 || len(typeIDs) > maxManagedLayouts {
		return store.Refuse("invalid_field")
	}
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = s.lockManagedLayouts(ctx, tx); err != nil {
		return err
	}
	if err = s.serializeWrites(ctx, tx, dataset); err != nil {
		return err
	}
	h, err := s.load(ctx, tx, a, dataset, false)
	if err != nil {
		return err
	}
	if !h.auth.Grants.Schema || !h.auth.View.Full() {
		return store.Refuse("permission_denied")
	}
	var layouts []installedLayout
	seen := make(map[string]bool, len(typeIDs))
	for _, id := range typeIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		meta, ok := h.image.Types[id]
		if !ok {
			return store.Refuse("meta_type_not_found")
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT "layout_id" FROM "tessera_managed_installations" WHERE "dataset_id"=$1 AND "type_id"=$2`, dataset, id).Scan(&existing)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return dbError(ctx, err)
		}
		for candidate := range h.image.Types {
			if !managedDescends(h.image.Types, candidate, id) {
				continue
			}
			var populated bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "tessera_objects" WHERE "dataset_id"=$1 AND "type_id"=$2)`, dataset, candidate).Scan(&populated); err != nil {
				return dbError(ctx, err)
			}
			if populated {
				return store.Refuse("conflict")
			}
		}
		layout, err := declaredManagedLayout(meta)
		if err != nil {
			return err
		}
		layouts = append(layouts, layout)
	}
	if err = s.registerManagedLayouts(ctx, tx, layouts); err != nil {
		return err
	}
	if err = bindManagedLayouts(ctx, tx, dataset, layouts); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

func declaredManagedLayout(meta p.Meta) (installedLayout, error) {
	layout := managedLayout{TypeID: meta.ID, Fields: make([]managedField, 0, len(meta.Fields))}
	for _, field := range meta.Fields {
		layout.Fields = append(layout.Fields, managedField{field.ID, field.Type})
	}
	sort.Slice(layout.Fields, func(i, j int) bool { return layout.Fields[i].ID < layout.Fields[j].ID })
	key, _, err := layout.identity()
	return installedLayout{key, layout}, err
}

func (s *Store) lockManagedLayouts(ctx context.Context, tx *sql.Tx) error {
	if s.postgres {
		if _, err := tx.ExecContext(ctx, `LOCK TABLE "tessera_managed_layouts" IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return dbError(ctx, err)
		}
	}
	return nil
}

func (s *Store) registerManagedLayouts(ctx context.Context, tx *sql.Tx, layouts []installedLayout) error {
	if len(layouts) == 0 {
		return nil
	}
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_managed_layouts"`).Scan(&total); err != nil {
		return dbError(ctx, err)
	}
	before := total
	for _, layout := range layouts {
		var present bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "tessera_managed_layouts" WHERE "layout_id"=$1)`, layout.id).Scan(&present); err != nil {
			return dbError(ctx, err)
		}
		if present {
			continue
		}
		if total >= maxManagedLayouts {
			return store.Refuse("query_result_limit_exceeded")
		}
		_, body, err := layout.identity()
		if err != nil {
			return err
		}
		if err = s.createManagedTable(ctx, tx, layout); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_managed_layouts" VALUES ($1,$2)`, layout.id, body); err != nil {
			return dbError(ctx, err)
		}
		total++
	}
	if total != before {
		return s.refreshManagedViews(ctx, tx)
	}
	return nil
}

func bindManagedLayouts(ctx context.Context, tx *sql.Tx, dataset string, layouts []installedLayout) error {
	for _, layout := range layouts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "tessera_managed_installations" VALUES ($1,$2,$3)`, dataset, layout.TypeID, layout.id); err != nil {
			return dbError(ctx, err)
		}
	}
	if len(layouts) != 0 {
		return updateManagedKey(ctx, tx, dataset)
	}
	return nil
}

func managedDescends(types map[string]p.Meta, candidate, root string) bool {
	for current, ok := types[candidate]; ok; current, ok = types[*current.ParentID] {
		if current.ID == root {
			return true
		}
		if current.ParentID == nil {
			break
		}
	}
	return false
}

func updateManagedKey(ctx context.Context, tx *sql.Tx, dataset string) error {
	rows, err := tx.QueryContext(ctx, `SELECT "layout_id" FROM "tessera_managed_installations" WHERE "dataset_id"=$1 ORDER BY "layout_id"`, dataset)
	if err != nil {
		return dbError(ctx, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return dbError(ctx, err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return dbError(ctx, err)
	}
	var key any
	if len(ids) != 0 {
		sum := sha256.Sum256([]byte(strings.Join(ids, "")))
		key = hex.EncodeToString(sum[:])
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_dataset" SET "managed_key"=$1 WHERE "dataset_id"=$2`, key, dataset); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

func parseLayout(id string, body []byte) (installedLayout, error) {
	var layout managedLayout
	if err := json.Unmarshal(body, &layout); err != nil {
		return installedLayout{}, store.Refuse("invalid_field")
	}
	key, _, err := layout.identity()
	if err != nil || key != id || p.ValidateIdentifier(p.MetaIdentifier, layout.TypeID) != nil {
		return installedLayout{}, store.Refuse("invalid_field")
	}
	seen := map[string]bool{}
	for _, field := range layout.Fields {
		if !field.Kind.ValidFieldType() || p.ValidateIdentifier(p.FieldIdentifier, field.ID) != nil || seen[field.ID] {
			return installedLayout{}, store.Refuse("invalid_field")
		}
		seen[field.ID] = true
	}
	return installedLayout{id, layout}, nil
}

func (s *Store) loadManaged(ctx context.Context, tx *sql.Tx, dataset, layoutKey, schemaKey string, types map[string]p.Meta) (managedPlan, error) {
	if layoutKey == "" {
		return s.readPlan(nil, nil), nil
	}
	key := layoutKey + ":" + schemaKey
	if cached, ok := s.managed.get(key); ok {
		return cached, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT i."type_id", l."layout_id", l."definition"
        FROM "tessera_managed_installations" i JOIN "tessera_managed_layouts" l ON l."layout_id"=i."layout_id"
        WHERE i."dataset_id"=$1`, dataset)
	if err != nil {
		return managedPlan{}, dbError(ctx, err)
	}
	defer rows.Close()
	installed := map[string]installedLayout{}
	for rows.Next() {
		var typeID, id string
		var body []byte
		if err = rows.Scan(&typeID, &id, &body); err != nil {
			return managedPlan{}, dbError(ctx, err)
		}
		layout, err := parseLayout(id, body)
		if err != nil || layout.TypeID != typeID {
			return managedPlan{}, store.Refuse("internal_error")
		}
		installed[typeID] = layout
	}
	if err = rows.Err(); err != nil {
		return managedPlan{}, dbError(ctx, err)
	}
	mapping, err := mapManagedTypes(types, installed)
	if err != nil {
		return managedPlan{}, err
	}
	var layouts []installedLayout
	for _, layout := range installed {
		layouts = append(layouts, layout)
	}
	sort.Slice(layouts, func(i, j int) bool { return layouts[i].id < layouts[j].id })
	plan := s.readPlan(mapping, layouts)
	s.managed.put(key, plan)
	return plan, nil
}

func mapManagedTypes(types map[string]p.Meta, installed map[string]installedLayout) (managedMapping, error) {
	mapping := managedMapping{}
	for typeID, meta := range types {
		current := meta
		for depth := 0; ; depth++ {
			if depth >= len(types) {
				return nil, store.Refuse("invalid_field")
			}
			if layout, ok := installed[current.ID]; ok {
				mapping[typeID] = append(mapping[typeID], layout)
			}
			if current.ParentID == nil {
				break
			}
			parent, ok := types[*current.ParentID]
			if !ok {
				return nil, store.Refuse("invalid_field")
			}
			current = parent
		}
	}
	return mapping, nil
}

func (m managedMapping) withTypes(types map[string]p.Meta) (managedMapping, error) {
	installed := map[string]installedLayout{}
	for _, layouts := range m {
		for _, layout := range layouts {
			installed[layout.TypeID] = layout
		}
	}
	return mapManagedTypes(types, installed)
}

func (s *Store) managedColumnType(kind p.Type) string {
	switch kind {
	case p.IntegerType, p.FloatType, p.BooleanType:
		return "BIGINT"
	case p.DateTimeType:
		if s.postgres {
			return "TIMESTAMPTZ"
		}
	}
	if s.postgres {
		return "BYTEA"
	}
	return "BLOB"
}

func (s *Store) createManagedTable(ctx context.Context, tx *sql.Tx, layout installedLayout) error {
	columns := []string{`"dataset_id" TEXT NOT NULL`, `"object_id" TEXT NOT NULL`}
	for i, field := range layout.Fields {
		columns = append(columns, managedColumn(i)+" "+s.managedColumnType(field.Kind))
	}
	columns = append(columns, `PRIMARY KEY ("dataset_id","object_id")`,
		`FOREIGN KEY ("dataset_id","object_id") REFERENCES "tessera_objects" ON DELETE CASCADE`)
	if _, err := tx.ExecContext(ctx, "CREATE TABLE "+layout.table()+" ("+strings.Join(columns, ",")+")"); err != nil {
		return dbError(ctx, err)
	}
	for i, field := range layout.Fields {
		if field.Kind != p.IntegerType && field.Kind != p.DateTimeType {
			continue
		}
		name := fmt.Sprintf(`"tessera_m_%s_%d"`, layout.id, i)
		column := managedColumn(i)
		index := ` ("dataset_id",` + column + `,"object_id") WHERE ` + column + " IS NOT NULL"
		if field.Kind == p.DateTimeType {
			identity := `"object_id"`
			if s.postgres {
				identity += ` COLLATE "C"`
				column += " DESC NULLS LAST"
			} else {
				column += " DESC"
			}
			index = ` ("dataset_id",` + column + "," + identity + ")"
		}
		if _, err := tx.ExecContext(ctx, "CREATE INDEX "+name+" ON "+layout.table()+index); err != nil {
			return dbError(ctx, err)
		}
	}
	return nil
}

func managedLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func (s *Store) listManagedLayouts(ctx context.Context, tx *sql.Tx) ([]installedLayout, error) {
	rows, err := tx.QueryContext(ctx, `SELECT "layout_id","definition" FROM "tessera_managed_layouts" ORDER BY "layout_id"`)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer rows.Close()
	var layouts []installedLayout
	for rows.Next() {
		var id string
		var body []byte
		if err = rows.Scan(&id, &body); err != nil {
			return nil, dbError(ctx, err)
		}
		layout, err := parseLayout(id, body)
		if err != nil {
			return nil, err
		}
		layouts = append(layouts, layout)
	}
	if err = rows.Err(); err != nil {
		return nil, dbError(ctx, err)
	}
	return layouts, nil
}

// PostgreSQL represents RFC3339 year 0000 as 1 BC. Convert it back without
// collapsing it onto year 0001, including in an ordered comparison.
func managedTimestampText(column string) string {
	utc := "(" + column + " AT TIME ZONE 'UTC')"
	return `(CASE WHEN extract(year FROM ` + utc + `)<0
        THEN '0000'||to_char(` + utc + `,'-MM-DD"T"HH24:MI:SS.MS"Z"')
        ELSE to_char(` + utc + `,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') END)`
}

// Each managed table contributes one branch regardless of its field count.
func (s *Store) managedBranch(layout installedLayout) string {
	var ids, numbers, data []string
	for i, field := range layout.Fields {
		column := "m." + managedColumn(i)
		key := managedLiteral(field.ID)
		ids = append(ids, "("+key+","+managedLiteral(string(field.Kind))+")")
		switch field.Kind {
		case p.IntegerType, p.FloatType, p.BooleanType:
			numbers = append(numbers, "WHEN "+key+" THEN "+column)
		default:
			if s.postgres && field.Kind == p.DateTimeType {
				column = `convert_to(` + managedTimestampText(column) + `,'UTF8')`
			}
			data = append(data, "WHEN "+key+" THEN "+column)
		}
	}
	dataType := "BLOB"
	if s.postgres {
		dataType = "BYTEA"
	}
	expression := func(arms []string, kind string) string {
		if len(arms) == 0 {
			return "CAST(NULL AS " + kind + ")"
		}
		return `CASE f."field_id" ` + strings.Join(arms, " ") + " END"
	}
	fields := `(SELECT "column1" AS "field_id", "column2" AS "kind" FROM (VALUES ` + strings.Join(ids, ",") + ")) AS f"
	if s.postgres {
		fields = `(VALUES ` + strings.Join(ids, ",") + `) AS f("field_id","kind")`
	}
	number := expression(numbers, "BIGINT")
	value := expression(data, dataType)
	return `SELECT m."dataset_id",m."object_id",f."field_id",f."kind",` + number + "," + value +
		" FROM " + layout.table() + " AS m CROSS JOIN " + fields + " WHERE " + number + " IS NOT NULL OR " + value + " IS NOT NULL"
}

func (s *Store) refreshManagedViews(ctx context.Context, tx *sql.Tx) error {
	layouts, err := s.listManagedLayouts(ctx, tx)
	if err != nil {
		return err
	}
	for _, name := range []string{`"tessera_read_objects"`, `"tessera_all_field_values"`} {
		if _, err = tx.ExecContext(ctx, "DROP VIEW IF EXISTS "+name); err != nil {
			return dbError(ctx, err)
		}
	}
	if _, err = tx.ExecContext(ctx, `CREATE VIEW "tessera_all_field_values" AS `+s.valueUnion(layouts)); err != nil {
		return dbError(ctx, err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE VIEW "tessera_read_objects" AS `+s.objectProjection(`"tessera_all_field_values"`)); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

func (s *Store) valueUnion(layouts []installedLayout) string {
	branches := []string{`SELECT "dataset_id","object_id","field_id","kind","number","data" FROM "tessera_field_values"`}
	for _, layout := range layouts {
		if len(layout.Fields) != 0 {
			branches = append(branches, s.managedBranch(layout))
		}
	}
	return strings.Join(branches, " UNION ALL ")
}

func (s *Store) readPlan(mapping managedMapping, layouts []installedLayout) managedPlan {
	values := `"tessera_field_values"`
	if len(layouts) != 0 {
		values = "(" + s.valueUnion(layouts) + ")"
	}
	return managedPlan{mapping: mapping, values: values, source: "(" + s.objectProjection(values) + ")"}
}

func (s *Store) objectProjection(values string) string {
	pack := `CAST((SELECT json_group_array(json_object('field',v."field_id",'kind',v."kind",
        'number',v."number",'data',hex(v."data"))) FROM ` + values + ` AS v
        WHERE v."dataset_id"=o."dataset_id" AND v."object_id"=o."object_id") AS BLOB)`
	if s.postgres {
		pack = `convert_to(coalesce((SELECT json_agg(json_build_object('field',v."field_id",'kind',v."kind",
            'number',v."number",'data',encode(v."data",'hex')))::text FROM ` + values + ` AS v
            WHERE v."dataset_id"=o."dataset_id" AND v."object_id"=o."object_id"),'[]'),'UTF8')`
	}
	return `SELECT o."dataset_id",o."object_id",o."type_id",` + pack +
		` AS "fields",o."wire_bytes" FROM "tessera_objects" o`
}

func (s *Store) persistManagedRecord(ctx context.Context, tx *sql.Tx, dataset, id string, layouts []installedLayout, previous, fields map[string]p.Value, exists bool) error {
	if len(layouts) == 0 {
		return persistTypedFields(ctx, tx, dataset, id, previous, fields)
	}
	oldCustom, newCustom := map[string]p.Value{}, map[string]p.Value{}
	for name, value := range previous {
		oldCustom[name] = value
	}
	for name, value := range fields {
		newCustom[name] = value
	}
	for _, layout := range layouts {
		args := []any{dataset, id}
		columns := []string{`"dataset_id"`, `"object_id"`}
		marks := []string{"$1", "$2"}
		var updates []string
		changed := !exists
		for i, field := range layout.Fields {
			old := previous[field.ID]
			value := fields[field.ID]
			if old.Type() == field.Kind {
				delete(oldCustom, field.ID)
			} else {
				old = p.Value{}
			}
			if value.Type() == field.Kind {
				delete(newCustom, field.ID)
			} else {
				value = p.Value{}
			}
			oldNumber, oldData := fieldParts(old)
			number, data := fieldParts(value)
			changed = changed || old.Type() != value.Type() || oldNumber != number || !bytes.Equal(oldData, data)
			var arg any = number
			if data != nil {
				arg = data
				if s.postgres && field.Kind == p.DateTimeType {
					stamp, err := time.Parse(time.RFC3339Nano, string(data))
					if err != nil {
						return store.Refuse("internal_error")
					}
					arg = stamp
				}
			}
			column := managedColumn(i)
			columns = append(columns, column)
			args = append(args, arg)
			marks = append(marks, "$"+strconv.Itoa(len(args)))
			updates = append(updates, column+"=excluded."+column)
		}
		if !changed {
			continue
		}
		conflict := "DO NOTHING"
		if len(updates) != 0 {
			conflict = "DO UPDATE SET " + strings.Join(updates, ",")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+layout.table()+" ("+strings.Join(columns, ",")+") VALUES ("+
			strings.Join(marks, ",")+`) ON CONFLICT ("dataset_id","object_id") `+conflict, args...); err != nil {
			return dbError(ctx, err)
		}
	}
	return persistTypedFields(ctx, tx, dataset, id, oldCustom, newCustom)
}

// Resolve a field to a single physical column only when all concrete types
// in the authorized query scope use the same binding.
func (m managedMapping) field(types []string, id string, kind p.Type) (installedLayout, string, bool) {
	var resolved installedLayout
	var column string
	for _, typeID := range types {
		var match installedLayout
		var found string
		for _, layout := range m[typeID] {
			for i, field := range layout.Fields {
				if field.ID == id && field.Kind == kind {
					match, found = layout, managedColumn(i)
				}
			}
		}
		if found == "" || (column != "" && (column != found || resolved.id != match.id)) {
			return installedLayout{}, "", false
		}
		resolved, column = match, found
	}
	return resolved, column, column != ""
}

// Archives flatten managed columns to typed rows. Rebuild the registered
// tables and split their values back out before the restore is committed.
func (s *Store) restoreManagedRows(ctx context.Context, tx *sql.Tx) error {
	layouts, err := s.listManagedLayouts(ctx, tx)
	if err != nil || len(layouts) == 0 {
		return err
	}
	if len(layouts) > maxManagedLayouts {
		return store.Refuse("invalid_field")
	}
	for _, layout := range layouts {
		if err = s.createManagedTable(ctx, tx, layout); err != nil {
			return err
		}
	}
	// The empty destination's read view still sees only shared values. Process
	// one batch at a time and publish the managed branches after moving all rows.
	datasets, err := tx.QueryContext(ctx, `SELECT DISTINCT "dataset_id" FROM "tessera_managed_installations"`)
	if err != nil {
		return dbError(ctx, err)
	}
	var ids []string
	for datasets.Next() {
		var id string
		if err = datasets.Scan(&id); err != nil {
			datasets.Close()
			return dbError(ctx, err)
		}
		ids = append(ids, id)
	}
	err = datasets.Err()
	datasets.Close()
	if err != nil {
		return dbError(ctx, err)
	}
	for _, dataset := range ids {
		types, err := restoreDeclaredTypes(ctx, tx, dataset)
		if err != nil {
			return err
		}
		if err = updateManagedKey(ctx, tx, dataset); err != nil {
			return err
		}
		var key string
		if err = tx.QueryRowContext(ctx, `SELECT "managed_key" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&key); err != nil {
			return dbError(ctx, err)
		}
		mapping, err := s.loadManaged(ctx, tx, dataset, key, dataset+":restore", types)
		if err != nil {
			return err
		}
		if err = s.restoreManagedDataset(ctx, tx, dataset, mapping.mapping); err != nil {
			return err
		}
	}
	return s.refreshManagedViews(ctx, tx)
}

func restoreDeclaredTypes(ctx context.Context, tx *sql.Tx, dataset string) (map[string]p.Meta, error) {
	rows, err := tx.QueryContext(ctx, `SELECT "type_id","body" FROM "tessera_types" WHERE "dataset_id"=$1`, dataset)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer rows.Close()
	types := map[string]p.Meta{}
	for rows.Next() {
		var id string
		var body []byte
		var meta p.Meta
		if err = rows.Scan(&id, &body); err != nil {
			return nil, dbError(ctx, err)
		}
		if json.Unmarshal(body, &meta) != nil {
			return nil, store.Refuse("invalid_field")
		}
		types[id] = meta
	}
	if err = rows.Err(); err != nil {
		return nil, dbError(ctx, err)
	}
	return types, nil
}

func (s *Store) restoreManagedDataset(ctx context.Context, tx *sql.Tx, dataset string, mapping managedMapping) error {
	type moved struct {
		id  string
		row recordstate.Row
	}
	order := `"object_id"`
	if s.postgres {
		order += ` COLLATE "C"`
	}
	last := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT "object_id","type_id","fields" FROM "tessera_read_objects"
            WHERE "dataset_id"=$1 AND `+order+`>$2 ORDER BY `+order+` LIMIT 256`, dataset, last)
		if err != nil {
			return dbError(ctx, err)
		}
		var batch []moved
		for rows.Next() {
			var move moved
			var raw []byte
			if err = rows.Scan(&move.id, &move.row.TypeID, &raw); err != nil {
				rows.Close()
				return dbError(ctx, err)
			}
			if err = decodeStoredFields(raw, &move.row); err != nil {
				rows.Close()
				return store.Refuse("invalid_field")
			}
			batch = append(batch, move)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return dbError(ctx, err)
		}
		if len(batch) == 0 {
			return nil
		}
		for _, move := range batch {
			last = move.id
			layouts := mapping[move.row.TypeID]
			if len(layouts) == 0 {
				continue
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM "tessera_field_values" WHERE "dataset_id"=$1 AND "object_id"=$2`, dataset, move.id); err != nil {
				return dbError(ctx, err)
			}
			if err = s.persistManagedRecord(ctx, tx, dataset, move.id, layouts, nil, move.row.Fields, false); err != nil {
				return err
			}
		}
	}
}
