// Package admin serves the standalone administration surface: server-rendered
// HTML over embedded assets, with no client vocabulary, workflow or approval
// flow in it. It reads the identity registry and the dataset's own description.
// Record values never reach these pages, and neither do secrets.
package admin

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ekarpov/tessera/internal/acquisition"
	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

//go:embed assets
var assets embed.FS

// The capability table is read from the protocol rather than copied here, so
// that a capability added or retired there appears on this page without this
// file being edited to match. The page lists every advertisable one, so that a
// withheld capability is visible as a decision rather than as an absence
// nobody noticed.
//
// The copy that used to live here is why: it kept `aggregation`,
// `distinct_references` and `text_matching` after they became required
// operations, so this page reported three operations the protocol no longer
// lets a deployment decline as things this deployment had declined. A name the
// protocol has withdrawn is not a capability with a false value — it is a name
// that no longer answers the question, and `retired` below says so in the one
// place an operator would otherwise ask.

// Handler renders the administration pages. access is an internal handle the
// composition provisions for reading the dataset's description and schema: it
// is not a credential, because no token maps to it and none can be issued for
// it. It carries read only.
type Handler struct {
	authority *identity.Authority
	records   store.Store
	access    store.Access
	engine    string
	pages     *template.Template
	style     []byte
	// provisioning is nil where the engine cannot make a dataset, which is the
	// only reason this surface would not offer it. Whether the *protocol*
	// offers creation is a separate question answered by the deployment: an
	// operator administering a server may create one there whether or not
	// callers may create one over the wire.
	provisioning store.Provisioning
	// backup takes a backup on demand; nil where the composition has none.
	backup         func(context.Context) (string, error)
	acquisitions   *acquisition.Manager
	backupInstance string
}

// grantInstaller is the engine's grant administration, which the record store
// interface does not carry. Administration needs it for one thing: a dataset
// created here is created by this handle, and creation installs full access
// for its creator, which this handle is documented not to hold.
type grantInstaller interface {
	SetAuthorization(context.Context, string, store.Access, store.Authorization) error
}

func New(authority *identity.Authority, records store.Store, access store.Access, engine string) (*Handler, error) {
	provisioning, _ := records.(store.Provisioning)
	if authority == nil || records == nil || !access.Valid() {
		return nil, store.Refuse("internal_error")
	}
	style, err := assets.ReadFile("assets/admin.css")
	if err != nil {
		return nil, err
	}
	h := &Handler{authority: authority, records: records, access: access, engine: engine, style: style, provisioning: provisioning, backupInstance: acquisition.NewInstanceID()}
	h.pages = template.New("pages")
	if _, err = h.pages.ParseFS(assets, "assets/layout.html"); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", h.serve)
	mux.HandleFunc("GET /admin/{page}", h.serve)
	// The wildcard has to be in the pattern for {page} to bind; a literal
	// POST route would arrive with no page and render the default one.
	mux.HandleFunc("POST /admin/{page}", h.serve)
}

// render parses the layout together with one page's content each time, so a
// template cannot leak state between requests.
func (h *Handler) render(w http.ResponseWriter, page, title, message string, data map[string]any) {
	h.renderStatus(w, 200, page, title, message, data)
}

