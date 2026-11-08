# Repository layout: what lives here and what does not

The specifications, Go server and conformance suite share this repository. Reference clients
live in separate repositories. For the package map, read [architecture](architecture.md).
The reasons for keeping these boundaries follow.

**Reference clients in other languages get their own repositories** — `tessera-swift`,
`tessera-kotlin` — rather than a directory here. Swift Package Manager resolves a package from a
repository root and has no subdirectory form, so a client in a subfolder could be read but never
depended on. Tags would collide too: one `v0.2.0` cannot mean the specification, the Go server and a
client at once, and a client fix should not force a Tessera tag.

Splitting them is safe because the vectors are frozen and append-only. A client's vendored copy can
only be stale, never contradictory — it runs fewer cases, not different ones, which is what that
freeze was for. Language names rather than platform names: Swift runs beyond iOS and Kotlin beyond
Android, and a platform name would foreclose that.

**The server stays here, and the argument that moved the clients does not move it.** A `tessera-server`
repository was considered and declined. The tag collision is real for a client, whose only version is
its module's; it is already designed around for the server, because the contract carries its own
version — `tessera-protocol-v1`, `tessera-content-v1` versioning independently, `protocol_versions` on
the wire — and the module tag says nothing about it.

What decides it is `pkg/conformance`, which has no good home under a split. With the specification,
the specification repository becomes a Go module and most of the point is lost; with the server, the
proof of the contract lives inside one implementation, which is worse than today and is the coupling
a split is meant to break; on its own, it is a third module for a project with one server. The
frozen vectors have no such problem — JSON, language-independent, append-only — which is why the
clients could leave and the suite cannot.

The cost also lands now and the benefit later: the split buys that no implementation is privileged,
which is worth little while there is one, and it taxes a specification and an engine that currently
move together several times a day. **Revisit when any of these is true** — a second server
implementation exists, a client consumes the suite rather than the vectors alone, the contract
stabilises after `v1.0`, or someone wants the specification without a Go toolchain and says so. The
first forces it: a suite cannot stay inside one implementation once there are two.

**Where the suite belongs in principle is not settled**, only where it currently sits. This repository
says the suite is the proof of the contract and ships it as a package of the reference server, and
nothing records how that resolves. Deciding it is what makes a later split cheap rather than urgent.
