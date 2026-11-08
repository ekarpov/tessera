# Deployment options for agents

Choose the composition the operator actually needs. Configuration, security controls and
backup limits are defined in [self-hosting](../../docs/self-hosting.md), not by the launcher.

## Quick choice

| Need | Composition | Important constraint |
| --- | --- | --- |
| Local synthetic client work | `make dev` / `make dev-content` | Loopback only; not an operator deployment |
| One host, few dependencies | Binary under systemd with SQLite | One serving process; private persistent storage |
| Existing container operations | Container with SQLite or PostgreSQL | Durable private mounts and readable credential ownership |
| Multiple record-serving processes | PostgreSQL | Per-process retirement fences; coherent identity/backup ownership |
| Host-managed identity or atomic host writes | Embedded packages | Host must supply the [embedding guarantees](../../specs/tessera-embedding.md) |

## Configure and verify

Use [deployment assets](../../deploy/README.md), then the relevant self-hosting sections.
Inspect the actual unit/container configuration before changing paths or ownership.
Keep secrets out of source, environment dumps and tool output. Validate TLS, issuer/callbacks,
proxy trust, encrypted storage and current authority in the composed server.

Online backup acquisition is not equivalent to copying a live database. Read
[acquisition](../../docs/backup-acquisition.md) and [scheduler settings](../../docs/backup-scheduler.md)
before changing storage or adding workers. Check exact locks/hard links, archive bounds,
lease/rotation behavior, independent retirement evidence and restore into an isolated target.
The offline example script stops the service and must not be substituted for online acquisition.

For candidate qualification use the [rc.4 checklist](../../docs/rc.4-acceptance-checklist.md).
For upgrades/publication use [releasing](../../docs/releasing.md). Prepare reviewable changes;
do not infer deployment, tag or publication authorization from a successful local check.
