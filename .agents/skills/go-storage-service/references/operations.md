# Repository operations

Read only the section needed for the current action.

## Windows shell

On this Windows host, run Git, searches, file operations, Bash, Go and make in Ubuntu WSL.
Use PowerShell only to launch `wsl -d Ubuntu`; do not substitute native Windows repository tools.
`~/projects/tessera` resolves to `/mnt/c/Kea/projects/tessera`, sharing
`C:\Kea\projects\tessera`. Go 1.27.0 matches `mise.toml`; add `/usr/local/go/bin` to
PATH for Go/make because the WSL login shell omits it:

```powershell
@'
export PATH="/usr/local/go/bin:$PATH"
cd ~/projects/tessera
make check
'@ | wsl -d Ubuntu -- bash -lc 'tr -d "\r" | bash'
```

For one command: `wsl -d Ubuntu -- bash -lc 'cd ~/projects/tessera && git status --short'`.
Strip CR from multiline PowerShell stdin before Bash as above; preserve LF in repository text/scripts.
If blocked by `Wsl/E_ACCESSDENIED`, request tool execution approval rather than switching shells.

## Commands and CI

- `make check`: formatting, vet, race tests and devfaults. PostgreSQL can skip without its DSNs;
  report that coverage limitation.
- `make release-check`: requires `TESSERA_TEST_POSTGRES_DSN` and
  `TESSERA_TEST_POSTGRES_RESTORE_DSN` for two disposable databases; enforces PostgreSQL coverage.
  Keep credentials out of tracked files and command output.
- `make dev` / `make dev-content`: synthetic `./cmd/tesserad`. Stop it before an explicitly requested
  `make dev-reset`.
- `make ci`: `release-check` with disposable PostgreSQL 17 and two fresh databases. Requires Docker,
  Go, make, flock and a clean committed tree. Uses a dynamic loopback-only port and removes only its
  own container/volume; never prune shared caches.
- `make ci-remote`: that gate on the shared Linux host under `/root/heavy.lock`. Ignored
  `ci/remote.local.env` supplies `TESSERA_CI_HOST` / `TESSERA_CI_KEY` from this host's private local settings.
  Keep it private; pass the key only to SSH/SCP, never a shared agent. Host prerequisites:
  Go in `/usr/local/go/bin`, Git, Docker, make, flock, a C compiler for race tests, module/image access.
  Only committed source travels by Git bundle to a unique `/root/tessera-ci/` directory.
  Logs return to `.ci/<commit>/`; inspect the summary with `git notes --ref=ci show`.
  Failed transfers retain the remote run for diagnosis. Run remote CI for milestones or on request;
  routine edits use appropriate local checks.

[Releasing](../../../../docs/releasing.md) owns release/publication gates. Do not import client
branch workflows or deployment gates.
