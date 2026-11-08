package admin_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekarpov/tessera/internal/standalone"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const administrator = "synthetic-administrator"
const passphrase = "correct horse battery staple"

// Authenticating costs a deliberate 600k-iteration derivation, so these tests
// are CPU-bound rather than slow for any interesting reason. Each builds its
// own runtime in its own temporary directory and shares nothing, so they run
// in parallel; serially the package takes about five times as long.

// runtime composes the real standalone server, so these tests exercise the
// administration surface as it is actually mounted rather than in isolation.
func runtime(t *testing.T) *standalone.Runtime {
	t.Helper()
	config := standalone.Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite",
		EncryptedStorage: true}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := standalone.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if err = r.Authority.AddPerson(administrator, passphrase, only(t, r), store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	return r
}

func get(t *testing.T, r *standalone.Runtime, path, user, secret string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("GET", path, nil)
	if user != "" {
		request.SetBasicAuth(user, secret)
	}
	w := httptest.NewRecorder()
	r.Handler.ServeHTTP(w, request)
	return w
}

func post(t *testing.T, r *standalone.Runtime, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(administrator, passphrase)
	w := httptest.NewRecorder()
	r.Handler.ServeHTTP(w, request)
	return w
}

// csrf reads a fresh single-use token out of the rendered page, the same way a
// browser would submit it.
func csrf(t *testing.T, r *standalone.Runtime) string {
	t.Helper()
	body := get(t, r, "/admin/authorizations", administrator, passphrase).Body.String()
	_, after, found := strings.Cut(body, `name="csrf" value="`)
	if !found {
		t.Fatal("no CSRF token in the authorizations page")
	}
	token, _, _ := strings.Cut(after, `"`)
	if token == "" {
		t.Fatal("empty CSRF token")
	}
	return token
}

func TestAdministrationRequiresAnAdministrator(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	if err := r.Authority.AddPerson("reader", passphrase, only(t, r), store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authority.AddService("synthetic-service", only(t, r), store.Grants{Read: true, Write: true}); err != nil {
		t.Fatal(err)
	}
	// Authenticating costs a deliberate 600k-iteration derivation, so the
	// refusal matrix runs against one page. Every page shares the same guard,
	// which the per-page checks below confirm.
	for _, who := range [][2]string{{"reader", passphrase}, {administrator, "wrong"}, {"absent", passphrase}, {"synthetic-service", passphrase}} {
		if w := get(t, r, "/admin/authorizations", who[0], who[1]); w.Code != 401 {
			t.Fatal("refused principal reached administration", who[0], w.Code)
		}
	}
	for _, page := range []string{"/admin", "/admin/principals", "/admin/authorizations", "/admin/schema"} {
		if w := get(t, r, page, "", ""); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("unauthenticated request reached", page, w.Code)
		}
		if w := get(t, r, page, administrator, passphrase); w.Code != 200 {
			t.Fatal("administrator refused", page, w.Code)
		}
	}
}

// Every mutation needs a token from a page this administrator was served, so a
// resubmitted or forged form cannot act.
func TestMutationsRequireASingleUseToken(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	id := authorization(t, r)
	for _, form := range []url.Values{
		{"action": {"revoke"}, "authorization": {id}},
		{"action": {"revoke"}, "authorization": {id}, "csrf": {"forged"}},
	} {
		if w := post(t, r, "/admin/authorizations", form); w.Code != 403 {
			t.Fatal("acted without a valid token", w.Code)
		}
	}
	token := csrf(t, r)
	spent := url.Values{"action": {"withdraw"}, "authorization": {id}, "csrf": {token}}
	if w := post(t, r, "/admin/authorizations", spent); w.Code != 303 {
		t.Fatal("valid token refused", w.Code, w.Body.String())
	}
	// The same token a second time is a replay.
	if w := post(t, r, "/admin/authorizations", spent); w.Code != 403 {
		t.Fatal("token was reusable", w.Code)
	}
}