func (h *Handler) renderStatus(w http.ResponseWriter, status int, page, title, message string, data map[string]any) {
	pages, err := h.pages.Clone()
	if err == nil {
		_, err = pages.ParseFS(assets, "assets/"+page+".html")
	}
	if err != nil {
		http.Error(w, "Administration page unavailable", 500)
		return
	}
	data["Page"], data["Title"], data["Message"] = page, title, message
	// Rendered whole before anything is written. Executing straight to the
	// response sends 200 and then discards any error, so a broken template
	// serves a truncated page under a success status — which is exactly what a
	// status-only test cannot see, and what one did not see here.
	var body bytes.Buffer
	if err = pages.ExecuteTemplate(&body, "layout", data); err != nil {
		http.Error(w, "Administration page unavailable", 500)
		return
	}
	secure(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}

// unavailable separates a dataset this administrator may not name from a
// storage that could not answer. Reporting the first as 503 tells an operator
// to wait for a condition that will never change, and tells them a dataset
// they may not see is having trouble.
func unavailable(w http.ResponseWriter, err error, failed string) {
	switch p.ErrorCode(err) {
	case "dataset_unavailable", "unavailable", "not_found":
		http.Error(w, "No such dataset", 404)
	default:
		http.Error(w, failed, 503)
	}
}

func secure(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Embedded assets only, no scripts, and no cross-origin form posts.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	page := r.PathValue("page")
	if page == "" {
		page = "overview"
	}
	// The stylesheet is served before authentication: it is a static asset with
	// nothing in it about this deployment, and a 401 on it would only make the
	// authenticated page render unstyled.
	if page == "admin.css" {
		secure(w)
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(h.style)
		return
	}
	// Rate limiting comes before authentication on purpose: verifying a
	// password costs a deliberate 600k-iteration derivation, and an
	// unauthenticated caller must not be able to spend it freely.
	if h.authority.AdminRateLimited(r, "admin", 30) {
		http.Error(w, "Too many administration requests", 429)
		return
	}
	id, secret, supplied := r.BasicAuth()
	if !supplied || !h.authority.AuthenticateAdministrator(id, secret) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Tessera administration", charset="UTF-8"`)
		secure(w)
		http.Error(w, "Administrator authentication required", 401)
		return
	}
	// Only pages that define an action accept one, so a POST to a reporting
	// page is a method error rather than a silently ignored request.
	if r.Method == "POST" && page != "authorizations" && page != "principals" && page != "datasets" {
		secure(w)
		http.Error(w, "That administration page takes no action", 405)
		return
	}
	switch {
	case page == "authorizations" && r.Method == "POST":
		h.act(w, r, id)
	case page == "principals" && r.Method == "POST" && r.PostFormValue("action") != "":
		h.accountAction(w, r, id)
	case page == "principals" && r.Method == "POST":
		h.addPerson(w, r, id)
	case page == "datasets" && r.Method == "POST":
		h.addDataset(w, r, id)
	case page == "datasets":
		held, err := h.datasets(r, id)
		if err != nil {
			secure(w)
			http.Error(w, "Dataset listing unavailable", 503)
			return
		}
		h.render(w, "datasets", "Datasets", outcomes[r.URL.Query().Get("done")],
			map[string]any{"Datasets": held, "CSRF": h.authority.IssueCSRF(id),
				"CanCreate": h.provisioning != nil})
	case page == "overview":
		h.overview(w, r, id)
	case page == "principals":
		overview := h.authority.AdminOverview(r.Context(), id)
		held, err := h.datasets(r, id)
		if err != nil {
			secure(w)
			http.Error(w, "Dataset listing unavailable", 503)
			return
		}
		h.principals(w, r, id, 200, outcomes[r.URL.Query().Get("done")], overview, held)
	case page == "authorizations":
		overview := h.authority.AdminOverview(r.Context(), id)
		h.render(w, "authorizations", "Authorizations", outcomes[r.URL.Query().Get("done")],
			map[string]any{"Authorizations": overview.Authorizations, "CSRF": h.authority.IssueCSRF(id)})
	case page == "schema":
		h.schema(w, r, id)
	default:
		secure(w)
		http.Error(w, "No such administration page", 404)
	}
}

// outcomes maps the codes an action redirects with to the sentence the page
// shows. The message never travels in the URL, so a crafted link cannot put
// wording of its own onto an administrator's page.
var outcomes = map[string]string{
	"revoked":   "Credentials revoked.",
	"withdrawn": "Dataset grants withdrawn; the credentials still authenticate.",
	"shared":    "Read access granted.",
	"added":     "Account added to that dataset with the grants chosen.",
	// A rejection says what was refused and not why: the authority refuses an
	// existing non-person, a short password and an over-long identifier the
	// same way, and naming which would tell a form which accounts exist.
	"rejected":        "That account could not be added. Check the identifier and that the password is at least twelve characters.",
	"no-grants":       "No grants were chosen, so nothing was added. An account with no grants cannot use the dataset.",
	"unknown-dataset": "That dataset is not one this administration can see.",
	"suspended":       "Account suspended. Every credential it holds stopped authenticating and is kept for reinstatement.",
	"reinstated":      "Account reinstated. The credentials it held authenticate again.",
	"deleted":         "Account deleted. Its credentials are gone and its name will not be issued again.",
	"password-reset":  "Password changed. The account's existing sign-ins keep working; suspend it if you suspect a compromise.",
	// One message for every refusal, because which reason applied would tell
	// this form things about accounts the administrator cannot see.
	"account-refused": "That action was refused. You may act only on accounts whose every dataset you administer, never on your own; setting a password takes administering every dataset the account holds; the server's last administrator cannot be deleted; and a new password must be at least twelve characters.",
}

