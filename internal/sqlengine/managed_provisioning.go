package sqlengine

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

type approvedManagedType struct {
	signature [32]byte
	layout    installedLayout
}

// ConfigureManagedTypes is a trusted host policy for subsequent creations.
// It validates the declarations and provisions the tables before publishing
// the policy. The life lock also protects policy reads during creation.
func (s *Store) ConfigureManagedTypes(ctx context.Context, types []p.Meta) error {
	if len(types) > maxManagedLayouts {
		return store.Refuse("query_result_limit_exceeded")
	}
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return store.Refuse("dataset_unavailable")
	}
	if len(types) == 0 {
		s.managedTypes = nil
		return nil
	}
	fresh, err := recordstate.New(s.defaults)
	if err != nil {
		return err
	}
	image, err := fresh.Image(ctx)
	if err != nil {
		return err
	}
	image, err = defineCreationTypes(ctx, fresh, image, types)
	if err != nil {
		return err
	}
	policy := make(map[string]approvedManagedType, len(types))
	layouts := make([]installedLayout, 0, len(types))
	for _, declared := range types {
		meta := image.Types[declared.ID]
		raw, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		layout, err := declaredManagedLayout(meta)
		if err != nil {
			return err
		}
		policy[meta.ID] = approvedManagedType{sha256.Sum256(raw), layout}
		layouts = append(layouts, layout)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = s.lockManagedLayouts(ctx, tx); err != nil {
		return err
	}
	if err = s.registerManagedLayouts(ctx, tx, layouts); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return dbError(ctx, err)
	}
	s.managedTypes = policy
	return nil
}

func (s *Store) approvedManagedLayouts(types map[string]p.Meta) ([]installedLayout, error) {
	if len(s.managedTypes) == 0 || len(types) == 0 {
		return nil, nil
	}
	matching := make(map[string]approvedManagedType)
	for id, approved := range s.managedTypes {
		meta, exists := types[id]
		if !exists {
			continue
		}
		raw, err := json.Marshal(meta)
		if err != nil {
			return nil, err
		}
		if sha256.Sum256(raw) == approved.signature {
			matching[id] = approved
		}
	}
	var layouts []installedLayout
	for id, approved := range matching {
		// A matching child with a different ancestor is a different
		// effective definition and does not use the approved layout.
		parent := types[id].ParentID
		for parent != nil {
			if _, ok := matching[*parent]; !ok {
				break
			}
			parent = types[*parent].ParentID
		}
		if parent == nil {
			layouts = append(layouts, approved.layout)
		}
	}
	return layouts, nil
}
