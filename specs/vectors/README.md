# Tessera protocol vectors — frozen

These files are language-independent fixtures for the Tessera protocol. **They are frozen**, as of
`1de0bfe` in this repository and `35ad924` in the Swift client (branch
`remote-storage-backend`) — the two independent consumers that read them. A case may be added; an
existing one is not edited or removed, for the same reason a published vocabulary entry is not: a
changed fixture retroactively alters what a passing implementation was certified to do, and nobody
reviewing a diff reliably notices a flipped expectation.

Passing these files does not certify HTTP or engine conformance.

## What "both consumers pass" means

A consumer passes when it either matches a case or **records a documented divergence** — one that
names the cause and says why that cause is acceptable. Requiring a clean sweep would have kept these
files unfrozen indefinitely over spellings no conformant server emits, while the implementations they
exist to keep honest carried on diverging in the places that matter.

The Swift client passes 47 of 47 identifier cases and 189 of 194 value and comparison cases, with
five recorded divergences, all of one cause: it does not own a JSON reader, and the platform's does
not expose what these cases turn on.

| Case | What it asks for | What the platform does |
| --- | --- | --- |
| `integral_lexeme_92233720368547758070e-1` | The exact decimal value 2⁶³−1 | Parses through a double and overflows |
| `invalid_integer_1e-999999999` | Refuse it | Delivers a plain `0` |
| `float_underflow`, `float_negative_underflow` | Underflow to ±0 | Refuses as unrepresentable |
| `duplicate_member` | Refuse a repeated member | Silently keeps the last |

These describe a client's robustness against a hostile or careless server, not a disagreement about
what the bytes mean: a server MAY return an integer as plain decimal digits, and MUST reject a
duplicate member before any client sees one. A consumer that wants them has to parse the bytes
itself, which is a decision about its HTTP layer rather than about this protocol.

## Reading the files

- `values.json`: decode `input` (a JSON document stored as a string), enforce the
  optional byte limits, optionally convert to `convert_to`, and either produce
  `expected` or fail with `error`. Absent limits use the discovery example. The
  expected document fixes the tagged value and its exact meaning, not outer JSON
  whitespace, property order, or equivalent number spelling. JSON-typed strings
  must retain their contents exactly. Binary64 comparisons include signed zero.
- `comparisons.json`: decode `left` and `right`. Check `equal`; if supplied, check
  ascending `order` (-1, 0, 1) or `order_error`. `error` instead means equality is
  undefined for those types. Compare numbers without first rounding integers.
- `identifiers.json`: validate `value` as `kind`; either succeed or return `error`.
  Kinds include `content` for item IDs: nonempty, at most 255 UTF-8 bytes, no control
  characters and no `//`.

- `version-selection.json`: pass `advertised` and `supported` arrays to record-protocol
  selection; match `selected`, or refuse with no selected version when `refused` is true.
  `pkg/conformance.CheckVersionSelection` accepts these vectors and a consumer callback.
- `version-discovery.json`: validate version metadata in `document`; `valid` fixes the
  outcome. Unknown response fields are additive. These cases do not claim that operator
  overlap/notice obligations or client warning/no-request behavior have been verified.
  These two files were added for Finding 47; existing frozen cases are unchanged.

Media fixtures follow `54d6ba3`: `<content_store_id>//<item_id>` is always qualified,
including text-to-media conversion. Bare IDs and ambiguous item separators are rejected.
The integer-to-float representability boundary fixtures are retained unchanged.

**A minted object ID may not begin with `$`**, which is what lets a transaction alias stand where an
identity does. No case tests it, and none should: `$foo` remains a valid object ID to *receive*, and
the constraint is on what a server mints, which a value-validation fixture cannot express.

Fixtures contain only synthetic data. `go test ./pkg/protocol ./pkg/conformance` consumes every case
and checks that seeded broken encoders are detected. Request-boundary, schema,
record-set and digest tests are in that package as well.

Do not reserialize a retained request from these semantic vectors when retrying
a write: idempotency requires the original serialized request bytes.

## What these vectors found

Kept because it is the argument for having them at all, and for adding a case rather than trusting
prose. Each of these was live in a client that believed it implemented this specification:

- An integer compared with a float **through a `Double`**, so `9007199254740993` and
  `9007199254740992.0` were equal.
- Text compared by **Unicode canonical equivalence** rather than by code point — in the comparison
  routine and in the value type's own `==` — so a precomposed `é` equalled a decomposed one.
- `json` and `blob` **ordered**, where the protocol gives them equality and no order.
- The decoder **admitting values the protocol rejects**: malformed media, unparseable JSON,
  non-canonical base64, a null carrying a payload.
- The timestamp wire type spelled **`dateTime` rather than `date_time`**. Every timestamp that client
  sent would have been refused and every one it received rejected as an unknown type. It stayed
  invisible because seven of the eight date cases were passing *for the wrong reason*: they expect a
  rejection, and an unknown type name is one. The single case that expects success is what exposed it.
