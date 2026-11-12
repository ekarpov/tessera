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

## Principal-wide cleanup

A separate host operation is planned for enumerating and erasing a principal's
stamped contributions across datasets. That slice needs a bounded storage scan,
a plan for concurrent inserts and revocation, and rules for retained receipts,
content and restored backups. The host coordinates account-wide cleanup and owns
any placeholders it retains.
