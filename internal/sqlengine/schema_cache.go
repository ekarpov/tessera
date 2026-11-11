package sqlengine

import (
	"container/list"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"sync"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// Declared metadata is immutable once published. The evaluator owns its
// type map and replaces declarations on writes. EffectiveMetadata copies
// nested values before exposing them to callers.
type cachedSchema struct {
	types map[string]p.Meta
	bytes int64
}

type schemaEntry struct {
	key string
	cachedSchema
}

// Cache by content so datasets provisioned from the same template share
// one decoded snapshot. Both the entry count and serialized size are bounded.
type schemaCache struct {
	mu      sync.Mutex
	order   list.List
	entries map[string]*list.Element
	bytes   int64
}

const (
	maxCachedSchemas     = 128
	maxCachedSchemaBytes = 8 << 20
)

func (c *schemaCache) get(key string) (cachedSchema, bool) {
	if key == "" {
		return cachedSchema{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element := c.entries[key]
	if element == nil {
		return cachedSchema{}, false
	}
	c.order.MoveToFront(element)
	return element.Value.(schemaEntry).cachedSchema, true
}

func (c *schemaCache) put(key string, schema cachedSchema) {
	if key == "" || schema.bytes > maxCachedSchemaBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil {
		c.order.MoveToFront(element)
		return
	}
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	c.entries[key] = c.order.PushFront(schemaEntry{key: key, cachedSchema: schema})
	c.bytes += schema.bytes
	for len(c.entries) > maxCachedSchemas || c.bytes > maxCachedSchemaBytes {
		last := c.order.Back()
		entry := last.Value.(schemaEntry)
		delete(c.entries, entry.key)
		c.bytes -= entry.bytes
		c.order.Remove(last)
	}
}

// Fingerprint the declarations actually committed by a schema edit. Record
// writes skip this pass, and the ordering is stable within the database.
func schemaCacheKey(ctx context.Context, tx *sql.Tx, dataset string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT "type_id","body" FROM "tessera_types" WHERE "dataset_id"=$1 ORDER BY "type_id"`, dataset)
	if err != nil {
		return "", dbError(ctx, err)
	}
	defer rows.Close()
	hash := sha256.New()
	var size [8]byte
	write := func(value []byte) {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		hash.Write(size[:])
		hash.Write(value)
	}
	for rows.Next() {
		var id string
		var body []byte
		if err := rows.Scan(&id, &body); err != nil {
			return "", dbError(ctx, err)
		}
		write([]byte(id))
		write(body)
	}
	if err = rows.Err(); err != nil {
		return "", dbError(ctx, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
