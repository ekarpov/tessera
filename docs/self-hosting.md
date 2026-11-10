# Running the reference server

Run Tessera over HTTPS with SQLite or PostgreSQL, standalone accounts and optional
filesystem content. Operators provide encrypted live storage and retain recovery material
independently. Use the [development server](sqlite-development.md) for synthetic local experiments.

## Quick guide

1. Choose a [binary or container deployment](../deploy/README.md).
2. Configure TLS, issuer, private storage and the [first account](#configuration-and-first-account).
3. Configure [PostgreSQL](#postgresql) if needed; SQLite permits one process.
4. Set up [online backups](#backups-while-running), off-host retention and an isolated restore test.
5. Review [verification and upgrades](#verification-and-upgrades) before changing a deployment.

The sections below are the detailed configuration and API reference. For website sign-in,
see [first-party Console policy](#first-party-console-sign-in); ordinary apps use
[OAuth scope and consent](#oauth-consent-defaults-scope).

## Configuration and first account

Build with Go 1.27.0, CGO and a C compiler:

```sh
go build -o tesserad ./cmd/tesserad
```

Copy `deploy/server.example.json` and configure a private data directory, a valid TLS certificate/key
and the public HTTPS issuer origin. The process terminates TLS itself. Keep the certificate's private
key, database, filesystem content, identity registry and retirement ledger on encrypted storage.
`encrypted_storage_confirmed` is an operator assertion that this is configured; setting it does not
perform encryption. The portable implementation uses volume/filesystem encryption for live storage
and AES-256-GCM for exported backups. PostgreSQL storage/WAL and any database-server backups need the
same protection. Do not expose the development listener as a production listener.

Optionally set `"display_name": "Example Home"` in the configuration. The sign-in and account pages show it beside the host, and discovery publishes it
as a top-level label for people, beside the server software metadata. Leave it unset or empty to
omit it. A configured non-empty name must be valid UTF-8, at most 200 bytes, contain non-whitespace
text and contain no control characters; invalid names stop startup. A client shows this
unauthenticated claim next to the host name, never instead of it. No API changes it.

Optional `deprecated_versions` configuration publishes record-protocol retirement plans
in discovery, for example `[{"version":"1","sunset":"2030-01-01T00:00:00Z"}]`.
Leave it unset for `v0.1.0`: v1 is not being retired. Only distinct currently served
versions may be named (this binary serves `"1"`), with valid UTC timestamps to whole
seconds. Invalid entries stop startup before storage is opened. Empty configuration
omits the field. Reaching a date never disables a route. Operators must satisfy the
[overlap and notice contract](../specs/tessera-protocol-v1.md#versioning-and-compatibility),
retain dated evidence and postpone plans while clients still need the version. This
setting does not deprecate the independent content protocol. Do not change frozen
App Review configuration to exercise it; use isolated synthetic qualification fixtures.

**Behind a reverse proxy**, name it in `trusted_proxies` — addresses or CIDR ranges, such as
`["172.18.0.2"]`. **Name the proxy's exact address, not its network**: every address listed can write
`X-Forwarded-For` and be believed, so trusting a container network lets any other container on it
speak for any caller. In a container network, give the proxy a fixed address outside the range the
network assigns automatically, or another container can take it first. Sign-ins, tokens, registrations and invitations are
limited per caller address, and behind a proxy every connection comes from the proxy: without this
the limits are one budget for the whole server, which anyone on the internet can spend. With it, the
caller is the right-most `X-Forwarded-For` entry that is not a listed proxy. Leave it empty when the
server is reached directly, because then nobody's `X-Forwarded-For` should be believed.

Local provisioning commands require the serving process to be stopped. Create a private password file, mode 0600, containing an initial administrator password of at least
12 bytes. Provisioning runs locally and exits; neither passwords nor tokens are printed:

```sh
./tesserad -config /etc/tessera/server.json -add-person admin -password-file /secure/initial-password
./tesserad -config /etc/tessera/server.json
```

The local `-add-person` command provisions an entitled dataset administrator. Ordinary applications
receive only the privileges explicitly selected on their consent screen, intersected with the
account's privileges. Erase is unchecked by default. Person sign-in uses Authorization Code plus
S256 PKCE, exact registered callbacks (with the loopback-port exception), five-minute access tokens,
rotating refresh tokens and separately revocable installations. Public registration is rate-limited;
a registered client has no dataset access until authorization. Client names on consent are unverified.

Open `/admin` over HTTPS and authenticate with the administrator account: a person principal that
may erase a dataset. A service principal is refused, because administration is an interactive act.
The pages are server-rendered from embedded assets, load no scripts, and are never cached.

**Administration is per dataset.** An administrator sees and acts on the datasets where their
account holds a current `erase` grant, and on nothing else — not the accounts of another dataset,
not its credentials, not its schema. A server holding a dataset for each person on it is the case
this exists for: owning yours must not make you an administrator of everybody else's.

Two rights sit beside a dataset rather than inside one, and each is granted explicitly because a
right that is derived grows silently as the server does:

| Flag | Gives |
| --- | --- |
| `-may-create-datasets` | The server right to create a dataset, reported to callers in `server_access`. An account may hold it with no dataset at all, which is how the first user of a server is provisioned. |
| `-may-administer-server` | The right to act on what belongs to the server rather than to a dataset — an approval carrying a server right and naming no dataset. Without an account holding this, such an approval is administrable by nobody, which is deliberate: it is better than being administrable by whoever is nearest to it. |

```sh
./tesserad -config /etc/tessera/server.json -add-person operator   -password-file /secure/initial-password -may-create-datasets -may-administer-server
```

An account given either right, and neither `-grants` nor `-dataset`, holds no dataset. (Before
2026-09-27 the `-grants` default of read and write put it in the storage's first dataset anyway,
unless `-grants ""` was given.) Name `-grants` or `-dataset` to give it one.

An administrator confers only what they hold: the account form will not grant `create_dataset` from
an administrator who does not have it.

| Page | Shows |
| --- | --- |
| `/admin` | Dataset identity and replacement, engine, watermark, schema and view versions, content store, every capability as advertised or withheld, and the declared bounds. |
| `/admin/principals` | Accounts and services with their grants and live authorization counts, and registered clients as public or confidential. |
| `/admin/authorizations` | One row per installation, with its grants, live token count and state, plus the actions below. |
| `/admin/schema` | Effective metadata: inheritance, fields and who declares them, roles and annotation keys. |

**Withdrawing grants and revoking credentials are different actions.** Withdrawal removes dataset
access and leaves the credentials authenticating, so the holder can still recover the outcome of a
write it made while it could; revocation ends the credentials themselves. Neither affects another
installation of the same account. Read access can also be shared to exact concrete types, read-only.

Every mutation spends a single-use CSRF token bound to that administrator, so a resubmitted form
cannot act twice. Secrets are not stored in a form these pages can read: passwords, service secrets
and token values have no field to travel in, and only counts and existence are reported. The pages
read the dataset through a handle carrying read only, so administration cannot write, change schema
or erase, and no record value appears on any page. Provisioning remains local.

## Identity page branding

An optional operator-owned `identity_brand` in the server JSON configuration
brands every identity page: sign-in, consent, cancellation/completion, errors,
expired-flow recovery, invitations and account management. Omit it to keep the
default appearance. Branding changes presentation only; it does not change
credentials, consent, OAuth grants, callback registration or session lifetime.

```json
"identity_brand": {
  "name": "Example Home",
  "logo_file": "/etc/tessera/identity-logo.png",
  "light": { "accent": "#174b91", "surface": "#ffffff", "text": "#1c1c1e" },
  "dark": { "accent": "#9ec7ff", "surface": "#000000", "text": "#f2f2f7" },
  "web_client_ids": ["the-registered-console-client-id"]
}
```

`name` is required when the block is present: nonblank UTF-8 text, at most 200
bytes, without control characters. It is escaped as text and appears beside the
issuer host, which remains visible. It takes precedence over `display_name` on
identity pages; discovery's `display_name` is unchanged. The visible name supplies
the logo's accessible identity; the adjacent logo has an empty decorative alt.

`logo_file` is optional and must be an absolute local path to a regular PNG file,
at most 256 KiB and 512 by 512 pixels. Tessera reads it at startup, validates and
re-encodes its pixels, discarding metadata and trailing bytes. It serves the
result as `image/png` with `nosniff` and `no-store` from `/oauth/brand/logo`.
Remote URLs, SVG, JPEG, GIF and arbitrary HTML are unsupported. Without a logo,
that endpoint returns 404. No client can supply or select an asset URL.

Each palette and each of its three colors is optional; omitted values retain
the built-in light or dark defaults. Colors must be opaque `#RRGGBB`. Both text
and accent must have at least 4.5:1 contrast against the surface. Tessera chooses
black or white button text for contrast; notes, fields, errors and focus outlines
use the validated palette. The visitor's system preference selects light/dark.
Invalid configuration or an unreadable/invalid logo stops startup rather than
falling back silently. No custom CSS, templates, fonts or JavaScript are loaded.

`web_client_ids` is an operator allowlist of at most 64 distinct registered OAuth
client IDs. Designation applies only when the validated callback is HTTPS.
Listed clients get website wording, including **Return to website**; native
callbacks keep **Return to app**. All branded sign-ins say **Sign in to [name]**.
Client-supplied names stay separately labelled as self-reported. An HTTPS callback
alone and public registration metadata (including `application_type`, `logo_uri`
or an attempted branding block) cannot grant operator designation. Register the
Console first, then put its returned client ID in configuration and restart. A
changed/re-registered ID needs an operator configuration update. Unknown IDs
confer nothing; their presence is allowed so configuration can precede deployment.

The CSP still denies resources by default, allows only the generated stylesheet
by SHA-256, keeps `form-action 'self'` and prohibits framing. With a logo, only
the exact same-origin `/oauth/brand/logo` path is added to `img-src`; no scripts,
external fonts or arbitrary image sources are allowed. rc.3's explicit completion
link remains the way out to native and web callbacks. The logo and colors never
come from authorization query or form parameters.

Within the reference implementation,
`Authority.SetBrand(*identity.BrandConfig) error` exposes the same contract. Call it after setting the issuer and before serving requests, alongside
`SetDisplayName`; nil keeps default pages. Validation errors preserve the previous
configuration. Restart/reconfigure before serving to change the brand; this is
not a live per-request theme API.

## First-party Console sign-in

This opt-in mode is a narrow exception to individual OAuth consent for the
operator's own website, including account administration only where the person
currently holds it. It is separate from `identity_brand.web_client_ids`, which
changes wording only. Other clients retain their existing consent screens.

Configure a dedicated operator-provisioned identity, not a public registration:

```json
"first_party_consoles": [{
  "client_id": "home-console",
  "name": "Example Console",
  "origin": "https://console.example",
  "redirect_uri": "https://console.example/oauth/callback",
  "data_grants": ["read", "write", "insert"],
  "administer": true
}]
```

At most 16 identities are supported. IDs and names are nonblank text of at most
200 bytes (IDs contain no whitespace). Origin is an exact HTTPS origin without
a trailing slash, path, query or fragment. The callback is an exact absolute
HTTPS URL on that origin, at most 2048 bytes, with no userinfo or fragment.
There is no wildcard, loopback-port exception or origin inferred from a request.
A public client that already has that ID cannot be promoted: choose a dedicated
operator ID. Reserved identities persist in the registry; removing their
configuration disables their website sign-in instead of falling back to consent.
Public registration cannot reserve a caller-chosen ID or set this designation.

The website sends the usual `client_id`, exact `redirect_uri`, `response_type=code`,
`state`, S256 `code_challenge` and `code_challenge_method` to `/oauth/authorize`.
Do not send `dataset`, `scope` or `shared_reads` in this mode: the operator policy
is authoritative, and those parameters are refused. Remove the website's separate
welcome/remember-me form and link directly to this authorization request. Passwords
continue to be submitted only to Tessera. This repository does not own the
website's introductory page or its session storage.

Tessera shows one compact card with the operator identity/logo (if configured),
a short Sign in heading, autofill-enabled account/password fields, an unchecked
Keep me signed in choice and one Sign in button. It does not show a dataset
selector or permission picker. Wrong credentials show the same card with an
error and retain the entered account name and persistence choice. A successful
submission completes authorization immediately. A script-free document refresh
navigates to the validated Console callback after the same-origin POST completes;
no second click is needed. A fallback return link is provided. Ordinary OAuth
clients keep rc.3's explicit Return to app/website link and their consent step.
CSP keeps default-deny resources, hashed styling and `form-action 'self'`.

### Current account authority and operator ceiling

`data_grants` accepts only `read`, `write`, `insert`, without duplicates. Omitted
or empty means no data access. `administer` defaults false. Each request/token
issue applies those ceilings to the account's **current** dataset grants and
existing type/object/frozen-reader restrictions. Administration never implies
reading data. All currently accessible datasets are listed through the normal
`GET /v1/datasets`; selection belongs in website navigation.

Tessera does not interpret Home/Research roles, household membership or study
workflows. Provision account grants/shares to express them: a household owner
cannot read members' private datasets merely because they administer accounts;
a member sees only their own data and explicitly shared views; researcher access
stays within that account's grants. Signing in does not redeem an invitation,
join a study or approve study/exchange consent. Those remain explicit app actions.

Schema changes, record-protocol erasure and dataset creation are outside this
policy. They still need their existing explicit authorization paths. Allowed
administrative operations keep all their current per-account/per-dataset checks,
confirmation inputs and any required fresh password verification. Console access
cannot be copied into a device code; a native installation follows its own
supported authorization flow. Suspensions, token expiry, revocations and grant
withdrawals still apply. Withdrawing one Console installation's grants leaves its
credentials usable for outcome recovery but prevents automatic re-granting.

### Remember-me handoff

Only this sign-in form accepts the unchecked `remember=yes` choice. The value
is bound to the CSRF-protected flow and its single-use authorization code. It
is not a callback query parameter. On successful PKCE code exchange only,
`POST /oauth/token` adds:

```json
"console_session": {
  "origin": "https://console.example",
  "keep_signed_in": true,
  "max_age_seconds": 2592000
}
```

Unchecked gives `false` and `0`. The website must match the returned origin to
its configured origin, validate its OAuth state/issuer as usual, and apply this
to its own opaque cookie and encrypted refresh storage. Cap persistence at 30
days from that sign-in; refresh responses do not repeat or renew this choice.
Values posted at token exchange or passed in authorization query parameters do
not override it. Token rights, five-minute access lifetime and refresh-token
rotation/expiry are unchanged. Keep existing server-side revocation checks even
for remembered website sessions. The checkbox creates no Tessera browser session
and stores no password.

Expired/spent-flow recovery requires a new sign-in with a fresh CSRF and an
unchecked persistence choice; it never repeats an invitation. A wrong-password
retry on the live flow preserves the choice. A code remains PKCE/redirect/client
bound, expires after one minute and can be exchanged once.

The internal composition API is
`Authority.SetConsoles([]identity.ConsoleConfig) error`, called before serving,
with the same validation. The operator policy is startup configuration, not
public registration metadata or a live per-request option. Existing sessions
are rechecked against current policy, account authority and the registered binding.
The identity registry now writes **format 5**; format-4 inputs are readable, but
older binaries refuse a format-5 registry. Back up before upgrading; restore
the backup to roll back. This support first ships in candidate `v0.1.0-rc.4`.

## OAuth consent defaults (`scope`)

The reference server accepts the optional, case-sensitive, space-separated OAuth
`scope` parameter on `GET /oauth/authorize` as **requested grant hints**. For example,
`scope=read%20write` asks to preselect Read and Write. Supported names are `read`,
`write`, `insert`, `schema`, `erase`, `administer`, `create_dataset` and `shared_reads`.
Unknown names, non-space separators and values longer than 256 bytes are refused
with `400 invalid_scope`; duplicate parameters remain `400 invalid_request`.
Repeated names are harmless. An explicitly empty `scope` requests no preselection.

Only hinted Read, Write and Insert can be preselected, and only where held by the
signed-in account. With no `scope`, the safe defaults are Read and Insert,
intersected with held grants. For a named dataset, these are that dataset's grants.
For an unnamed dataset chooser, they are the grants held on **every offered
dataset**, so choosing another dataset cannot leave an unheld grant preselected.
A hint unavailable on one or more offered datasets is visibly listed on the page;
an unsupported Insert request never silently falls back to Read.

Schema, Erase, administration, dataset creation and shared reads always require
an explicit tick, even when hinted. `shared_reads` still requires the separate
`shared_reads=true` authorization parameter to offer that choice. Hints neither
confer authority nor limit the person's explicit choices. The submitted consent
remains authoritative, intersected with current held grants. The token response
does not echo these UI hints as granted scopes: clients inspect the dataset's
`access` (and `server_access` for server rights) to check actual permission and
refuse an operation if required access is missing.

Hints are held with the validated authorization request through wrong-password
retries and consent, and included in rc.3's bounded recovery link for expired or
spent flows. Recovery requires a fresh sign-in and consent. Account creation on
an invitation page is independent: the client retains its intended authorization
request, including `scope`, and starts or resumes normal sign-in after creation.
The invitation is spent once the account exists. If a later join fails, authorize
the existing account again; never redeem the invitation again. The client must
retain hints when it starts a new request after recovery context expires or the
server restarts. The Return to app completion page and its strict CSP are unchanged.

## Native and ordinary OAuth client presentation

Native applications continue to use the system browser (for example
ASWebAuthenticationSession), not an embedded login or a password proxy. These
presentation rules also apply to ordinary OAuth web clients; they do not enable
the separately operator-provisioned first-party Console policy.

Consent shows the signed-in account, dataset choice where needed, and one
sentence summarising the initially selected Read/Write/Insert grants. For
`scope=read%20write`, when held, this is **[client] will see your data and add,
change and delete entries.** Allow and Cancel stay visible. Individual grants,
including administration, are inside the collapsed **Change what the app may
do** disclosure. Keyboard users can open it and change any choice. Only submitted
checked values authorize access; the static initial summary does not override
edits. Unavailable hints remain visibly listed outside the disclosure. Schema,
Erase, administration, creation and shared reads are never preselected.

### Re-authorization and review

After fresh password sign-in, Tessera can omit repeated consent when the request
fits the **most recent live approval** for the same account, client and exact
callback. It never combines several approvals, uses another account's approval,
or falls back to an older broader choice after a newer narrower one. Revoked or
withdrawn data grants and expired account grants do not authorize reuse. Grants
issued by older builds without an exact recorded callback require consent once.

A named dataset must match the prior approval. When the request omits a dataset,
reuse keeps the person's previous dataset choice only while it is still offered;
it never chooses a new dataset. Shared reads and server rights must have been
explicitly approved before and still be requested and held. When `scope` is omitted, re-authorization continues only previously granted
Read/Write/Insert rights that are still held, without adding newly held rights;
it does not silently reduce a prior Write grant to a first-sign-in default.
An explicit scope, including empty, replaces that data request. Privileged rights
require an explicit hint for reuse. The resulting new
installation carries only that request's rights, capped by that approval
and current account authority. A new permission or an unavailable hinted grant
shows the review page instead. Schema, Erase and administration can be reused
only when explicitly requested and already granted, never as default selections.
This changes no confirmations/fresh authentication required for an operation.

Use `prompt=consent` to force the review page, including to choose another
dataset or reduce an earlier grant. It survives authorization recovery. Clients
and automated consent drivers must handle either completion after sign-in or a
consent page; drivers testing individual choices should request `prompt=consent`.
The standard sign-in form remains the authentication boundary. There is no
Tessera remembered browser-password session or embedded sign-in introduced here.

Native completion retains **Return to app** and its explicit link. Ordinary web
clients retain their explicit return link as well. Only the operator-provisioned
first-party Console uses automatic return. Repeated consent/code submissions do
not issue more credentials; existing bounded recovery requires fresh sign-in.

### Client name and icon registration

`client_name` remains self-reported OAuth registration metadata. The optional
`logo_png` field on `POST /oauth/register` is a base64-encoded PNG, for example:

```json
{
  "client_name": "Example Notes",
  "redirect_uris": ["com.example.notes:/oauth/callback"],
  "logo_png": "BASE64_ENCODED_PNG_BYTES"
}
```

The decoded input is limited to 16 KiB and 256 by 256 pixels, with transparency
preserved. Tessera decodes and re-encodes the image, drops metadata/trailing bytes
and caps stored output at 64 KiB. Registration bodies are limited to 32 KiB;
all registered icons together are capped at 8 MiB. Invalid icons are refused as
`400 invalid_client_metadata`; exhaustion of the total budget is
`429 temporarily_unavailable`. Registration's existing rate and client-count
bounds still apply. Omit the field for no icon. Arbitrary `logo_uri` URLs are
not fetched, and no client HTML, CSS, SVG or JavaScript is accepted.

Sanitized icons are served with PNG/nosniff/no-store from
`/oauth/client-logos/{client_id}`. CSP permits only this local asset path and the
optional operator logo path, keeping hashed styles, default deny, no scripts and
`form-action 'self'`. The client icon appears beside the requesting client's
name in the main content, separately from the operator's header identity and
host. The self-reported-name notice remains visible; copying the operator name
or logo confers no trust, role or first-party treatment. The same validation and
page mechanism serves native and ordinary web OAuth clients.

## Joining from an app, without a browser

On a home network with its own certificate authority, the system sign-in sheet on a phone trusts
only the system's certificates, so signing in through it means installing and trusting a profile on
every phone. An issuer configured with `"device_redemption": true` avoids that: an app redeems its
invitations directly for an installation on the device, and the account it makes has no password.
The app approves the server's certificate itself. See [the deployment
specification](../specs/tessera-deployment.md) for the routes.

The issuer can then reconnect such an account to a new phone, for somebody who lost theirs:

```sh
curl -u home-members:"$ISSUER_SECRET" -d rebind=sara https://home.example/oauth/invitations
```

That signs every other device of the account out. It also means whoever holds the issuer's secret can
reach the account, so tell people this when they join; once a member sets a password, from the app
through a link to this server's page, it stops.

The server's owner signs in from the app the same way, with a code written on the host while the
server is stopped. `-revoke-installations` first signs out every device the account has, for a lost
phone:

```sh
./tesserad -config /etc/tessera/server.json -device-code owner -revoke-installations -secret-output /secure/owner-code
```

## Services and automation

A service is its own principal, explicitly entitled by local administration:

```sh
./tesserad -config /etc/tessera/server.json -add-service service-id -secret-output /secure/service-secret
./tesserad -config /etc/tessera/server.json -add-token admin -secret-output /secure/automation-token
```

Service credentials use HTTP Basic at `/oauth/token` with `grant_type=client_credentials` and receive
short-lived bearer tokens. Renewed service tokens retain the same authorization identity, preserving
receipt ownership. Automation tokens last one year and are separately revocable. Neither command
grants erase to its resulting application authorization. The token-issuing command does not print its
secret; the output file is created exclusively with mode 0600. Admin revocation takes effect for new
requests immediately, including access tokens from a revoked refresh family. Revoking a service
authorization also disables its client secret, preventing it from immediately minting another
authorization.

`-add-service` gives the service the dataset `-dataset` names, or the only one storage holds. For a
service that keeps its own records, give it one nobody else holds, and have its identity written
where your tooling can read it:

```sh
./tesserad -config /etc/tessera/server.json -add-service console -new-dataset \
  -secret-output /secure/console-secret -dataset-output /secure/console-dataset
```

**A lost service secret is replaced, not recovered**: `-rotate-service-secret console -secret-output
/secure/console-secret-2` on the host, or `POST /admin/api/v1/services/{account}/secret` as the
server's administrator who also administers every dataset the service holds — whoever has the new
secret reads what the service reads. The old secret stops at once; the service keeps its authorizations, so its receipts
stay its own. A secret withdrawn by revoking the service stays withdrawn. After restoring onto a new
machine, this is how services sign in again without copying their secret files across.

## Suspending and deleting accounts

```sh
./tesserad -config /etc/tessera/server.json -suspend-account member
./tesserad -config /etc/tessera/server.json -reinstate-account member
./tesserad -config /etc/tessera/server.json -delete-account member
```

**Suspending** stops every credential the account holds at once and keeps them; reinstating
restores exactly what it had. The person is told `account_suspended` — at sign-in once their password
is right, and when their application refreshes — so the app can say to ask you rather than to sign in
again; a wrong password learns nothing. Use it for abuse or a lost device. **Deleting** removes the account
and its credentials permanently and retires its name, which is never issued again. Datasets somebody
else also administers are kept. A dataset the account is the **last administrator** of is erased with
it — there would otherwise be data nobody could administer — so the deletion refuses until you name
exactly those datasets:

```sh
./tesserad -config /etc/tessera/server.json -delete-account member -erase-datasets s-7f3a9c21b04e4d1789ab
```

The refusal lists what to pass. The last server administrator cannot be deleted.

The same actions are on `/admin/principals`, limited there to accounts whose every dataset you
administer, and never your own. A server administrator sees and acts on every account, but not the
datasets or applications of accounts outside what they administer. A deletion that erases names the
datasets on the page, as on the command line and the API, and goes ahead only when they are entered.

**People can delete their own account** with their password. A product sends them to Tessera's own
page, so the password never passes through the product:

```text
https://tessera.example.org/oauth/account/delete?client_id=…&return_to=https://console.example.org/done
```

The page asks for the account and password, then shows exactly which datasets will be erased and which
will stay, and deletes only on confirmation; the confirmation carries a one-time ticket, not the
password. `return_to` must be one of the product's registered redirect URIs, or no way back is shown.
`/oauth/account/password` is the same kind of page for changing a password. Both also answer as an
API:

```sh
curl -X POST https://tessera.example.org/oauth/account/delete \
  --data-urlencode account=member --data-urlencode password@/dev/stdin
```

A `409` with `"error":"erasure_required"` lists the datasets the deletion would erase; post again with
one `erase` field per dataset to confirm. Success answers `{"deleted", "erased", "kept"}`, where
`kept` names each dataset the account held that stays, with `reason` `another_administrator` or
`member` — records the person wrote there are not removed, and a product should say so. A bearer
token is not enough here, and a suspended account cannot delete itself.

## Invitations

An invitation lets a person create their own account. You configure an **issuer** — typically a
signup portal — with exactly what its invitees get:

```json
{ "datasets": ["s-7f3a9c21b04e4d1789ab"], "grants": "read,write" }
```

or, to give each invitee a new dataset of their own with a fixed schema:

```json
{ "template": { "administrator": "researcher", "display_name": "Participant",
                "types": [ { "id": "study.measurement", "name": "Measurement",
                             "fields": [ { "id": "n", "meta_id": "study.measurement", "type": "integer" } ] } ] },
  "grants": "read,write,erase", "cap": 200 }
```

```sh
./tesserad -config /etc/tessera/server.json -add-issuer portal -issuer-file portal.json -secret-output /secure/portal-secret
```

A template issuer must have a `cap`, because each redemption creates a dataset. The portal asks for
an invitation with its identifier and secret, and gives the person the link:

```sh
curl -u portal:$(cat /secure/portal-secret) -X POST https://tessera.example.org/oauth/invitations
```

The person opens `redeem_url`, chooses a name and a password, and signs in from their application
as usual. To offer a way back into the application afterwards, add its `client_id` and a
`return_to` to the link; the page shows **Continue to …** only when `return_to` is exactly one of
that client's registered redirect URIs, as the account pages do. A private-use scheme must contain a
dot, such as `com.example.app:/connect`. Invitations are single-use and expire after seven days. The password never passes
through the portal, and the portal holds nothing in the datasets it causes to be created.

**Without a portal**, when your administration tool is an application, a server administrator issues
from an issuer on their own sign-in instead of its secret — an issuer secret outlives every sign-in and
does not belong on a phone. `POST /admin/api/v1/issuers/{issuer}/invitations` with an installation
approved to administer, and optionally `{"account": …}` for an issuer that assigns names, answers
exactly as above. The issuer's cap, template and disabling apply; an issuer into existing datasets
also takes administering each of them. **Revoking that installation ends every invitation it issued
and nobody has redeemed**, so a lost phone's pending invitations go with it.

A template can give services or people read on every dataset it creates — a study's analysis service,
or a household service that reads only the types each member shares:

```json
{ "template": { "administrator": "researcher", "display_name": "{account}", "types": [ … ],
                "readers": [ { "account": "analysis" },
                             { "account": "home", "types": [ "study.measurement" ] } ] },
  "grants": "read,write,erase", "cap": 200 }
```

A reader must exist when the issuer is configured. A service that should hold nothing until the
template creates datasets is added with `-add-service analysis -secret-output … -no-dataset`, and asks
for a token per dataset with `grant_type=client_credentials&dataset=…`. A reader gets read only; each
dataset's administrator can change its types with `PUT /admin/api/v1/datasets/{dataset}/readers/home`,
which accepts a service as well as a person.

**An issuer's policy is fixed; its lifetime is not.** Raise or lower its cap, stop it, or replace a
secret its portal may have leaked:

```sh
./tesserad -config /etc/tessera/server.json -change-issuer portal -issuer-cap 500
./tesserad -config /etc/tessera/server.json -change-issuer portal -issuer-disabled true
./tesserad -config /etc/tessera/server.json -rotate-issuer-secret portal -secret-output /secure/portal-secret-2
```

A cap is at least 1; a stopped issuer is a disabled one. Disabling also stops its outstanding
invitations from being redeemed, and enabling it again restores them until they expire. A rotated
secret replaces the old one at once; invitations already handed out still work. What an issuer hands
out — its datasets, template, readers and grants — does not change, because datasets already
created would disagree with the new policy: configure a new issuer instead.

Add `"erase_with_account": true` to a template to have each dataset it creates erased when its
person's account is deleted, even though the template's administrator also holds it. Without it, a
custodian account that co-administers every member's dataset keeps a deleted member's records. The
deletion still lists those datasets and erases them only when named.

For a study, add `"assign_names": true` to the issuer file. The portal then chooses each account name
— pass `account=p-7f3a` with the request, or leave it out for a generated `p-…` name — and the
participant sees it as fixed, so nobody types their real name. A template's `display_name` may contain
`{account}`, which becomes the account each dataset is made for. A dataset's administrator renames
one with `PUT /admin/api/v1/datasets/{dataset}/name` and `{"display_name": "…"}`.

## Limits

New datasets take the development limits unless the configuration overrides them, member by member —
anything left out keeps its default:

```json
{ "limits": { "max_stored_bytes": 1073741824 } }
```

**A storage quota is optional and off by default.** `max_stored_bytes` caps how many bytes a dataset
may store; a write past it answers `507 dataset_full` with `details.reason: "storage_limit"`, and
the dataset's usage reports the quota as `max_bytes`. Without it, size alone never refuses a write,
and usage reports no maximum. `max_materialized_bytes` is a different setting: how much one operation
may hold in memory, always on, 64 MiB unless raised. Only an operation that needs the whole dataset
at once (a schema change that rewrites every record, say) reaches it, and is refused with
`details.reason: "materialization_limit"`.

A dataset keeps the limits it was created with. To change one that exists — for example after a write
answered `507 dataset_full` — raise it directly, or set `max_stored_bytes` to `0` to remove the quota:

```sh
echo '{"max_stored_bytes": 2147483648}' > raise.json
./tesserad -config /etc/tessera/server.json -set-dataset-limits s-7f3a9c21b04e4d1789ab -limits-file raise.json
```

`idempotency_retention_seconds` may only grow. How full each dataset is shows on `/admin`. Every
change takes effect at once, without a restart.

## Sizing

What an operation costs as one dataset grows, measured on 2026-09-27 on an Apple M4 laptop, PostgreSQL
17 in a local container, a type with one integer field, the median of repeated runs:

| Engine | Records | Insert one | Update one | Read one | Query ten | Newest ten | Aggregate all | Start page set (50 a page) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| SQLite | 1,000 | 0.15 ms | 0.15 ms | 0.05 ms | 0.07 ms | 0.4 ms | 2.1 ms | 0.6 ms |
| SQLite | 10,000 | 0.18 ms | 0.15 ms | 0.05 ms | 0.07 ms | 2.7 ms | 24 ms | 0.5 ms |
| SQLite | 100,000 | 0.15 ms | 0.14 ms | 0.04 ms | 0.08 ms | 31 ms | 249 ms | 17 ms |
| PostgreSQL | 1,000 | 3.7 ms | 2.0 ms | 0.8 ms | 1.0 ms | 1.7 ms | 3.7 ms | 2.9 ms |
| PostgreSQL | 10,000 | 1.3 ms | 1.4 ms | 0.7 ms | 2.0 ms | 7.0 ms | 31 ms | 4.3 ms |
| PostgreSQL | 100,000 | 1.6 ms | 1.4 ms | 0.8 ms | 13 ms | 62 ms | 519 ms | 3.3 ms |

Reading and writing records by identity, queries that stop early in identity order, and starting a
page set do not grow with the dataset. **Newest ten** is a query sorted on a field and limited to
ten, the list an app shows most: the database orders the rows and the read stops at ten, so it grows
with the type about fifteen times more slowly than before this was done in SQL. **A page set is
computed a page at a time**, as each cursor is first asked for, so starting one costs its first page
— it used to evaluate the whole result and store every page first, 440 ms at 100,000 on SQLite and
747 ms on PostgreSQL. A sort on a `fixed` or `reference` field, or a per-type selection, still reads
and sorts the whole scope, and a page set over one is still stored whole when it starts.
Aggregation reads the whole type, at a few microseconds a record. The PostgreSQL rows are noisier
than SQLite's; a local container is not a tuned server. Measured to 100,000 records; the 500,000 row
the table once carried costs gigabytes of disk to produce and moved nothing but the aggregate.

Two defaults bind before these sizes do: `max_scanned_rows` is 200,000 and `max_materialized_bytes`
64 MiB. A dataset per person rarely comes near either. A dataset that will, and whose people will
aggregate over it, is better split, or its limits raised knowing what the table says an aggregate
then costs. Measure your own with
`TESSERA_CAPACITY=1 go test ./internal/sqlengine -run TestCapacity -v -timeout 60m`.

## Backups while running

Completed scheduled and manual results can be discovered through the
[acquisition catalog](backup-acquisition.md#completed-result-catalog-and-collector-credentials).
A server administrator can provision a revocable retrieval-only collector credential;
keep the native schedule enabled when the collector only retrieves results. These
catalog and collector credentials are included in the current reference server.

```json
{ "backup": { "directory": "/var/backups/tessera", "key_file": "/etc/tessera/backup.key",
              "every_hours": 24, "keep": 7 } }
```

With positive `every_hours`, the server attempts a backup at startup, then one interval after each
scheduled attempt finishes or is refused; there is no queued-tick catch-up.
Set it to `0` when an administrative caller owns scheduling; acquisition remains enabled. All producers use the bounded
[backup acquisition service](backup-acquisition.md), which publishes an encrypted archive, immutable
retirement evidence captured after export, and authenticated pairing metadata. Manual and scheduled
backups share admission and rotation; leases protect copies from rotation. `keep` bounds published
sets, with one additional staging set. Keep the private 32-byte key separately.

The [native scheduler administration](backup-scheduler.md) adds sanitized observations
and durable live `every_hours`/`keep` commands for signed-in server administrators. Static values
bootstrap a missing settings journal; acknowledged durable values take precedence on restart.
A keep reduction installs policy before deferred, lease-respecting cleanup and preserves the last
archive/evidence pair. Save the observation's versioned `settings_export` separately; the explicit
local `-backup-settings-bootstrap /secure/settings-export.json` recovery input accepts that export
only with missing settings state and the same provisioned deployment identity. Recovery starts a
new settings epoch and rejects old commands. See the contract for exact request IDs, reconciliation,
clock windows, refusal codes and recovery limits.


For integrations, use `PUT /admin/api/v1/backup-operations/{id}`, reconcile with GET, acquire an
independent lease at `/leases/{lease}`, and copy the exact artifact IDs with `Tessera-Backup-Lease`.
Use `kind: "evidence"` to acquire newer retirement evidence independently. The linked contract has
complete API examples, snapshot/erasure semantics, authentication verification and restore commands.
This is a server-administrator-only interface. The legacy manual route still returns a filename;
its synchronous response is not a durable acquisition receipt.

Online filesystem exports require a shared local filesystem supporting hard links and flock, all
writers upgraded together, and at most 256 required content files. The 256 MiB archive ceiling remains;
SQL materialization now also counts allocation overhead. See the contract for bounds, abandoned-pin
recovery, legacy archive migration and unsupported registry-sharing arrangements.

## Passwords

```sh
./tesserad -config /etc/tessera/server.json -reset-password member -password-file /secure/new-password
```

Also on `/admin/principals` and the administration API. The account's existing sign-ins keep
working; suspend it if you suspect a compromise.

**Setting somebody's password is reading their data**, because whoever sets it can sign in with it.
So on the pages and the API it takes administering every dataset the account holds — a server
administrator who does not is refused, though they may still suspend or delete the account. The
command line, run by whoever holds the host, is not limited. The person is told: `GET /oauth/account`
answers `password_set_by` with who set it and when, until they choose their own at
`/oauth/account/password`, which also signs out every application signed in as them.

**A person who forgets their password uses their recovery code.** Every account made from an
invitation is shown one, once, on the page that made it — marked `data-recovery-code`, so a
product's own tests can check that everybody was given one; a person whose account an administrator
made gets one at `/oauth/account/recovery` with their password, which also replaces any earlier
code. `/oauth/account/recover` takes the account, the code and a new password, signs out everything
signed in as them, and shows a new code. Only a digest is kept, and no administrator surface shows
or issues a code — so where nobody may set a person's password, the code is their only way back,
and the command line's `-reset-password` the operator's last resort.

## Administration API

Everything above is also available as JSON at `/admin/api/v1`, for a product with its own interface.
A request carries a bearer token with `administer` authority and acts with the account's
current administrative standing. Ordinary clients require the person's explicit **Administer
on your behalf** choice. The [operator-designated Console](#first-party-console-sign-in)
uses its dedicated policy instead; it cannot grant authority the account does not currently hold:

| Route | For |
| --- | --- |
| `GET /overview` | Datasets you administer with their usage, and their accounts with `created_at`, `suspended_at`, `invited_by` and caller-scoped `rebindable`; `server_administrator` says whether you are one, and a server administrator sees every account and `server.last_backup_at` |
| `POST /accounts` | Add an account to a dataset you administer |
| `POST /accounts/{account}/suspend`, `/reinstate`, `/delete`, `/password` | Account lifecycle; `/delete` takes `{"erase": [...]}` when the account is a dataset's last administrator, and answers what it erased and `kept` |
| `PUT /datasets/{dataset}/name` | Rename a dataset you administer |
| `GET /datasets/{dataset}/readers`; `GET`, `PUT`, `DELETE /datasets/{dataset}/readers/{account}` | Who reads which types; `PUT` with `password` creates the account. `{account}` as `issuer:{issuer}` is a group — everyone that issuer made — and the listing carries `groups` with their members |
| `GET /datasets/{dataset}/contributors`; `GET`, `PUT`, `DELETE /datasets/{dataset}/contributors/{account}` | Who adds records of which types, seeing none of them. Same body as readers. An account may be both; removing one standing keeps the other |
| `PUT /datasets/{dataset}/limits` | Server administrator only |
| `POST /backups` | Server administrator only; synchronous compatibility adapter |
| `GET /backup-scheduler` | Server administrator only; native scheduler/settings/retention observation; see [contract](backup-scheduler.md) |
| `PUT`, `GET /backup-settings-operations/{request_id}` | Server administrator only; durable interval/producer-retention command and reconciliation |
| `PUT`, `GET`, `DELETE /backup-operations/{id}` | Server administrator only; accept/reconcile/cancel a bounded backup or evidence operation; see [acquisition](backup-acquisition.md) |
| `PUT`, `DELETE /backup-operations/{id}/leases/{lease}` | Protect a complete set with an independent renewable lease, or release it |
| `GET /backup-operations/{id}/artifacts/{artifact}` | Server administrator only; exact immutable artifact, with `Tessera-Backup-Lease` |
| `POST /services/{account}/secret` | The server's administrator who administers every dataset the service holds. A new service secret, shown once |
| `DELETE /invitations` with `{"invitation": code}` | End one waiting invitation, leaving the issuer's others; takes what issuing from that issuer takes |
| `POST /unshare` with `{"types": [...], "datasets"?: [...], "dry_run"?}` | Stop those types being shared — every group and named share — from the named datasets, or each one you may moderate: those you administer, and as the server's administrator those an issuer with `moderate_sharing` made. Deletes nothing; the dry run names who would stop reading, and the dataset's readers listing tells its own account who stopped what and when (`stopped`) |
| `POST /issuers/{issuer}/invitations` | Issue an invitation on your own sign-in, for an administration tool that should not hold the issuer's secret. Server administrator; an issuer into existing datasets also takes administering them. Revoking the sign-in ends its unredeemed invitations |
| `GET`, `POST /issuers`; `PATCH /issuers/{issuer}` with `cap` or `disabled`; `POST /issuers/{issuer}/secret` | Server administrator only; an issuer into existing datasets, and its secret, also take administering them. No listing carries a secret; rotating returns the new one once |

Any valid token, administering or not, can ask `GET /oauth/account` whose it is: `account`, `kind`,
`server_administrator`, `may_create_datasets`, and `installation` with what this sign-in was approved
for.

A refusal is `{"error": {"code": "…", "details": {…}}}`, and an unknown route answers `404` in that
shape. A body that does not parse says `details.reason` `malformed_body`, and one with a member the
route does not take says `unknown_member`; a deletion that would erase answers `409`
with `details.reason` `erasure_required` and `details.datasets`.

## PostgreSQL

Set `engine` to `postgres` and `postgres_dsn_file` to a file containing the connection string, mode
`0600`. It carries the database password, so the server treats it as a credential and refuses to
start on one other accounts can read — naming the file and its mode, because a deployment has
several and a refusal that named none of them was a restart loop pointing nowhere.
Use a dedicated database and a role permitted to apply the hand-written migrations. A database holds
as many datasets as it is given, and more than one process may serve them. Record writes serialize
through a transaction advisory lock scoped to the dataset, so writes to different datasets do not wait
for each other; reads use coherent SQL snapshots. The retirement fence lives in the configured data
directory and must outlive database restores. **Each process needs its own fence file**, on storage
that outlives it, because the fence is what refuses to serve a dataset a restore brought back.
For online archives, independent standalone identity registries cannot freeze one another: capture
is refused while another registered standalone registry owner is present. Evidence-only acquisition
remains available. See [the supported arrangement](backup-acquisition.md).

Each process holds a session advisory lock under its own executor identity, and every idempotency
claim records the executor that made it. A process resolves another's claim as abandoned only by
taking that executor's lock, which succeeds exactly when the session that held it has ended — never
by waiting out a clock, because a slow executor and a dead one look the same to one. A process that
loses its own lock stops serving rather than committing work another process may already have
resolved as failed.

**Stop every process running the previous version before starting this one.** A process from before
executors were recorded resolves every in-progress claim as abandoned, including claims a new process
is working on. A starting process refuses rather than running beside one, but the reverse order is
not detectable: do not start an old binary against a database a new one is serving.

SQLite and PostgreSQL share the exact evaluator, receipt implementation, pagination and attachment
authority. The current evaluator materializes bounded state: `max_scanned_rows` and
`max_materialized_bytes` are enforced rather than returning partial results. Host composition uses
native SQL in the same transaction via `WithTx`/`MutateWith`; callbacks must not commit, retain or
concurrently use that transaction or perform network I/O.

## SQLite stays one process

SQLite has no cross-process session lock, so it cannot be asked whether another
executor is still alive. It answers by exclusion instead: the engine takes an
exclusive lock on a file beside the database and refuses to open if another
process holds it. A starting process has therefore already proved no other
executor is running, which is what lets it resolve every in-progress claim as
abandoned — the thing PostgreSQL now decides per executor.

This is a decision rather than an omission. A timestamped lease was the
alternative and was not taken: a lease resolves live work as failed whenever an
executor is slower than the lease, which is a correctness failure traded for a
deployment shape SQLite is not the engine for. A deployment wanting more than
one process wants PostgreSQL.

One consequence remains: writes to different datasets in one SQLite database
still wait for each other, because one writer at a time is a property of the
file. PostgreSQL does not.

On a platform without the file lock the engine refuses to open at all, rather
than running without the exclusion its recovery depends on.

## Erasure, cleanup and recovery

Erasure replaces dataset identity, revokes all existing authorizations and provisions only the erasing
authorization on the replacement, with exactly the grants it held on what it erased — never more.
`replaced_dataset_id` supports recovery after a lost response. Old receipts remain owner-bound and
return identity-only results; they cannot expose old response bodies through access to the replacement.

Retirement and cleanup obligations commit together. Before acknowledgement, retired identities are
also fsynced to an independent `.retired` ledger. Several server processes sharing one PostgreSQL
database each keep their own ledger, and each brings its own up to date with every retirement any of
them made, at its next maintenance pass — within a minute. For a complete online export, use an
independent evidence acquisition: it reads all retirements
from a fresh SQL snapshot plus retained history, regardless of maintenance timing or which process
made an erasure. Startup reconciles the SQL queue before accepting
requests. A minute-based worker retries content deletion, idle import cleanup, physical SQL cleanup
and receipt pruning. Close cancels and joins that worker. A retired database restored beside its current
ledger is refused. Keep this ledger independently of the backup being restored: restoring an old ledger
alongside an old database would defeat the protection.

SQLite cleanup rewrites live pages and truncates WAL; PostgreSQL performs vacuum reclamation. Neither
is a promise of forensic overwriting on SSDs, storage snapshots or provider replicas. Protect those with
encryption and operational retention controls. The filesystem backend has no CDN, thumbnails or object
versions; external snapshots and copies must be retired by the operator. Downloaded copies are outside
the server's reach.

## Backup and restore

For supported online acquisition, verification and restoring an old archive with newer evidence,
follow [backup acquisition](backup-acquisition.md). The commands below are offline local adapters.

Generate and retain a private 32-byte binary backup key separately from the archive. Backup output is
created exclusively; an existing file is never overwritten:

```sh
./tesserad -config /etc/tessera/server.json -backup /secure/backup.enc -backup-key-file /secure/backup.key
```

Stop every process serving the database before running it: the export is a consistent picture of the
whole storage, and the restore fence it must agree with belongs to a process rather than to the
database.
It exports SQL authority state, identity registry and filesystem bytes in an authenticated encrypted
archive, bounded to 256 MiB. Export works with either SQL engine. Host-owned tables outside Tessera's
schema require the host's own coordinated backup; they are not silently included.

Restore targets a fresh SQLite data directory or a fresh dedicated PostgreSQL database, selected by
the destination configuration. Archives are portable between the SQL engines. Configure the fresh
directory/database, and supply the independent current retirement ledger from the original deployment:

```sh
./tesserad -config /etc/tessera/restore.json -restore /secure/backup.enc \
  -backup-key-file /secure/backup.key -retirement-fence /durable/current-retirement-ledger
```

Restore **re-applies every erasure the current ledger records**: a dataset or content store erased
since the backup was taken is erased again inside the restoring transaction, its bytes are not
written out, and everything else is restored. So one person erasing their data does not make
earlier backups unusable for everybody else. Restore refuses an unreadable ledger, authentication
failures, existing destinations and unsafe archive paths. Failure removes only newly created output, preserving existing retirement evidence. Versions and
watermark are regenerated; cursors and transfer URLs are not restored. The backup's dataset identity
and values are retained, and pending deletion state is retained for cleanup. Before serving, startup reconciles
the identity registry against the complete storage inventory. Grants, share/ownership
bindings and device codes for absent datasets are removed; installations bound to them
are revoked. A fixed-policy issuer naming any missing dataset is disabled without
changing its policy or moving it to a replacement. Unaffected datasets and account/server
rights remain intact. Inventory or registry-persistence failure prevents startup. This
also repairs stale references in an already-restored directory; it grants nobody access
to an erasure-created replacement. Switch the deployment to
the restored location while preserving its current retirement ledger; do not run it as a divergent copy.
A PostgreSQL restore refuses a database that already contains Tessera tables. A failed attempt may
leave an initialized empty database, so retry into a fresh database; it never overwrites an existing
deployment. SQL restoration commits only after filesystem and identity files have been restored.

## Content and sharing

Enable `content` to bind a private filesystem directory to the SQL authority. Import operations are
owner-bound; offset transfers enforce limits, exact retries and one final total. Original reads and
offset uploads are supported; optional renditions, ranges and whole-object mode are not advertised.
Control calls never fetch caller URLs. Bytes use a separate transfer route with five-minute credentials;
responses are download-only and sandboxed, so uploaded HTML cannot execute with the admin origin's
privileges. Dataset erasure retires the content store too. Content-only erasure preserves record values
and initially restricts the replacement store to the erasing authorization.

An authorized attachment commits in the record transaction. Shared readers resolve items only through
attachments in their current view. Knowing an item ID does not grant access, and reading it does not
grant authority to attach it elsewhere. Previously issued read URLs may remain useful until expiration;
clients must not describe revocation as recall of downloaded bytes.

Type sharing currently uses exact concrete types pending the author's scope clarification. Object grants
permit direct reads, never query discovery. Guest updates and deletes are refused; a guest adds
records only as a contributor, below. Changes to grants invalidate even
previously issued pages. Full dataset owners bypass row filtering. Administrative sharing changes are
explicit and separate from record writes; field values never determine visibility.

**One sign-in can read everything shared with a person.** An application asks
`/oauth/authorize` with `shared_reads=true`, alone or beside a `dataset`; the approval page offers,
unticked, to also read every dataset where the person is a reader, within what each share names,
**including anything shared with them later, without asking again**. That part is read only and
never reaches a dataset the person writes; a dataset approved beside it keeps exactly the rights
ticked for it. A new share reaches it at once
through the readers API, and otherwise from its next access token; a share ending is immediate.
Removing the application ends all of it. Discovery advertises `shared_reads`.

**A household can share with everyone its issuer made.** `PUT
/admin/api/v1/datasets/{dataset}/readers/issuer:{issuer}` with `{"types": [...]}` lets every account
that issuer's invitations made read those types, members who join later included, without anybody
naming them. Only an administrator of the dataset may, and only for the issuer that made their own
account. **Every invitation that issuer hands out is then read access to every such share**: keep
its cap, and disable it when the household is complete. The dataset's own account, and anyone who
writes it, is never a group reader; a share naming a member wins over the group's, and removing a
group member on their own is refused — change the group instead. Accounts made before this existed
belong to no group.

An issuer configured with `"moderate_sharing": true` lets the server's administrator stop what its
members share from the datasets it made them, one type and one dataset at a time, through
`/admin/api/v1/unshare` — without reading those datasets or deleting anything. Each stop is shown
to the member in their dataset's readers listing as `stopped`, with who and when, and sharing the
type again clears it. It is fixed when the issuer is configured: tell the people you invite.

**Contributors add records and see none.** A form, a device, a feed: something that asserts once and
never revises. Name one through `PUT /admin/api/v1/datasets/{dataset}/contributors/{account}` with the
concrete `types` it may add, as the dataset's administrator. A device is usually a service made with
`-add-service device -no-dataset`, which then asks for a token on that dataset with
`client_credentials`. What it adds is the dataset's from the moment it commits: it cannot read it back,
change it or delete it, and removing the contributor leaves every record it added where it is. It sees
the schema of the types it adds to and nothing else. An account may also read, by type; the two are
separate, and removing either keeps the other. A person's application adds only if its approval ticked
**Add records where you contribute**, which is on by default and grants nothing to an account that
does not contribute. Nothing bounds how much a contributor adds except the dataset's own limits, so
watch its usage.

`optional_reads` names optional read capabilities this deployment advertises. There are none left to
name: `aggregation`, `distinct_references` and `text_matching` are required operations now, and a
configuration naming one **fails at startup** rather than being accepted and ignored — it is asking
to withhold something the protocol no longer lets a server withhold, and starting anyway would leave
the file claiming a choice nobody has. An unknown name fails the same way: a server must never
advertise what it cannot do. Configuration can withhold an optional feature; it can never disable a
required operation.

If you are upgrading a configuration that names any of the three, delete the member.

Content stores advertise `thumbnails`. Renditions are computed per request and never cached, which
costs a little processing on each read and removes an entire class of cleanup obligation: there is no
derivative for an erase, a retirement or a restored backup to miss. Requests are bounded at 4096
pixels and refuse to decode images beyond 50 megapixels, so a small requested thumbnail cannot be
used to make the server do a large amount of work.

For a disposable local TLS setup and Swift OAuth callback/credential transitions, see the
client's own OAuth runbook.

**A self-signed certificate for a test server must still be a server certificate.** Apple platforms
reject a TLS server certificate without the `serverAuth` extended key usage, even when the client
pins it as its only trust anchor. They also cap validity: 398 days for a publicly trusted
certificate, and 825 days for one under a root the person installed. A plain `openssl req -x509`
makes a CA certificate without the usage, and an Apple client fails with "the certificate for this
server is invalid" before sending a request. Issue a leaf instead:

```sh
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
  -subj /CN=127.0.0.1 -addext "subjectAltName=IP:127.0.0.1,DNS:localhost" \
  -addext "extendedKeyUsage=serverAuth" -addext "basicConstraints=critical,CA:FALSE" \
  -addext "keyUsage=critical,digitalSignature" -keyout server.key -out server.crt
```

A server on a home network with its own CA keeps sign-in in the system sheet, as the protocol
requires: each phone installs the CA as a profile and turns on full trust for it. Showing these
pages in a view embedded in the app instead would let the app read the password, which is what
signing in through this server prevents. `make integration-auth` exercises the real standalone
composition over HTTPS, including service receipt ownership across token renewal and restart.

## Verification and upgrades

`make check` runs formatting, vet and race-enabled tests. The importable core, recovery, pagination and
content suites run on SQLite. Supply `TESSERA_TEST_POSTGRES_DSN` pointing to a disposable dedicated
database to run the same suites on PostgreSQL; those tests create and erase data. Supplying a second
disposable database as `TESSERA_TEST_POSTGRES_RESTORE_DSN` also exercises encrypted PostgreSQL
restore — it must be disposable rather than merely fresh, because a restore refuses a database that
already holds a deployment, so the second run into the same one would fail. `make release-check`
requires both and fails rather than skipping, which is what a release is verified with. Additional tests cover
process crashes, backup tampering/retirement, exact numeric values, attachment rollback, chunk limits,
revocation and OAuth replay/CSRF boundaries.

For unit files, a container build and a backup script, see [deploy/README.md](../deploy/README.md).
Those are examples to adapt, not a supported distribution.

### Upgrading

Check the [active App Review freeze and consumer upgrade gates](releasing.md#app-review-freeze)
before changing a shared deployment. Release availability alone does not authorize an upgrade.

SQL migrations are forward-only and run on startup. Take a protected backup and retain the
independent retirement ledger before upgrading.

**There is no downgrade.** A binary that meets a database from a newer version refuses to start,
naming `not_supported`, and leaves the schema untouched — so rolling the binary forward again
recovers the deployment without a restore. That refusal is deliberate and tested: operating on a
schema the binary does not understand fails silently, while refusing does not. Plan a rollback as
"restore the backup taken before the upgrade", never as "start the old binary".

Stop all previous-version writers before migrating. A writer already connected to PostgreSQL
can continue using the old schema rules until it stops. In particular, creator attribution depends
on every writer enforcing creator fields.

Stop the server before provisioning, backup, restore or reset commands; they take the same database.
A migration that fails leaves the transaction rolled back and the server refusing to start, which is
the same situation as a rollback: restore, or fix forward.

Current schema versions are SQLite 10 and PostgreSQL 7. An upgrade that crosses them is automatic and
needs no operator action beyond the backup, with one exception named above: every process running the
previous version must be stopped first.

## Freeze a reader share

The recorded owner of a dataset can keep a reader's access to existing records and
hide records added afterwards. Its ordinary installation approved for `write`
calls `POST /v1/datasets/{dataset}/readers/{account}:freeze` with no body. Named
readers, readers installed by a template, and groups (`issuer:{issuer}`) all work.
The boundary is the dataset sequence. Existing records remain current: edits are
visible and deletion removes them. Schema stays readable; content resolves only
through visible records. Counts, aggregates and pages cover only that view.

`GET /v1/datasets/{dataset}/readers` and `/{account}` show the share, including
`frozen_at` and `frozen_by`. Both require the same write approval. Administrators
can see these fields on their existing reader routes and freeze through
`POST /admin/api/v1/datasets/{dataset}/readers/{account}:freeze` with `administer`.
Only the recorded owner's write-approved installation can restore access with
`:unfreeze`. Template redemption records the invitee; dataset creation records
its creating account. This ownership follows erasure independently of grants.
An administrator who is not the owner is refused even with write approval.
For older and operator-created datasets without recorded ownership, administrators
are refused unfreeze; other write-approved accounts retain it. Listings remain
open to every write-approved account. No new approval scope is
introduced. Success is `204`; no standing is `403`, no share is `404`, and unfreeze
of an unfrozen share is `409`. Repeating freeze keeps the original cut.

Freeze and unfreeze invalidate the reader's old cursors with `410 view_changed`.
Unfreeze restores records added while frozen. Changing or recreating the share
cannot remove the freeze. The cut persists across restart, future sign-ins and
future group memberships. This adds no SQL migration; the identity registry's
version advances to prevent an older binary from discarding frozen-share policy.

## First-release page language

`v0.1.0` sign-in, approval and account pages are English only, regardless of `Accept-Language`.
The language-selection and catalogue structure, `data-page` markers and `Vary: Accept-Language`
remain so translations can return after the first release.