// Withdrawing grants and revoking credentials are different answers. This is
// the distinction the standalone authentication gate could not reach over HTTP
// before this surface existed.
func TestWithdrawnGrantsLeaveCredentialsUsable(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	bearer := automation(t, r)
	id := holder(t, r, bearer)
	if w := get(t, r, "/v1/datasets", "", ""); w.Code != 401 {
		t.Fatal("unauthenticated dataset read", w.Code)
	}
	if code := datasets(t, r, bearer); code != 200 {
		t.Fatal("fresh automation token cannot read", code)
	}
	if w := post(t, r, "/admin/authorizations", url.Values{"action": {"withdraw"}, "authorization": {id}, "csrf": {csrf(t, r)}}); w.Code != 303 {
		t.Fatal("withdrawal refused", w.Code, w.Body.String())
	}
	// The credential still authenticates: the failure is authorization, not
	// authentication, and discovery simply stops listing the dataset.
	if _, err := r.Authority.Authenticate(context.Background(), bearer); err != nil {
		t.Fatal("withdrawal invalidated the credentials", err)
	}
	if code := datasets(t, r, bearer); code == 200 {
		t.Fatal("withdrawn grants still read the dataset")
	}
	page := get(t, r, "/admin/authorizations", administrator, passphrase).Body.String()
	if !strings.Contains(page, "grants withdrawn") {
		t.Fatal("the page does not report the withdrawal")
	}
	// Withdrawing twice is refused rather than reported as a fresh change.
	if w := post(t, r, "/admin/authorizations", url.Values{"action": {"withdraw"}, "authorization": {id}, "csrf": {csrf(t, r)}}); w.Code != 400 {
		t.Fatal("repeated withdrawal was not refused", w.Code)
	}
	if w := post(t, r, "/admin/authorizations", url.Values{"action": {"revoke"}, "authorization": {id}, "csrf": {csrf(t, r)}}); w.Code != 303 {
		t.Fatal("revocation refused", w.Code)
	}
	if _, err := r.Authority.Authenticate(context.Background(), bearer); err == nil {
		t.Fatal("revocation left the credentials working")
	}
}

// Nothing secret is stored in a form these pages can render, so no page can
// print one. This checks the actual bytes rather than the intention.
func TestSecretsAreNeverRendered(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	serviceSecret, err := r.Authority.AddService("synthetic-service", only(t, r), store.Grants{Read: true})
	if err != nil {
		t.Fatal(err)
	}
	bearer := automation(t, r)
	authorization(t, r)
	for _, page := range []string{"/admin", "/admin/principals", "/admin/authorizations", "/admin/schema"} {
		w := get(t, r, page, administrator, passphrase)
		body := w.Body.String()
		for name, secret := range map[string]string{"service secret": serviceSecret, "automation token": bearer, "administrator password": passphrase} {
			if strings.Contains(body, secret) {
				t.Fatal(page, "rendered the", name)
			}
		}
		// These pages are private to the administrator who asked for them.
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(page, "is cacheable")
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatal(page, "can be framed")
		}
	}
	// The registry file holds hashes; the pages must not carry those either.
	raw, err := os.ReadFile(filepath.Join(r.Config.DataDir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), serviceSecret) || strings.Contains(string(raw), passphrase) {
		t.Fatal("the identity registry stored a secret in the clear")
	}
}

// Administration reads the dataset through a handle that carries read only, so
// the surface cannot write, change schema or erase even if a page tried.
func TestAdministrationCannotMutateTheDataset(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	inspector := store.Access{Principal: "administration", Authorization: "administration-inspector"}
	dataset := datasetID(t, r, "")
	err := r.Engine.Transact(t.Context(), inspector, dataset, func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: "test.admin.forbidden"})
		return e
	})
	if p.ErrorCode(err) != "permission_denied" {
		t.Fatal("the administration handle could change schema", err)
	}
	err = r.Engine.Transact(t.Context(), inspector, dataset, func(tx store.Tx) error {
		_, e := tx.Insert("test.admin.forbidden", nil)
		return e
	})
	if p.ErrorCode(err) != "permission_denied" {
		t.Fatal("the administration handle could write records", err)
	}
}