type named struct {
	Name  string
	Value string
}
type capability struct {
	Name       string
	Advertised bool
}

// dataset resolves the one dataset these pages administer. The engine may hold
// more than one; choosing between them is a surface this page does not have, so
// anything other than exactly one is reported as unavailable rather than picked
// from arbitrarily.
// dataset is the one a dataset-scoped page is describing. A server holds as
// many as it is given, so the page says which — `?dataset=` when the request
// names one, and otherwise the first this administration may read, in the
// listing's own order so the choice is stable between requests.
//
// It refuses rather than guessing when a named dataset is not one of them: an
// administration page that quietly described a different dataset than the one
// asked for would be worse than an error.
func (h *Handler) dataset(r *http.Request, administrator string) (store.Info, error) {
	held, err := h.datasets(r, administrator)
	if err != nil {
		return store.Info{}, err
	}
	if len(held) == 0 {
		return store.Info{}, store.Refuse("dataset_unavailable")
	}
	named := r.URL.Query().Get("dataset")
	if named == "" {
		return held[0], nil
	}
	for _, info := range held {
		if info.ID == named {
			return info, nil
		}
	}
	return store.Info{}, store.Refuse("dataset_unavailable")
}

// datasets is every dataset this administration may read, which is what the
// selector on a dataset-scoped page offers.
// datasets lists what this administrator administers, which is not what the
// storage holds. The handle below reads every dataset the server keeps — it is
// the composition's, not the administrator's — so filtering it here is what
// makes the dataset named in a query string or a form theirs to name. Every
// page and every action goes through this one listing for that reason.
func (h *Handler) datasets(r *http.Request, administrator string) ([]store.Info, error) {
	held, err := h.records.Datasets(r.Context(), h.access)
	if err != nil {
		return nil, err
	}
	scope := h.authority.Administers(administrator)
	within := make([]store.Info, 0, len(held))
	seen := map[string]bool{}
	for _, info := range held {
		if scope[info.ID] {
			within = append(within, info)
			seen[info.ID] = true
		}
	}
	// A dataset this administrator holds that the handle above cannot read is
	// one created since this server started: the composition gives the handle
	// its read over what the storage held at startup, and over each dataset
	// as it is made. Installing it here as well means visibility does not
	// depend on that having succeeded, and it grants nothing that is not
	// already this administrator's to see.
	installer, canInstall := h.records.(grantInstaller)
	for dataset := range scope {
		if seen[dataset] || !canInstall {
			continue
		}
		// One dataset that cannot be reached is left out rather than failing
		// the whole listing: an erased dataset's old identity reads this way,
		// and one person erasing theirs used to blank every administration
		// page on the server.
		if e := installer.SetAuthorization(r.Context(), dataset, h.access,
			store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}); e != nil {
			continue
		}
		info, e := h.records.Info(r.Context(), h.access, dataset)
		if e != nil {
			// The registry says they hold it and the storage does not have
			// it. A retired dataset reads this way, and it is not an error
			// for a page that is listing what can be administered.
			continue
		}
		within = append(within, info)
	}
	sort.Slice(within, func(i, j int) bool { return within[i].ID < within[j].ID })
	return within, nil
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request, administrator string) {
	info, err := h.dataset(r, administrator)
	if err != nil {
		secure(w)
		unavailable(w, err, "Dataset description unavailable")
		return
	}
	advertised := map[string]bool{}
	for _, name := range info.Capabilities {
		advertised[name] = true
	}
	names := p.DatasetCapabilities()
	capabilities := make([]capability, 0, len(names))
	for _, name := range names {
		capabilities = append(capabilities, capability{Name: name, Advertised: advertised[name]})
	}
	types := 0
	if err = h.records.Transact(r.Context(), h.access, info.ID, func(tx store.Tx) error {
		list, e := tx.ListMetadata()
		types = len(list)
		return e
	}); err != nil {
		secure(w)
		http.Error(w, "Dataset description unavailable", 503)
		return
	}
	// How full the dataset is, beside the bound that stops it accepting
	// writes. Shown so an administrator sees a dataset filling rather than
	// learning from a refused write; absent where the engine cannot measure.
	var usage *store.Usage
	if reader, ok := h.records.(store.UsageReader); ok {
		if measured, e := reader.DatasetUsage(r.Context(), info.ID); e == nil {
			usage = &measured
		}
	}
	h.render(w, "overview", "Overview", "", map[string]any{"Dataset": info, "Engine": h.engine,
		"Capabilities": capabilities, "Retired": p.RetiredReadCapabilities,
		"Limits": bounds(info.Limits), "TypeCount": types, "Usage": usage})
}

