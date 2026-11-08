# Deploying Tessera

Run the reference server as a binary or container with private persistent storage.
SQLite is the simplest single-process option; PostgreSQL supports multiple record-serving
processes. Both need TLS, encrypted storage, provisioning and tested recovery.
[Self-hosting](../docs/self-hosting.md) is the configuration reference.

## Quick guide: one host with systemd

Build with Go 1.27.0, CGO and a C compiler. As an operator with installation privileges:

```sh
go build -o tesserad ./cmd/tesserad
install -m 0755 tesserad /usr/local/bin/tesserad
useradd --system --home-dir /var/lib/tessera --shell /usr/sbin/nologin tessera
install -d -o tessera -g tessera -m 0700 /var/lib/tessera
install -d -o root -g tessera -m 0750 /etc/tessera
install -o tessera -g tessera -m 0600 deploy/server.example.json /etc/tessera/server.json
```

Before starting, edit the configuration: set the public HTTPS issuer, install a valid TLS
certificate/key and configure encrypted storage. The example deliberately sets
`encrypted_storage_confirmed` to false. Changing it asserts an existing storage control;
it does not encrypt the disk. Make credential files mode `0600`, owned by the service user.
The data directory must be `0700`. Verify access as that user, not only as root.

Create a private initial password file and provision the first account as described in
[self-hosting](../docs/self-hosting.md#configuration-and-first-account). Run the local
provisioning command as `tessera` with the service stopped. Then install and start the unit:

```sh
install -m 0644 deploy/tesserad.service /etc/systemd/system/tesserad.service
systemctl daemon-reload
systemctl enable --now tesserad
```

The unit runs without capabilities and restricts writes to `/var/lib/tessera`. If a
backup directory is elsewhere, explicitly grant that directory in a unit override.
It cannot bind privileged port 443; use a reverse proxy or a high port. Configure only
actual [trusted proxy addresses](../docs/self-hosting.md#configuration-and-first-account).

## Containers

[Containerfile](Containerfile) uses a two-stage Alpine build with CGO and the C runtime:

```sh
docker build -f deploy/Containerfile -t tesserad .
```

[The Compose example](compose.example.yaml) uses PostgreSQL. It mounts configuration,
TLS material and the DSN read-only, drops capabilities and uses a read-only root filesystem.
Use private durable volumes; the runtime user has UID 10001, so verify host mount ownership.
Credential files must also be mode `0600`: a read-only mount does not prevent other host
accounts from reading a secret. Keep database passwords in the DSN file, out of image
layers and inline configuration.

## Multiple processes and orchestration

SQLite permits one serving process per database, enforced by an exclusive file lock.
Avoid overlapping old/new pods when rolling it out. No Kubernetes chart is supplied.
PostgreSQL supports multiple record-serving processes, with each process's retirement fence
on persistent storage. This does not remove the
[identity-registry and archive consistency constraints](../docs/self-hosting.md#postgresql).
Do not assume a shared database makes independent registries a coherent deployment.

## Backups and recovery

Prefer the running server's [online acquisition](../docs/backup-acquisition.md) and
[native scheduler](../docs/backup-scheduler.md) for supported storage arrangements.
Copy encrypted archives, authenticated pairing metadata and current retirement evidence
off the host; retain the key and trusted producer identity independently. Test isolated restore.

[The offline example script](tesserad-backup.example.sh) stops the service before invoking
the CLI backup and restarts it afterwards. It is a downtime-based alternative, not the
online acquisition flow. Adapt it to the deployment and retain independent retirement
evidence; an encrypted archive alone is not sufficient erasure-safe recovery.

## Upgrading

Follow [the upgrade procedure](../docs/self-hosting.md#upgrading). Migrations are
forward-only. Rollback requires restoring compatible pre-upgrade state and retirement
evidence; do not run an older binary on newer state. Pre-releases have no migration
guarantee, and final `v0.1.0` requires fresh storage.