// Schema inspection shows metadata, never the values a record holds.
func TestSchemaInspectionShowsNoRecordValues(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	bearer := automation(t, r)
	const typeID = "test.admin.note"
	const secretValue = "a value no administrator should read"
	body := `{"type":{"id":"` + typeID + `","name":"Note","fields":[{"id":"body","meta_id":"` + typeID + `","name":"Body","type":"text"}]}}`
	dataset := datasetID(t, r, bearer)
	if code := call(t, r, "POST", "/v1/datasets/"+dataset+"/meta", bearer, body); code != 201 {
		t.Fatal("schema creation failed", code)
	}
	record := `{"fields":{"body":{"type":"text","value":"` + secretValue + `"}}}`
	if code := call(t, r, "POST", "/v1/datasets/"+dataset+"/objects/"+typeID, bearer, record); code != 201 {
		t.Fatal("record creation failed", code)
	}
	page := get(t, r, "/admin/schema", administrator, passphrase).Body.String()
	if !strings.Contains(page, typeID) || !strings.Contains(page, "Body") {
		t.Fatal("schema page does not describe the type")
	}
	if strings.Contains(page, secretValue) {
		t.Fatal("schema page rendered a record value")
	}
}

// The capability table reports decisions, so the only names in it are names a
// deployment can decide about.
//
// This assertion used to require the opposite. It checked that `aggregation`
// and `distinct_references` appeared somewhere on the page, which they did —
// as rows saying `withheld`, for three operations the protocol had already
// made required and this server always performs. The test passed throughout,
// because substring presence is not the question; where a name appears is.
func TestOverviewReportsWithheldCapabilitiesAndNotRetiredOnes(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	page := get(t, r, "/admin", administrator, passphrase).Body.String()
	for _, fragment := range []string{"advertised", "withheld", "max_request_bytes"} {
		if !strings.Contains(page, fragment) {
			t.Fatal("overview omits", fragment)
		}
	}
	for _, name := range p.DatasetCapabilities() {
		if !strings.Contains(page, "<td>"+name+"</td>") {
			t.Fatal("the overview omits the capability", name)
		}
	}
	for _, name := range p.RetiredReadCapabilities {
		if strings.Contains(page, "<td>"+name+"</td>") {
			t.Fatal("the overview reports", name, "as a decision; it is a required operation")
		}
		if !strings.Contains(page, "<code>"+name+"</code>") {
			t.Fatal("the overview does not say where", name, "went")
		}
	}
	// How full the dataset is; this one has no quota, so none is shown.
	if !strings.Contains(page, "Stored bytes") || !strings.Contains(page, "no quota") {
		t.Fatal("the overview does not show how full the dataset is")
	}
	// Withdrawn from v1, so it is not a capability this deployment withholds.
	if strings.Contains(page, "locales") {
		t.Fatal("the overview lists a capability the protocol no longer has")
	}
}

// Pages are private to the administrator who asked for them.
func TestUnknownAdministrationPageIsRefused(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	if w := get(t, r, "/admin/nonexistent", administrator, passphrase); w.Code != 404 {
		t.Fatal("unknown page did not 404", w.Code)
	}
}

func authorization(t *testing.T, r *standalone.Runtime) string {
	t.Helper()
	if _, err := r.Authority.AddAutomation(t.Context(), administrator, only(t, r)); err != nil {
		t.Fatal(err)
	}
	for _, auth := range r.Authority.AdminOverview(t.Context(), administrator).Authorizations {
		if auth.Principal == administrator && !auth.Revoked && !auth.Withdrawn {
			return auth.ID
		}
	}
	t.Fatal("no authorization to act on")
	return ""
}

// holder names the authorization a bearer token belongs to, so a test acts on
// the same installation it is checking.
func holder(t *testing.T, r *standalone.Runtime, bearer string) string {
	t.Helper()
	access, err := r.Authority.Authenticate(t.Context(), bearer)
	if err != nil {
		t.Fatal(err)
	}
	return access.Authorization
}

