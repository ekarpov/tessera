package store

import p "github.com/ekarpov/tessera/pkg/protocol"

// Execute runs inside the caller-owned unit of work. A returned error must abort
// that unit. The same executor serves HTTP transactions and REST conveniences.
// Represent asks Execute to carry the stored row back with each insert, which
// a caller requests with `Prefer: return=representation`. It is a preference
// rather than an instruction: a caller that may write and not read gets the
// identities alone, because handing a representation to somebody who may not
// read it would be a disclosure rather than a convenience.
type Represent bool

// Execute runs inside the caller-owned unit of work. A returned error must abort
// that unit. The same executor serves HTTP transactions and REST conveniences.
func Execute(tx Tx, request p.Transaction, dataset string, limits p.Limits) (p.TransactionResult, error) {
	return ExecuteWith(tx, request, dataset, limits, false)
}

// ExecuteWith is Execute, told whether the caller asked for representations.
func ExecuteWith(tx Tx, request p.Transaction, dataset string, limits p.Limits, represent Represent) (p.TransactionResult, error) {
	result := p.TransactionResult{Results: make([]p.OperationResult, 0, len(request.Operations))}
	aliases := map[string]p.Reference{}
	type guard struct {
		parent string
		filter *p.Predicate
	}
	guards := map[string][]guard{}
	// Authorization failures precede all writes (and durable claim in persistent
	// adapters). Each operation requires only its own grant.
	if err := AuthorizeTransaction(tx, request); err != nil {
		return result, err
	}
	for i, op := range request.Operations {
		one, err := func() (p.OperationResult, error) {
			r := p.OperationResult{}
			fields, err := op.Values(aliases, limits)
			if err != nil {
				return r, err
			}
			target := p.Target{}
			if op.Target != nil {
				target, err = op.ResolveTarget(aliases)
				if err != nil {
					return r, err
				}
			}
			resolved, err := p.ResolvePredicateAliases(op.Filter, aliases)
			if err != nil {
				return r, err
			}
			predicate, err := p.ParsePredicate(resolved, limits)
			if err != nil {
				return r, err
			}
			switch op.Op {
			case "insert":
				records, e := tx.Insert(op.Type, fields)
				if e != nil {
					return r, e
				}
				row := records.Records[0]
				for _, g := range guards[op.As] {
					matches, e := tx.Matches(op.Type, row.ID.ID, g.parent, g.filter)
					if e != nil {
						return r, e
					}
					if !matches {
						return r, Refuse("invalid_filter")
					}
				}
				r.Objects = []p.Identity{{ID: row.ID.ID, MetaID: row.MetaID}}
				if represent {
					// Read back through the ordinary read path rather than
					// reshaping what the write returned, so the representation
					// is the one a read would give — including the version,
					// which the write does not compute.
					//
					// Named by the row's own type rather than the request's.
					// On an insert the two are equal by construction, so this
					// is not a distinction a test here can make; it is written
					// this way because it is the row being described, and
					// because the two stop being equal the moment anything
					// else reaches this line.
					stored, e := tx.Object(row.MetaID, row.ID.ID, nil)
					if e == nil {
						set := stored
						r.RecordSet = &set
					} else if code := p.ErrorCode(e); code != "permission_denied" && code != "unavailable" {
						return r, e
					}
				}
				if op.As != "" {
					aliases[op.As] = p.Reference{DatasetID: dataset, ObjectID: row.ID.ID, MetaID: row.MetaID}
				}
			case "update":
				if err = tx.Update(target.Type, target.ID, fields); err != nil {
					return r, err
				}
				n := 1
				r.Affected = &n
			case "delete":
				if err = tx.Delete(target.Type, target.ID); err != nil {
					return r, err
				}
				n := 1
				r.Affected = &n
			case "expect":
				return r, tx.Expect(target.Type, target.ID, fields, op.Version)
			case "expect_type":
				return r, tx.ExpectType(op.Type, op.SchemaVersion)
			case "expect_absent":
				rows, e := tx.Matching(op.Type, predicate)
				if e != nil {
					return r, e
				}
				if len(rows) > 0 {
					return r, Refuse("conflict")
				}
				for _, name := range op.Guards {
					guards[name] = append(guards[name], guard{op.Type, predicate})
				}
			case "update_where", "delete_where":
				if op.ObjectIDs != nil {
					ids, e := p.ResolveObjectAliases(op.ObjectIDs, aliases)
					if e != nil {
						return r, e
					}
					predicate = &p.Predicate{Kind: "object_ids", IDs: ids}
				}
				rows, e := tx.Matching(op.Type, predicate)
				if e != nil {
					return r, e
				}
				// Even an empty selection must reject fields outside the named schema.
				if op.Op == "update_where" {
					if e = tx.ValidateFields(op.Type, fields); e != nil {
						return r, e
					}
				}
				for _, row := range rows {
					if op.Op == "update_where" {
						e = tx.Update(row.TypeID, row.ObjectID, fields)
					} else {
						e = tx.Delete(row.TypeID, row.ObjectID)
					}
					if e != nil {
						return r, e
					}
				}
				n := len(rows)
				r.Affected = &n
			case "define_type":
				if op.Meta == nil {
					return r, Refuse("invalid_field")
				}
				m, e := tx.CreateMetadata(*op.Meta)
				if e != nil {
					return r, e
				}
				r.Type = &m
			case "update_type":
				if op.Meta == nil {
					return r, Refuse("invalid_field")
				}
				m, e := tx.UpdateMetadata(*op.Meta, op.Confirm)
				if e != nil {
					return r, e
				}
				r.Type = &m
			case "delete_type":
				types, n, e := tx.DeleteMetadata(op.Type, op.Cascade)
				if e != nil {
					return r, e
				}
				r.DeletedTypes = types
				r.DeletedObjects = &n
			default:
				return r, Refuse("invalid_field")
			}
			return r, nil
		}()
		if err != nil {
			return p.TransactionResult{}, p.AtOperation(err, i)
		}
		result.Results = append(result.Results, one)
	}
	result.Watermark = tx.Watermark()
	return result, nil
}
