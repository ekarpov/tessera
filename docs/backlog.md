# Backlog

Agreed work that remains open. Remove an item when it lands; its implementation and
regression tests remain in Git. Check [the decision index](asked-and-answered.md) before
proposing additional protocol work.

## Active: the first release

Complete the [rc.4 acceptance checklist](rc.4-acceptance-checklist.md), resolve reported
defects, then follow [the release procedure](releasing.md). Independent app/device and
server acceptance is still required. No new tag or publication is implied by this list.

## Stream aggregates instead of materialising them

Defer until after the first release.

**Problem.** `Aggregate` (`internal/recordstate/aggregate.go`) gathers its rows through `exact()`
and `matching()`. That collects every key of the type, sorts them by an identity order the
aggregate doesn't need, then fetches each record. Aggregating 100,000 records takes 249 ms on SQLite
and 519 ms on PostgreSQL (see [sizing](self-hosting.md#sizing)).

**Change.** Read candidate rows from an unordered streaming source modelled on `sortedObjects`
(`internal/sqlengine/sorted.go`). Keep the arithmetic in the exact Go accumulator and never move
SUM into SQL. Leave the wire unchanged: the same answers, the same refusals and the same scan bound.
Apply the view, the frozen-share sequence cut and the exact type to every row. Keep the existing
path for write transactions with changes, for the in-memory engine and for distinct references.
Measure before and after with `TestCapacity`, and drop the change if the gain at 100,000 rows is
under about 2×.


## Extend online backup filesystem scale

The initial [administrative acquisition service](backup-acquisition.md) bounds pin preparation to
256 required content files and requires reliable local flock/hard links. A larger deployment needs
measured preparation bounds and a storage-level snapshot or immutable chunk model before raising
that ceiling. Distributed byte stores and multiple writers of one identity registry remain outside
the supported online snapshot model. Do not substitute an unlocked mutable file or stale ledger.