func automation(t *testing.T, r *standalone.Runtime) string {
	t.Helper()
	token, err := r.Authority.AddAutomation(t.Context(), administrator, only(t, r))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func datasets(t *testing.T, r *standalone.Runtime, bearer string) int {
	t.Helper()
	request := httptest.NewRequest("GET", "/v1/datasets", nil)
	request.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	r.Handler.ServeHTTP(w, request)
	if w.Code == 200 && !strings.Contains(w.Body.String(), `"id"`) {
		return 404
	}
	return w.Code
}

func datasetID(t *testing.T, r *standalone.Runtime, bearer string) string {
	t.Helper()
	held, err := r.Engine.Datasets(t.Context(), store.Access{Principal: "administration", Authorization: "administration-inspector"})
	if err != nil || len(held) != 1 {
		t.Fatal("expected exactly one dataset", err)
	}
	return held[0].ID
}

func call(t *testing.T, r *standalone.Runtime, method, path, bearer, body string) int {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.Handler.ServeHTTP(w, request)
	return w.Code
}

// The outcome banner is chosen from a fixed set on the server, so a crafted
// link cannot put wording of its own in front of an administrator.
func TestOutcomeMessagesAreNotTakenFromTheURL(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	injected := "Your account is suspended, telephone this number"
	page := get(t, r, "/admin/authorizations?done="+url.QueryEscape(injected), administrator, passphrase)
	if page.Code != 200 {
		t.Fatal(page.Code)
	}
	if strings.Contains(page.Body.String(), "telephone this number") {
		t.Fatal("the page repeated a message supplied in the URL")
	}
	id := holder(t, r, automation(t, r))
	if w := post(t, r, "/admin/authorizations", url.Values{"action": {"withdraw"}, "authorization": {id}, "csrf": {csrf(t, r)}}); w.Code != 303 {
		t.Fatal("withdrawal refused", w.Code)
	}
	// A real action still reports itself.
	if body := get(t, r, "/admin/authorizations?done=withdrawn", administrator, passphrase).Body.String(); !strings.Contains(body, "still authenticate") {
		t.Fatal("the page did not report a genuine outcome")
	}
}

// only names the storage's single dataset. These fixtures provision one, and a
// grant has to name the dataset it is over now that a storage may hold several.
func only(t *testing.T, r *standalone.Runtime) string {
	t.Helper()
	held, err := r.Engine.Provisioned(t.Context())
	if err != nil || len(held) != 1 {
		t.Fatal("fixture should hold exactly one dataset", err)
	}
	return held[0].ID
}

// Creating an account had no HTTP path at all: the surface could revoke,
// withdraw and share, and provisioning a person was a CLI flag. So a web
// administration front end could define types through the protocol and still
// not create the person who would use them.
func TestAnAccountCanBeAddedFromAdministration(t *testing.T) {
	r := runtime(t)
	dataset := only(t, r)

	page := get(t, r, "/admin/principals", administrator, passphrase)
	if page.Code != 200 {
		t.Fatal("principals page unavailable", page.Code)
	}
	if !strings.Contains(page.Body.String(), dataset) {
		t.Fatal("the account form offers no dataset to add to")
	}

	add := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		form.Set("csrf", r.Authority.IssueCSRF(administrator))
		req := httptest.NewRequest("POST", "/admin/principals", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(administrator, passphrase)
		w := httptest.NewRecorder()
		r.Handler.ServeHTTP(w, req)
		return w
	}

	// A dataset this administration cannot see is refused, so a crafted form
	// cannot name one the administrator was never shown.
	if w := add(url.Values{"person": {"mallory"}, "password": {"synthetic-password-only"}, "dataset": {"s-elsewhere"}, "read": {"yes"}}); !strings.Contains(w.Header().Get("Location"), "unknown-dataset") {
		t.Fatal("an unseen dataset was accepted", w.Header().Get("Location"))
	}
	// An account with nothing granted cannot use the dataset, so it is refused
	// rather than created empty.
	if w := add(url.Values{"person": {"nobody"}, "password": {"synthetic-password-only"}, "dataset": {dataset}}); !strings.Contains(w.Header().Get("Location"), "no-grants") {
		t.Fatal("an account with no grants was created", w.Header().Get("Location"))
	}
	if w := add(url.Values{"person": {"bob"}, "password": {"synthetic-password-only"}, "dataset": {dataset}, "read": {"yes"}, "write": {"yes"}}); !strings.Contains(w.Header().Get("Location"), "added") {
		t.Fatal("a valid account was not added", w.Header().Get("Location"))
	}

	// Added with what was chosen, and nothing more. Erase is the grant that
	// destroys a dataset and it was not on the form.
	for _, entry := range r.Authority.AdminOverview(t.Context(), administrator).Principals {
		if entry.ID != "bob" {
			continue
		}
		g := entry.Datasets[dataset]
		if !g.Read || !g.Write || g.Schema || g.Erase {
			t.Fatal("the account was not created with the grants chosen", g)
		}
	}
	// And the form is single-use: a resubmitted one cannot act twice.
	replay := url.Values{"person": {"chris"}, "password": {"synthetic-password-only"}, "dataset": {dataset}, "read": {"yes"}, "csrf": {"stale"}}
	req := httptest.NewRequest("POST", "/admin/principals", strings.NewReader(replay.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(administrator, passphrase)
	w := httptest.NewRecorder()
	r.Handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("a stale form token was accepted", w.Code)
	}
}

// The whole multi-user flow with no browser consent and no shell: an operator
// creates a dataset and adds a person to it, both from the administration
// surface. Before this, dataset creation was reachable only from an
// authorization carrying erase, which only an interactive consent could mint —
// so a server owner adding people as they arrive had to approve in a browser
// once before anything else worked.
func TestAnOperatorCanCreateADatasetAndPopulateIt(t *testing.T) {
	r := runtime(t)
	original := only(t, r)
	before := len(datasetsOf(t, r))

	post := func(page string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		form.Set("csrf", r.Authority.IssueCSRF(administrator))
		req := httptest.NewRequest("POST", "/admin/"+page, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(administrator, passphrase)
		w := httptest.NewRecorder()
		r.Handler.ServeHTTP(w, req)
		return w
	}

	if w := post("datasets", url.Values{"display_name": {"Second person"}}); !strings.Contains(w.Header().Get("Location"), "created") {
		t.Fatal("an administrator could not create a dataset", w.Code, w.Header().Get("Location"))
	}
	held := datasetsOf(t, r)
	if len(held) != before+1 {
		t.Fatal("creation did not add a dataset", before, len(held))
	}
	var made string
	for _, d := range held {
		if d.DisplayName == "Second person" {
			made = d.ID
		}
	}
	if made == "" {
		t.Fatal("the created dataset is not listed by name")
	}

	// And the other half of the workflow, which was already here: an account
	// on the dataset just made, with the grants chosen and not more.
	if w := post("principals", url.Values{"person": {"dana"}, "password": {"synthetic-password-only"},
		"dataset": {made}, "read": {"yes"}, "write": {"yes"}}); !strings.Contains(w.Header().Get("Location"), "added") {
		t.Fatal("an account could not be added to the created dataset", w.Header().Get("Location"))
	}
	for _, entry := range r.Authority.AdminOverview(t.Context(), administrator).Principals {
		if entry.ID != "dana" {
			continue
		}
		g := entry.Datasets[made]
		if !g.Read || !g.Write || g.Erase || entry.Datasets[original].Read {
			t.Fatal("the account landed on the wrong dataset or with the wrong grants", entry.Datasets)
		}
	}
}

func datasetsOf(t *testing.T, r *standalone.Runtime) []store.Dataset {
	t.Helper()
	held, err := r.Engine.Provisioned(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// A server holds a dataset for each person on it. Administration derived the
// right to administer from holding erase anywhere, and then listed every
// dataset the storage holds — so owning a dataset of your own made you an
// administrator of everybody else's, and one form post was enough to add
// yourself to theirs with every grant.
func TestAnAdministratorReachesOnlyTheDatasetsTheyAdminister(t *testing.T) {
	t.Parallel()
	config := standalone.Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite",
		EncryptedStorage: true}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := standalone.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	mine := only(t, r)
	if err = r.Authority.AddPerson(administrator, passphrase, mine,
		store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	// A second dataset, administered by somebody else and by nobody here.
	owner := store.Access{Principal: "neighbour", Authorization: "setup"}
	if err = r.Engine.SetAuthorization(t.Context(), mine, owner,
		store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	theirs, err := r.Engine.CreateDataset(t.Context(), owner, "Somebody else", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Authority.AddPerson("neighbour", passphrase, theirs.DatasetID,
		store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	// Reopened, because the composition gives administration its read-only
	// handle over every dataset the storage holds at startup. Without this the
	// second dataset is unreachable here for a reason that has nothing to do
	// with who administers it, and the checks below would pass on a server
	// that had simply not noticed it yet.
	r.Close()
	if r, err = standalone.Open(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })

	// Not listed, not describable, and not readable as a schema.
	body := get(t, r, "/admin/datasets", administrator, passphrase).Body.String()
	if strings.Contains(body, theirs.DatasetID) {
		t.Fatal("a dataset this administrator does not administer was listed to them")
	}
	for _, page := range []string{"/admin?dataset=", "/admin/schema?dataset="} {
		if w := get(t, r, page+theirs.DatasetID, administrator, passphrase); w.Code == 200 {
			t.Fatal("another dataset's description was served", page, w.Body.String())
		}
	}

	// And not writable. Naming it on the form is the escalation: the listing
	// the form is checked against was the storage's, not the administrator's.
	form := url.Values{"csrf": {csrf(t, r)}, "person": {administrator}, "dataset": {theirs.DatasetID},
		"read": {"yes"}, "write": {"yes"}, "schema": {"yes"}, "erase": {"yes"}}
	if w := post(t, r, "/admin/principals", form); !strings.Contains(w.Header().Get("Location"), "unknown-dataset") {
		t.Fatal("an administrator added themselves to a dataset they do not administer", w.Header().Get("Location"))
	}
	for _, entry := range r.Authority.AdminOverview(t.Context(), "neighbour").Principals {
		if g := entry.Datasets[theirs.DatasetID]; entry.ID == administrator && (g.Read || g.Write || g.Schema || g.Erase) {
			t.Fatal("the escalation landed: they are now on the other dataset", entry.Datasets)
		}
	}
	// The other half: what the page reports. An administrator of one dataset
	// reading the membership of another is the disclosure that makes the
	// escalation easy to aim.
	for _, entry := range r.Authority.AdminOverview(t.Context(), administrator).Principals {
		if entry.ID == "neighbour" {
			t.Fatal("an account on another dataset was reported to this administrator", entry.Datasets)
		}
	}

	// Nor may they act on credentials belonging to it. The page never offers
	// the authorization, and supplying its identifier anyway is refused.
	if _, err = r.Authority.AddAutomation(t.Context(), "neighbour", theirs.DatasetID); err != nil {
		t.Fatal(err)
	}
	target := ""
	for _, auth := range r.Authority.AdminOverview(t.Context(), "neighbour").Authorizations {
		if auth.Principal == "neighbour" {
			target = auth.ID
		}
	}
	if target == "" {
		t.Fatal("fixture should hold the neighbour's authorization")
	}
	for _, auth := range r.Authority.AdminOverview(t.Context(), administrator).Authorizations {
		if auth.ID == target {
			t.Fatal("another dataset's authorization was shown")
		}
	}
	if w := post(t, r, "/admin/authorizations", url.Values{"csrf": {csrf(t, r)}, "action": {"revoke"}, "authorization": {target}}); w.Code != 400 {
		t.Fatal("an administrator revoked credentials on a dataset they do not administer", w.Code)
	}
}

// An expired grant is not a grant. The engine has read it that way since
// grants could expire; administration read the flag alone.
func TestAnExpiredGrantDoesNotAdminister(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	past := time.Now().Add(-time.Hour)
	if err := r.Authority.GrantDataset(administrator, only(t, r),
		store.Grants{Read: true, Write: true, Schema: true, Erase: true, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if w := get(t, r, "/admin/authorizations", administrator, passphrase); w.Code != 401 {
		t.Fatal("an expired erase grant still opened administration", w.Code)
	}
}

// An administrator confers only a right they hold. Adding an account to a
// dataset needs no password, so otherwise an administrator of one dataset
// could hand any existing account authority over the whole server.
func TestAnAdministratorCannotConferARightTheyDoNotHold(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	if w := post(t, r, "/admin/principals", url.Values{"csrf": {csrf(t, r)}, "person": {"eve"},
		"password": {passphrase}, "dataset": {only(t, r)}, "read": {"yes"}, "create_dataset": {"yes"}}); !strings.Contains(w.Header().Get("Location"), "added") {
		t.Fatal("the account was not added", w.Header().Get("Location"))
	}
	if r.Authority.HoldsCreation("eve") {
		t.Fatal("an administrator without the server right conferred it")
	}
}

// An approval names one dataset and may also carry the right to create.
// Creating installs access on what was created, so an ordinary approval's
// reach outgrows the dataset it names — and withdrawal acts across the whole
// reach, because a withdrawn installation must not keep what it made.
//
// The standing to withdraw therefore has to cover the whole reach. Checking
// only the named dataset let an administrator of one dataset cut an
// installation off from another they have nothing to do with.
func TestWithdrawalNeedsStandingOverEverythingItReaches(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	mine := only(t, r)
	// An installation on this administrator's dataset, holding creation.
	if err := r.Authority.AddPerson("builder", passphrase, mine,
		store.Grants{Read: true, Write: true, Schema: true}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authority.AddAutomation(t.Context(), "builder", mine); err != nil {
		t.Fatal(err)
	}
	target := ""
	for _, auth := range r.Authority.AdminOverview(t.Context(), administrator).Authorizations {
		if auth.Principal == "builder" {
			target = auth.ID
		}
	}
	if target == "" {
		t.Fatal("fixture should offer the installation before it creates anything")
	}
	// It creates a dataset. Nothing about this administrator's dataset
	// changed, and the new one is none of their business.
	made, err := r.Engine.CreateDataset(t.Context(), store.Access{Principal: "builder", Authorization: target},
		"Somewhere else", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range r.Authority.AdminOverview(t.Context(), administrator).Authorizations {
		if auth.ID == target {
			t.Fatal("an approval reaching outside this administrator's datasets was still offered")
		}
	}
	if w := post(t, r, "/admin/authorizations", url.Values{"csrf": {csrf(t, r)}, "action": {"withdraw"},
		"authorization": {target}}); w.Code != 400 {
		t.Fatal("an administrator withdrew an installation's access to a dataset they do not administer", w.Code)
	}
	// The access it created with is still there, which is what the refusal
	// was protecting.
	if _, err = r.Engine.Info(t.Context(), store.Access{Principal: "builder", Authorization: target}, made.DatasetID); err != nil {
		t.Fatal("the refusal did not protect what it was refusing over", err)
	}
	// And an administrator who does cover the whole reach may act.
	if err = r.Authority.GrantDataset(administrator, made.DatasetID,
		store.Grants{Read: true, Write: true, Schema: true, Erase: true}); err != nil {
		t.Fatal(err)
	}
	if w := post(t, r, "/admin/authorizations", url.Values{"csrf": {csrf(t, r)}, "action": {"withdraw"},
		"authorization": {target}}); !strings.Contains(w.Header().Get("Location"), "withdrawn") {
		t.Fatal("an administrator covering the whole reach was refused", w.Code, w.Header().Get("Location"))
	}
	if _, err = r.Engine.Info(t.Context(), store.Access{Principal: "builder", Authorization: target}, made.DatasetID); err == nil {
		t.Fatal("withdrawal left the installation the dataset it created")
	}
}

// A dataset created over the protocol was one the engine knew and the
// deployment did not. The administration handle is given its read over what
// the storage holds at startup, so until the next restart the new dataset was
// invisible here — and nobody was recorded as holding it, so nobody
// administered it either.
func TestADatasetCreatedOverTheProtocolIsAdministrableAtOnce(t *testing.T) {
	t.Parallel()
	config := standalone.Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite",
		EncryptedStorage: true, DatasetCreation: true}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := standalone.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if err = r.Authority.AddPerson(administrator, passphrase, only(t, r),
		store.Grants{Read: true, Write: true, Schema: true, Erase: true}, true); err != nil {
		t.Fatal(err)
	}
	token, err := r.Authority.AddAutomation(t.Context(), administrator, only(t, r))
	if err != nil {
		t.Fatal(err)
	}

	// Created over the wire, by the protocol route, on a running server.
	request := httptest.NewRequest("POST", "/v1/datasets", strings.NewReader(`{"display_name":"Made over the wire"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "7f3a1c52-9b4e-4d21-8a6f-0c5e2d914b73")
	w := httptest.NewRecorder()
	r.Handler.ServeHTTP(w, request)
	if w.Code != 201 {
		t.Fatal("creation over the protocol was refused", w.Code, w.Body.String())
	}
	var created struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(w.Body.Bytes(), &created) != nil || created.DatasetID == "" {
		t.Fatal("no identity in the creation response", w.Body.String())
	}

	// Without a restart: administered by its creator, and listed to them.
	if !r.Authority.Administers(administrator)[created.DatasetID] {
		t.Fatal("nobody administers a dataset created over the protocol")
	}
	if body := get(t, r, "/admin/datasets", administrator, passphrase).Body.String(); !strings.Contains(body, created.DatasetID) {
		t.Fatal("a dataset created over the protocol was not listed until a restart")
	}
	if code := get(t, r, "/admin?dataset="+created.DatasetID, administrator, passphrase).Code; code != 200 {
		t.Fatal("a dataset created over the protocol could not be described", code)
	}
	// And an account can be added to it, which is the workflow the gap broke.
	if resp := post(t, r, "/admin/principals", url.Values{"csrf": {csrf(t, r)}, "person": {"newcomer"},
		"password": {passphrase}, "dataset": {created.DatasetID}, "read": {"yes"}}); !strings.Contains(resp.Header().Get("Location"), "added") {
		t.Fatal("an account could not be added to a dataset created over the protocol", resp.Header().Get("Location"))
	}
}

// The notice a creation sends the deployment is best effort: the dataset is
// already committed when it runs, so its failure cannot unmake it. Listing
// therefore installs the read handle itself for a dataset the administrator
// holds and it cannot see, rather than leaving visibility to depend on a step
// that may not have run.
func TestListingHealsAHandleThatCannotSeeAnAdministeredDataset(t *testing.T) {
	t.Parallel()
	r := runtime(t)
	// Created straight through the engine, so nothing told the deployment.
	owner := store.Access{Principal: administrator, Authorization: "setup"}
	if err := r.Engine.SetAuthorization(t.Context(), only(t, r), owner,
		store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	made, err := r.Engine.CreateDataset(t.Context(), owner, "Unannounced", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Authority.GrantDataset(administrator, made.DatasetID,
		store.Grants{Read: true, Write: true, Schema: true, Erase: true}); err != nil {
		t.Fatal(err)
	}
	if body := get(t, r, "/admin/datasets", administrator, passphrase).Body.String(); !strings.Contains(body, made.DatasetID) {
		t.Fatal("a dataset this administrator holds was not listed to them")
	}
	if code := get(t, r, "/admin?dataset="+made.DatasetID, administrator, passphrase).Code; code != 200 {
		t.Fatal("a dataset this administrator holds could not be described", code)
	}
}
