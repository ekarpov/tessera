.PHONY: check release-check fmt test dev dev-content dev-reset dev-faults integration integration-auth faults

# Implemented Tessera subset gate. HTTP tests use temporary loopback listeners.
# -count=1 rather than letting the cache answer. These tests depend on state Go
# cannot see — a PostgreSQL database that may have been recreated, or stopped —
# so a cache hit can report a pass for a run that did not happen. It did: a
# release gate reported green on both engines while the database had been down
# for two hours.
check:
	@test -z "$$(gofmt -l pkg internal cmd db)" || (gofmt -l pkg internal cmd db; exit 1)
	go vet ./...
	go test -race -count=1 ./...
	go vet -tags devfaults ./...
	go test -race -tags devfaults ./internal/devfaults -count=1

# The gate a tag is cut against. `check` skips every PostgreSQL test when no
# DSN is set and still prints `ok`, so a run that exercised one engine reads
# exactly like a run that exercised two. This one refuses to skip: an absent
# DSN fails and names the variable. Set both, against disposable databases.
release-check:
	@test -n "$$TESSERA_TEST_POSTGRES_DSN" || (echo "set TESSERA_TEST_POSTGRES_DSN to a disposable database"; exit 1)
	@test -n "$$TESSERA_TEST_POSTGRES_RESTORE_DSN" || (echo "set TESSERA_TEST_POSTGRES_RESTORE_DSN to a second disposable database"; exit 1)
	TESSERA_REQUIRE_POSTGRES=1 $(MAKE) check

fmt:
	gofmt -w pkg internal cmd db

test:
	go test ./...

# Synthetic data only; credentials are written to a private local file.
dev:
	go run ./cmd/tesserad -dev

# Disposable synthetic integration environment, with attachments.
dev-content:
	go run ./cmd/tesserad -dev -dev-content

# Stop the server first. Retires all existing development records and attachments.
dev-reset:
	go run ./cmd/tesserad -dev -dev-content -dev-reset

# Isolated HTTP integration run; never touches .tessera-dev.
integration:
	go test -race ./internal/devserver -run TestClientIntegration -count=1 -v

# Standalone OAuth/PKCE over trusted local TLS; disposable synthetic data only.
integration-auth:
	go test -race ./internal/standalone -run TestClientAuthenticationIntegration -count=1 -v

# Synthetic transport faults for driving an independent client through a lost
# response, a throttle and a disconnect. Excluded from ordinary builds.
dev-faults:
	go run -tags devfaults ./cmd/tesserad -dev -dev-content -dev-faults

# The fault-injection suite, which only exists in a devfaults build.
faults:
	go test -race -tags devfaults ./internal/devfaults -count=1 -v

.PHONY: ci ci-remote
# Full gate with disposable PostgreSQL; clean committed tree required.
ci:
	bash ci/run.sh

# Same gate on the shared Linux host, under its heavy-work lock.
ci-remote:
	bash ci/remote.sh

.PHONY: browser-auth
# Actual WebKit CSP enforcement, using only a disposable synthetic authority.
# python -m pip install playwright==1.58.0 && python -m playwright install --with-deps webkit
browser-auth:
	@test -n "$$TESSERA_WEBKIT_PYTHON" || (echo "set TESSERA_WEBKIT_PYTHON to Python with Playwright/WebKit installed"; exit 1)
	go test -race ./internal/identity -run 'TestWebKit(AuthorizationCompletion|IdentityBranding|FirstPartyConsole|NativeConsentCustomization)' -count=1 -v