// bounds reads the declared limits through their wire names, so a limit added
// to the protocol appears here without this page being edited to match.
func bounds(limits p.Limits) []named {
	value, kind := reflect.ValueOf(limits), reflect.TypeOf(limits)
	result := make([]named, 0, kind.NumField())
	for i := 0; i < kind.NumField(); i++ {
		name, _, _ := strings.Cut(kind.Field(i).Tag.Get("json"), ",")
		if name == "" {
			continue
		}
		result = append(result, named{Name: name, Value: strconv.FormatInt(value.Field(i).Int(), 10)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

type schemaType struct {
	p.Meta
	AnnotationKeys []string
}

func (h *Handler) schema(w http.ResponseWriter, r *http.Request, administrator string) {
	info, err := h.dataset(r, administrator)
	if err != nil {
		secure(w)
		unavailable(w, err, "Dataset description unavailable")
		return
	}
	var types []schemaType
	if err := h.records.Transact(r.Context(), h.access, info.ID, func(tx store.Tx) error {
		list, e := tx.ListMetadata()
		if e != nil {
			return e
		}
		for _, meta := range list {
			// Annotation values are opaque bytes that may hold anything a
			// client put there; only their keys are shown.
			keys := make([]string, 0, len(meta.Annotations))
			for key := range meta.Annotations {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			types = append(types, schemaType{Meta: meta, AnnotationKeys: keys})
		}
		return nil
	}); err != nil {
		secure(w)
		http.Error(w, "Schema unavailable", 503)
		return
	}
	sort.Slice(types, func(i, j int) bool { return types[i].ID < types[j].ID })
	h.render(w, "schema", "Schema", "", map[string]any{"Types": types})
}

func (h *Handler) act(w http.ResponseWriter, r *http.Request, administrator string) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil {
		secure(w)
		http.Error(w, "Malformed administration request", 400)
		return
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			secure(w)
			http.Error(w, "Malformed administration request", 400)
			return
		}
	}
	// Every mutation spends a single-use token bound to this administrator, so
	// a resubmitted form cannot act twice and no other origin can act at all.
	if !h.authority.ConsumeCSRF(administrator, r.PostForm.Get("csrf")) {
		secure(w)
		http.Error(w, "Administration form expired, reload and retry", 403)
		return
	}
	target := r.PostForm.Get("authorization")
	// The page shows only authorizations within this administrator's datasets,
	// and this refuses any other by the same rule. A form is an identifier a
	// caller supplies, so what was rendered is no part of what is accepted.
	may, err := h.authority.MayAdminister(r.Context(), administrator, target)
	if err != nil {
		secure(w)
		http.Error(w, "Administration action could not be completed", 503)
		return
	}
	if !may {
		secure(w)
		http.Error(w, "Administration action refused: not_found", 400)
		return
	}
	var done string
	switch r.PostForm.Get("action") {
	case "revoke":
		err, done = h.authority.RevokeAuthorization(r.Context(), target), "revoked"
	case "withdraw":
		err, done = h.authority.WithdrawGrants(r.Context(), target), "withdrawn"
	case "share":
		var expiry *time.Time
		if value := r.PostForm.Get("expires_at"); value != "" {
			when, e := time.Parse(time.RFC3339Nano, value)
			if e != nil {
				secure(w)
				http.Error(w, "Expiry must be an RFC 3339 UTC timestamp", 400)
				return
			}
			expiry = &when
		}
		types := strings.Split(r.PostForm.Get("types"), ",")
		for i := range types {
			types[i] = strings.TrimSpace(types[i])
		}
		err, done = h.authority.GrantRead(r.Context(), target, types, expiry), "shared"
	default:
		secure(w)
		http.Error(w, "Unknown administration action", 400)
		return
	}
	if err != nil {
		secure(w)
		// The refusal code is the whole diagnostic: identifiers the request
		// carried are not echoed back into the page.
		switch p.ErrorCode(err) {
		case "not_found", "conflict", "invalid_field":
			http.Error(w, "Administration action refused: "+p.ErrorCode(err), 400)
		default:
			http.Error(w, "Administration action could not be completed", 503)
		}
		return
	}
	secure(w)
	http.Redirect(w, r, "/admin/authorizations?"+url.Values{"done": {done}}.Encode(), 303)
}

// addPerson provisions an account from the administration surface. Until now
// this existed only as a CLI flag, so a web front end could define types
// through the protocol and still not create the person who would use them.
//
// It takes the dataset and the grants explicitly for the reason the CLI does:
// a grant set nobody chose is how everybody ends up able to erase everything.
func (h *Handler) addPerson(w http.ResponseWriter, r *http.Request, administrator string) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil {
		secure(w)
		http.Error(w, "Malformed administration request", 400)
		return
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			secure(w)
			http.Error(w, "Malformed administration request", 400)
			return
		}
	}
	if !h.authority.ConsumeCSRF(administrator, r.PostForm.Get("csrf")) {
		secure(w)
		http.Error(w, "Administration form expired, reload and retry", 403)
		return
	}
	// The dataset has to be one this administration can see, so a crafted form
	// cannot name a dataset the administrator was never shown.
	held, err := h.datasets(r, administrator)
	if err != nil {
		secure(w)
		http.Error(w, "Dataset listing unavailable", 503)
		return
	}
	dataset, known := r.PostForm.Get("dataset"), false
	for _, info := range held {
		if info.ID == dataset {
			known = true
		}
	}
	grants := store.Grants{
		Read:   r.PostForm.Get("read") == "yes",
		Write:  r.PostForm.Get("write") == "yes",
		Schema: r.PostForm.Get("schema") == "yes",
		Erase:  r.PostForm.Get("erase") == "yes",
	}
	done := "added"
	switch {
	case !known:
		done = "unknown-dataset"
	case !grants.Read && !grants.Write && !grants.Schema && !grants.Erase:
		done = "no-grants"
	default:
		// The server right is conferred only by an administrator who holds it.
		// Adding an account to a dataset needs no password, so without this an
		// administrator of one dataset could hand any existing account a right
		// over the whole server — one they need not hold themselves. It is the
		// rule every other grant here already follows: never more than the
		// granter has.
		create := r.PostForm.Get("create_dataset") == "yes" && h.authority.HoldsCreation(administrator)
		if err = h.authority.AddPerson(r.PostForm.Get("person"), r.PostForm.Get("password"), dataset, grants, create); err != nil {
			done = "rejected"
		}
	}
	secure(w)
	http.Redirect(w, r, "/admin/principals?done="+done, http.StatusSeeOther)
}

// addDataset creates a dataset from the administration surface, which is where
// a server operator stands. Creation existed only through an interactive
// consent flow carrying erase, so an owner adding people as they arrive had to
// approve in a browser once before anything else worked — and person creation
// landed here at alpha.3 while dataset creation did not, leaving one workflow
// stopped halfway.
func (h *Handler) addDataset(w http.ResponseWriter, r *http.Request, administrator string) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil {
		secure(w)
		http.Error(w, "Malformed administration request", 400)
		return
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			secure(w)
			http.Error(w, "Malformed administration request", 400)
			return
		}
	}
	if !h.authority.ConsumeCSRF(administrator, r.PostForm.Get("csrf")) {
		secure(w)
		http.Error(w, "Administration form expired, reload and retry", 403)
		return
	}
	done := "created"
	switch {
	case h.provisioning == nil:
		done = "cannot-create"
	default:
		// Administration acts as itself rather than as the administrator: this
		// is the composition creating a dataset, not a caller exercising a
		// right, so it takes no idempotency key and mints no authorization.
		made, err := h.provisioning.CreateDataset(r.Context(), h.access, r.PostForm.Get("display_name"), p.Limits{}, nil, "", [32]byte{})
		switch {
		case err != nil:
			done = "create-refused"
		default:
			// Creation installs full access for whoever created it, and here
			// that is this handle — which is documented to carry read only and
			// is relied on for that everywhere else. Put it back to read.
			installer, ok := h.records.(grantInstaller)
			if !ok {
				done = "create-refused"
				break
			}
			if err = installer.SetAuthorization(r.Context(), made.DatasetID, h.access,
				store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}); err != nil {
				done = "create-refused"
				break
			}
			// The operator who made it holds it. Administration is per
			// dataset, so without this the dataset would exist with no member
			// and nobody — including its creator — able to add one.
			if err = h.authority.RecordCreation(administrator, made.DatasetID,
				store.Grants{Read: true, Write: true, Schema: true, Erase: true}); err != nil {
				done = "create-refused"
			}
		}
	}
	secure(w)
	http.Redirect(w, r, "/admin/datasets?done="+done, http.StatusSeeOther)
}

