# Scoped record access

Creator attribution, collection sharing, and scoped inserts, updates and deletes are implemented.
A schema declares an insertion-only text field with `"collection": true`.
A record's value in that field identifies its collection within the dataset.
The same collection can span concrete types when each type is explicitly granted.
Existing records without the field remain outside collections.

An embedding host uses `store.SharedView(nil).WithCollections(read, insert)` and
`SetAuthorization` to grant exact type and collection pairs. The host decides
which accounts or audiences may use each collection, distributes its ID, and
replaces or revokes grants. Collection grants use the existing `Read`, `Insert`
and expiry fields. The standalone account administration routes manage type
grants. Collection administration is available through the embedding API.

[Collection conformance](../pkg/conformance/collections.go) exercises the
portable record operations. The SQL suite checks managed and shared field
storage, content visibility and restore.

## Creator-scoped updates and deletes

The embedding host sets separate exact type and collection pairs through
`View.WithOwnMutations(update, remove)` with `Grants.UpdateOwn` and
`Grants.DeleteOwn`. The store checks current read access and the record's creator
stamp on every explicit update or delete. Changes use the ordinary transaction
and receipt paths.

## Contribution inventory and erasure

A trusted embedding host can page live attributed record identities with
`Contributions(ctx, dataset, principal, after, limit)`. Each call sees the live
index and returns a position for the next page. An insert's creator stamp stays
indexed if a schema change later removes the visible creator field. Upgrades
recover existing stamps that are still present in storage.

`BeginContributionErasure` revokes the principal's authorizations and receipts
in that dataset and returns a stable job ID. `StepContributionErasure` deletes
bounded batches and can resume across restarts. Batches remove record links and
retained page snapshots, and mark attachments with no surviving references for
the existing content cleanup. The erasure fence requires an archive taken
after the job completes before that dataset can be restored. Keep the current
fence independently of the archive.

The host runs this operation for each dataset in scope, handles account
identity and cross-service coordination, and owns any placeholders it retains.
