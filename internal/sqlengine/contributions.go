package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const maxContributionBatch = 256

// Contributions lists live, attributed records in one dataset for a trusted
// host. A returned position is the exclusive object ID for the next page.
func (s *Store) Contributions(ctx context.Context, dataset, principal, after string, limit int) ([]store.ObjectKey, string, error) {
	if principal == "" || limit < 1 || limit > maxContributionBatch {
		return nil, "", store.Refuse("invalid_field")
	}
	if after != "" {
		if err := p.ValidateIdentifier(p.ObjectIdentifier, after); err != nil {
			return nil, "", err
		}
	}
	if err := s.enter(); err != nil {
		return nil, "", err
	}
	defer s.life.RUnlock()
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", dbError(ctx, err)
	}
	defer tx.Rollback()
	creator, err := contributionCreator(ctx, tx, dataset, principal)
	if err != nil {
		return nil, "", err
	}
	keys, err := contributionKeys(ctx, tx, dataset, creator, after, limit+1)
	if err != nil || len(keys) <= limit {
		return keys, "", err
	}
	return keys[:limit], keys[limit-1].ObjectID, nil
}

func contributionCreator(ctx context.Context, tx *sql.Tx, dataset, principal string) (string, error) {
	var secret []byte
	err := tx.QueryRowContext(ctx, `SELECT coalesce("creator_secret","secret") FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&secret)
	if err == sql.ErrNoRows {
		return "", store.Refuse("not_found")
	}
	if err != nil {
		return "", dbError(ctx, err)
	}
	if len(secret) != 32 {
		return "", store.Refuse("internal_error")
	}
	return recordstate.CreatorID(secret, dataset, principal), nil
}

func contributionKeys(ctx context.Context, tx *sql.Tx, dataset, creator, after string, limit int) ([]store.ObjectKey, error) {
	rows, err := tx.QueryContext(ctx, `SELECT o."type_id",c."object_id" FROM "tessera_contributions" c
		JOIN "tessera_objects" o ON o."dataset_id"=c."dataset_id" AND o."object_id"=c."object_id"
		WHERE c."dataset_id"=$1 AND c."creator_id"=$2 AND c."object_id">$3 ORDER BY c."object_id" LIMIT $4`,
		dataset, creator, after, limit)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer rows.Close()
	keys := make([]store.ObjectKey, 0, limit)
	for rows.Next() {
		var key store.ObjectKey
		if err = rows.Scan(&key.TypeID, &key.ObjectID); err != nil {
			return nil, dbError(ctx, err)
		}
		keys = append(keys, key)
	}
	if err = rows.Err(); err != nil {
		return nil, dbError(ctx, err)
	}
	return keys, nil
}

// Legacy records can be attributed only while their original stamp survives.
func (s *Store) backfillContributions(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT "dataset_id","body" FROM "tessera_types"`)
	if err != nil {
		return dbError(ctx, err)
	}
	byDataset := map[string]map[string]p.Meta{}
	for rows.Next() {
		var dataset string
		var raw []byte
		if err = rows.Scan(&dataset, &raw); err != nil {
			rows.Close()
			return dbError(ctx, err)
		}
		var meta p.Meta
		if json.Unmarshal(raw, &meta) != nil {
			rows.Close()
			return store.Refuse("internal_error")
		}
		if byDataset[dataset] == nil {
			byDataset[dataset] = map[string]p.Meta{}
		}
		byDataset[dataset][meta.ID] = meta
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return dbError(ctx, err)
	}
	text := `CAST(v."data" AS TEXT)`
	if s.postgres {
		text = `convert_from(v."data",'UTF8')`
	}
	for dataset, types := range byDataset {
		for id := range types {
			if recordstate.CreatorField(types, id) == "" {
				continue
			}
			meta, err := p.EffectiveMetadata(types, id)
			if err != nil {
				return err
			}
			for _, field := range meta.Fields {
				if !field.Creator {
					continue
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_contributions" ("dataset_id","object_id","creator_id")
					SELECT o."dataset_id",o."object_id",`+text+` FROM "tessera_objects" o
					JOIN "tessera_all_field_values" v ON v."dataset_id"=o."dataset_id" AND v."object_id"=o."object_id"
					WHERE o."dataset_id"=$1 AND o."type_id"=$2 AND v."field_id"=$3 AND v."kind"='text'
					ON CONFLICT ("dataset_id","object_id") DO NOTHING`, dataset, id, field.ID); err != nil {
					return dbError(ctx, err)
				}
			}
		}
	}
	return nil
}