// accountAction suspends, reinstates or deletes an account.
//
// Standing is decided by the authority, not by what this page rendered: a
// form is an identifier a caller supplies. An administrator may act only on an
// account whose every dataset they administer — administering one dataset a
// person belongs to is not standing to remove them from the others — and never
// on their own, which would leave no way back through this surface.
// principals renders the principals page.
func (h *Handler) principals(w http.ResponseWriter, r *http.Request, id string, status int, message string, overview identity.AdminOverview, held []store.Info) {
	h.renderStatus(w, status, "principals", "Principals", message,
		map[string]any{"Principals": overview.Principals, "Clients": overview.Clients,
			"Datasets": held, "CSRF": h.authority.IssueCSRF(id)})
}

func (h *Handler) accountAction(w http.ResponseWriter, r *http.Request, administrator string) {
	for _, values := range r.PostForm {
		if len(values) != 1 {
			secure(w)
			http.Error(w, "Malformed administration request", 400)
			return
		}
	}
	if !h.authority.ConsumeCSRF(administrator, r.PostForm.Get("csrf")) {
		secure(w)
		http.Error(w, "Administration form expired, reload and retry", 403)
		return
	}
	account := r.PostForm.Get("account")
	done := "account-refused"
	if h.authority.MayAdministerAccount(administrator, account) {
		var err error
		switch r.PostForm.Get("action") {
		case "suspend":
			err, done = h.authority.SuspendAccount(account), "suspended"
		case "reinstate":
			err, done = h.authority.ReinstateAccount(account), "reinstated"
		case "delete":
			// The datasets it erases are named in the form, exactly as the
			// command line and the API take them; a deletion that would
			// erase anything else is refused below, naming what to enter.
			erase := strings.FieldsFunc(r.PostForm.Get("erase"), func(c rune) bool { return c == ',' || c == ' ' || c == '\n' })
			_, err = h.authority.DeleteAccount(r.Context(), account, erase)
			done = "deleted"
			var refusal *p.Error
			if errors.As(err, &refusal) && refusal.Details["reason"] == "erasure_required" {
				named, _ := refusal.Details["datasets"].([]string)
				held, e := h.datasets(r, administrator)
				if e != nil {
					secure(w)
					http.Error(w, "Dataset listing unavailable", 503)
					return
				}
				message := "Deleting " + account + " erases no dataset, but what was entered to erase does not match. Leave it empty and delete again."
				if len(named) > 0 {
					message = "Deleting " + account + " erases " + strings.Join(named, ", ") + ", with everything in them: it is the last administrator of each. To go ahead, delete again and enter exactly: " + strings.Join(named, ",")
				}
				h.principals(w, r, administrator, 409, message, h.authority.AdminOverview(r.Context(), administrator), held)
				return
			}
		case "reset-password":
			if !h.authority.MayResetPassword(administrator, account) {
				err = store.Refuse("invalid_field")
				break
			}
			err, done = h.authority.ResetPassword(account, r.PostForm.Get("password"), administrator), "password-reset"
		default:
			err = store.Refuse("invalid_field")
		}
		if err != nil {
			if code := p.ErrorCode(err); code != "conflict" && code != "not_found" && code != "invalid_field" {
				secure(w)
				http.Error(w, "Administration action could not be completed", 503)
				return
			}
			done = "account-refused"
		}
	}
	secure(w)
	http.Redirect(w, r, "/admin/principals?done="+done, http.StatusSeeOther)
}
