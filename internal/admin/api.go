package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// The administration API: what the administration pages do, as JSON, for a
// product with its own interface.
//
// It holds no credential of its own. A request carries the bearer token of an
// installation the administrator approved with the separate "administer" right,
// and acts as that administrator with exactly their standing — the same rules
// the pages apply, through the same authority methods, so the two surfaces
// cannot disagree about who may do what. There is no CSRF token because there
// is no cookie: a bearer token is not sent by a browser on its own.

// limitsAdministrator is the engine's per-dataset limits, which the record
// store interface does not carry.
type renamer interface {
	RenameDataset(ctx context.Context, dataset, displayName string) error
}

type limitsAdministrator interface {
	DatasetLimits(ctx context.Context, dataset string) (p.Limits, error)
	SetDatasetLimits(ctx context.Context, dataset string, limits p.Limits) error
}

// SetBackup installs how a server administrator takes a backup on demand.
// Backups belong to the composition, which knows where they are kept.
func (h *Handler) SetBackup(backup func(context.Context) (string, error)) { h.backup = backup }

// APIRoutes registers the administration API.
func (h *Handler) APIRoutes(mux *http.ServeMux) {
	h.acquisitionRoutes(mux)
	mux.HandleFunc("GET /admin/api/v1/overview", h.api(h.apiOverview))
	mux.HandleFunc("POST /admin/api/v1/accounts", h.api(h.apiAddAccount))
	mux.HandleFunc("POST /admin/api/v1/accounts/{account}/{action}", h.api(h.apiAccountAction))
	mux.HandleFunc("GET /admin/api/v1/accounts/{account}/installations", h.api(h.apiInstallations))
	mux.HandleFunc("DELETE /admin/api/v1/accounts/{account}/installations/{installation}", h.api(h.apiRemoveInstallation))
	mux.HandleFunc("GET /admin/api/v1/datasets/{dataset}/readers", h.api(h.apiReaders))
	mux.HandleFunc("GET /admin/api/v1/datasets/{dataset}/readers/{account}", h.api(h.apiReader))
	mux.HandleFunc("PUT /admin/api/v1/datasets/{dataset}/readers/{account}", h.api(h.apiSetReader))
	mux.HandleFunc("DELETE /admin/api/v1/datasets/{dataset}/readers/{account}", h.api(h.apiRemoveReader))
	mux.HandleFunc("POST /admin/api/v1/datasets/{dataset}/readers/{action}", h.api(h.apiFreezeReader))
	mux.HandleFunc("GET /admin/api/v1/datasets/{dataset}/contributors", h.api(h.apiContributors))
	mux.HandleFunc("GET /admin/api/v1/datasets/{dataset}/contributors/{account}", h.api(h.apiContributor))
	mux.HandleFunc("PUT /admin/api/v1/datasets/{dataset}/contributors/{account}", h.api(h.apiSetContributor))
	mux.HandleFunc("DELETE /admin/api/v1/datasets/{dataset}/contributors/{account}", h.api(h.apiRemoveContributor))
	mux.HandleFunc("PUT /admin/api/v1/datasets/{dataset}/limits", h.api(h.apiSetLimits))
	mux.HandleFunc("PUT /admin/api/v1/datasets/{dataset}/name", h.api(h.apiRename))
	mux.HandleFunc("POST /admin/api/v1/issuers", h.api(h.apiAddIssuer))
	mux.HandleFunc("GET /admin/api/v1/issuers", h.api(h.apiIssuers))
	mux.HandleFunc("PATCH /admin/api/v1/issuers/{issuer}", h.api(h.apiChangeIssuer))
	mux.HandleFunc("POST /admin/api/v1/issuers/{issuer}/secret", h.api(h.apiRotateIssuer))
	mux.HandleFunc("POST /admin/api/v1/issuers/{issuer}/invitations", h.api(h.apiIssueInvitation))
	mux.HandleFunc("DELETE /admin/api/v1/invitations", h.api(h.apiEndInvitation))
	mux.HandleFunc("POST /admin/api/v1/unshare", h.api(h.apiUnshare))
	mux.HandleFunc("POST /admin/api/v1/services/{account}/secret", h.api(h.apiRotateService))
	mux.HandleFunc("POST /admin/api/v1/backups", h.api(h.apiBackup))
	// Anything else under the API answers in the API's own shape, not the
	// router's plain-text page, so a client can tell a wrong route from a
	// refusal without parsing prose.
	mux.HandleFunc("/admin/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		apiFailure(w, store.Refuse("not_found"))
	})
}

type apiHandler func(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error)

func (h *Handler) api(handle apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fields := strings.Fields(r.Header.Get("Authorization"))
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
			apiFailure(w, store.Refuse("authentication_failed"))
			return
		}
		access, err := h.authority.AdministrationAccess(r.Context(), fields[1])
		if err != nil {
			apiFailure(w, err)
			return
		}
		administrator := access.Principal
		r = r.WithContext(context.WithValue(r.Context(), installationKey{}, access))
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			apiFailure(w, store.Refuse("invalid_request"))
			return
		}
		status, result, err := handle(w, r, administrator, body)
		if err != nil {
			apiFailure(w, err)
			return
		}
		if status == 0 {
			return
		} // the artifact handler already streamed the response
		w.WriteHeader(status)
		if result != nil {
			_ = json.NewEncoder(w).Encode(result)
		}
	}
}

// installationKey carries the administering installation to a handler whose
// action must stay tied to it rather than only to the account.
type installationKey struct{}

// apiFailure answers in the protocol's error shape. A code the authority did
// not produce is reported as an internal error rather than echoed, so nothing
// a request carried comes back in a message.
func apiFailure(w http.ResponseWriter, err error) {
	code := p.ErrorCode(err)
	answer := map[string]any{"code": code}
	// Details are the server's own — which bound, which datasets — and never
	// an echo of the request.
	var refusal *p.Error
	if errors.As(err, &refusal) && len(refusal.Details) > 0 {
		answer["details"] = refusal.Details
	}
	w.WriteHeader(p.ErrorStatus(code))
	_ = json.NewEncoder(w).Encode(map[string]any{"error": answer})
}

// decode reads a request body, refusing a member the route does not know
// rather than ignoring it. The refusal says which of the two went wrong, so a
// caller is not left guessing whether it misspelled a member or sent JSON
// that does not parse; it never echoes what was sent.
func decode(body []byte, into any) error {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field") {
			return store.RefuseWith("invalid_request", "unknown_member", nil)
		}
		return store.RefuseWith("invalid_request", "malformed_body", nil)
	}
	if decoder.More() {
		return store.RefuseWith("invalid_request", "malformed_body", nil)
	}
	return nil
}

func (h *Handler) apiOverview(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	held, err := h.datasets(r, administrator)
	if err != nil {
		return 0, nil, store.Refuse("dataset_unavailable")
	}
	reader, measures := h.records.(store.UsageReader)
	type dataset struct {
		ID          string       `json:"id"`
		DisplayName string       `json:"display_name"`
		Usage       *store.Usage `json:"usage,omitempty"`
	}
	datasets := make([]dataset, 0, len(held))
	for _, info := range held {
		entry := dataset{ID: info.ID, DisplayName: info.DisplayName}
		if measures {
			if usage, e := reader.DatasetUsage(r.Context(), info.ID); e == nil {
				entry.Usage = &usage
			}
		}
		datasets = append(datasets, entry)
	}
	type account struct {
		Account     string                  `json:"account"`
		Kind        string                  `json:"kind"`
		Suspended   bool                    `json:"suspended"`
		Datasets    map[string]store.Grants `json:"datasets"`
		CreatedAt   *string                 `json:"created_at"`
		SuspendedAt *string                 `json:"suspended_at"`
		InvitedBy   string                  `json:"invited_by,omitempty"`
		Rebindable  bool                    `json:"rebindable"`
	}
	accounts := []account{}
	for _, principal := range h.authority.AdminOverview(r.Context(), administrator).Principals {
		accounts = append(accounts, account{Account: principal.ID, Kind: principal.Kind, Suspended: principal.Suspended, Datasets: principal.Datasets,
			CreatedAt: unixTime(principal.CreatedAt), SuspendedAt: unixTime(principal.SuspendedAt), InvitedBy: principal.InvitedBy,
			Rebindable: h.authority.RebindableAs(r.Context().Value(installationKey{}).(store.Access), principal.ID)})
	}
	answer := map[string]any{"administrator": administrator, "server_administrator": h.authority.AdministersServer(administrator),
		"datasets": datasets, "accounts": accounts}
	// When the last backup was taken is the server's, and told to whoever
	// administers the server.
	if h.authority.AdministersServer(administrator) {
		var last *string
		if at := h.authority.LastBackup(); !at.IsZero() {
			last = unixTime(at.Unix())
		}
		answer["server"] = map[string]any{"last_backup_at": last}
	}
	return 200, answer, nil
}

// unixTime renders Unix seconds as RFC 3339, and zero — not known — as null.
func unixTime(seconds int64) *string {
	if seconds == 0 {
		return nil
	}
	formatted := time.Unix(seconds, 0).UTC().Format(time.RFC3339)
	return &formatted
}

// apiEndInvitation withdraws one invitation nobody has redeemed. The code is
// in the body rather than the path, because it is a secret.
func (h *Handler) apiEndInvitation(w http.ResponseWriter, r *http.Request, _ string, body []byte) (int, any, error) {
	var request struct {
		Invitation string `json:"invitation"`
	}
	if err := decode(body, &request); err != nil {
		return 0, nil, err
	}
	if request.Invitation == "" {
		return 0, nil, store.Refuse("invalid_field")
	}
	access, _ := r.Context().Value(installationKey{}).(store.Access)
	if err := h.authority.EndInvitationAs(access, request.Invitation); err != nil {
		return 0, nil, err
	}
	return 204, nil, nil
}

// apiUnshare stops the named types being shared from every dataset this
// administrator administers, deleting nothing; with dry_run it says what
// would change.
func (h *Handler) apiUnshare(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	var request struct {
		Types    []string `json:"types"`
		Datasets []string `json:"datasets"`
		DryRun   bool     `json:"dry_run"`
	}
	if err := decode(body, &request); err != nil {
		return 0, nil, err
	}
	changed, err := h.authority.Unshare(r.Context(), administrator, request.Types, request.Datasets, request.DryRun)
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"datasets": changed, "dry_run": request.DryRun}, nil
}

// grantsFrom reads grants the way the protocol names them.
type grantRequest struct {
	Read   bool `json:"read"`
	Write  bool `json:"write"`
	Schema bool `json:"schema"`
	Erase  bool `json:"erase"`
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	var request struct {
		Account       string       `json:"account"`
		Password      string       `json:"password"`
		Dataset       string       `json:"dataset"`
		Grants        grantRequest `json:"grants"`
		CreateDataset bool         `json:"create_dataset"`
	}
	if err := decode(body, &request); err != nil {
		return 0, nil, err
	}
	// The dataset must be one this administrator administers — the rule the
	// page applies, through the same listing.
	held, err := h.datasets(r, administrator)
	if err != nil {
		return 0, nil, store.Refuse("dataset_unavailable")
	}
	known := false
	for _, info := range held {
		known = known || info.ID == request.Dataset
	}
	if !known {
		return 0, nil, store.Refuse("permission_denied")
	}
	grants := store.Grants{Read: request.Grants.Read, Write: request.Grants.Write, Schema: request.Grants.Schema, Erase: request.Grants.Erase}
	if !(grants.Read || grants.Write || grants.Schema || grants.Erase) {
		return 0, nil, store.Refuse("invalid_field")
	}
	// Never more than the granter has, for the server right as for the page.
	create := request.CreateDataset && h.authority.HoldsCreation(administrator)
	if err = h.authority.AddPerson(request.Account, request.Password, request.Dataset, grants, create); err != nil {
		return 0, nil, store.Refuse("invalid_field")
	}
	return 201, map[string]any{"account": request.Account}, nil
}

func (h *Handler) apiAccountAction(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	account := r.PathValue("account")
	if !h.authority.MayAdministerAccount(administrator, account) {
		return 0, nil, store.Refuse("permission_denied")
	}
	var err error
	switch r.PathValue("action") {
	case "suspend":
		err = h.authority.SuspendAccount(account)
	case "reinstate":
		err = h.authority.ReinstateAccount(account)
	case "delete":
		// A body is optional; it names the datasets the deletion erases, which
		// must be exactly those the account is the last administrator of.
		var request struct {
			Erase []string `json:"erase"`
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			if err = decode(body, &request); err != nil {
				return 0, nil, err
			}
		}
		kept, err := h.authority.DeleteAccount(r.Context(), account, request.Erase)
		if err != nil {
			return 0, nil, err
		}
		erased := append([]string{}, request.Erase...)
		slices.Sort(erased)
		return 200, map[string]any{"deleted": account, "erased": slices.Compact(erased), "kept": kept}, nil
	case "password":
		var request struct {
			Password string `json:"password"`
		}
		if err = decode(body, &request); err != nil {
			return 0, nil, err
		}
		// Setting a password is reading the account's data, so it has its own
		// standing, narrower than suspending or deleting.
		if !h.authority.MayResetPassword(administrator, account) {
			return 0, nil, store.Refuse("permission_denied")
		}
		err = h.authority.ResetPassword(account, request.Password, administrator)
	default:
		return 0, nil, store.Refuse("not_found")
	}
	if err != nil {
		return 0, nil, err
	}
	return 204, nil, nil
}

func (h *Handler) apiReaders(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	readers, err := h.authority.Readers(administrator, r.PathValue("dataset"))
	if err != nil {
		return 0, nil, err
	}
	groups, err := h.authority.Groups(administrator, r.PathValue("dataset"))
	if err != nil {
		return 0, nil, err
	}
	stops, err := h.authority.Stops(administrator, r.PathValue("dataset"))
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"readers": readers, "groups": groups, "stopped": stops}, nil
}

// groupPrefix names a group reader in the readers routes: everyone the
// issuer after it made, rather than one account.
const groupPrefix = "issuer:"

func groupIssuer(r *http.Request) (string, bool) {
	return strings.CutPrefix(r.PathValue("account"), groupPrefix)
}

// apiReader answers one account's reading of a dataset: the types it reads,
// or not_found when it is not a reader there.
func (h *Handler) apiReader(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	if issuer, group := groupIssuer(r); group {
		groups, err := h.authority.Groups(administrator, r.PathValue("dataset"))
		if err != nil {
			return 0, nil, err
		}
		for _, g := range groups {
			if g.Issuer == issuer {
				return 200, g, nil
			}
		}
		return 0, nil, store.Refuse("not_found")
	}
	readers, err := h.authority.Readers(administrator, r.PathValue("dataset"))
	if err != nil {
		return 0, nil, err
	}
	for _, reader := range readers {
		if reader.Account == r.PathValue("account") {
			return 200, reader, nil
		}
	}
	return 0, nil, store.Refuse("not_found")
}
func (h *Handler) apiSetReader(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	var request struct {
		Types     []string   `json:"types"`
		ExpiresAt *time.Time `json:"expires_at"`
		// Password creates the account when it does not exist yet.
		Password string `json:"password"`
	}
	if err := decode(body, &request); err != nil {
		return 0, nil, err
	}
	if issuer, group := groupIssuer(r); group {
		if request.Password != "" {
			return 0, nil, store.Refuse("invalid_field")
		}
		if err := h.authority.SetGroupReader(r.Context(), administrator, r.PathValue("dataset"), issuer, request.Types, request.ExpiresAt); err != nil {
			return 0, nil, err
		}
		return 204, nil, nil
	}
	if err := h.authority.SetReader(r.Context(), administrator, r.PathValue("dataset"), r.PathValue("account"), request.Types, request.ExpiresAt, request.Password); err != nil {
		return 0, nil, err
	}
	return 204, nil, nil
}

func (h *Handler) apiRemoveReader(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	if issuer, group := groupIssuer(r); group {
		if err := h.authority.RemoveGroupReader(r.Context(), administrator, r.PathValue("dataset"), issuer); err != nil {
			return 0, nil, err
		}
		return 204, nil, nil
	}
	if err := h.authority.RemoveReader(r.Context(), administrator, r.PathValue("dataset"), r.PathValue("account")); err != nil {
		return 0, nil, err
	}
	return 204, nil, nil
}

func (h *Handler) apiContributors(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	contributors, err := h.authority.Contributors(administrator, r.PathValue("dataset"))
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"contributors": contributors}, nil
}

// apiContributor answers what one account adds to a dataset, or not_found
// when it adds nothing there.
func (h *Handler) apiContributor(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	contributors, err := h.authority.Contributors(administrator, r.PathValue("dataset"))
	if err != nil {
		return 0, nil, err
	}
	for _, contributor := range contributors {
		if contributor.Account == r.PathValue("account") {
			return 200, contributor, nil
		}
	}
	return 0, nil, store.Refuse("not_found")
}
func (h *Handler) apiSetContributor(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	var request struct {
		Types     []string   `json:"types"`
		ExpiresAt *time.Time `json:"expires_at"`
		// Password creates the account when it does not exist yet.
		Password string `json:"password"`
	}
	if err := decode(body, &request); err != nil {
		return 0, nil, err
	}
	if err := h.authority.SetContributor(r.Context(), administrator, r.PathValue("dataset"), r.PathValue("account"), request.Types, request.ExpiresAt, request.Password); err != nil {
		return 0, nil, err
	}
	return 204, nil, nil
}

func (h *Handler) apiRemoveContributor(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	if err := h.authority.RemoveContributor(r.Context(), administrator, r.PathValue("dataset"), r.PathValue("account")); err != nil {
		return 0, nil, err
	}
	return 204, nil, nil
}

// Limits, issuers and backups belong to the server, so they are for its
// administrator only: they spend storage every dataset shares.
func (h *Handler) apiSetLimits(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if !h.authority.AdministersServer(administrator) {
		return 0, nil, store.Refuse("permission_denied")
	}
	engine, ok := h.records.(limitsAdministrator)
	if !ok {
		return 0, nil, store.Refuse("not_supported")
	}
	dataset := r.PathValue("dataset")
	current, err := engine.DatasetLimits(r.Context(), dataset)
	if err != nil {
		return 0, nil, err
	}
	changed, err := p.MergeLimits(current, body)
	if err != nil {
		return 0, nil, store.Refuse("invalid_field")
	}
	if err = engine.SetDatasetLimits(r.Context(), dataset, changed); err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"limits": changed}, nil
}

// issuerView is an issuer as the API shows it: its policy and its counts,
// never its secret.
func issuerView(issuer identity.Issuer) map[string]any {
	view := map[string]any{"id": issuer.ID, "grants": issuer.Grants, "cap": issuer.Cap, "issued": issuer.Issued,
		"disabled": issuer.Disabled, "assign_names": issuer.AssignNames, "moderate_sharing": issuer.ModerateSharing, "device_redemption": issuer.DeviceRedemption}
	if len(issuer.Datasets) > 0 {
		view["datasets"] = issuer.Datasets
	}
	if t := issuer.Template; t != nil {
		types := make([]string, 0, len(t.Types))
		for _, m := range t.Types {
			types = append(types, m.ID)
		}
		readers := make([]map[string]any, 0, len(t.Readers))
		for _, reader := range t.Readers {
			entry := map[string]any{"account": reader.Account}
			if reader.Types != nil {
				entry["types"] = reader.Types
			}
			readers = append(readers, entry)
		}
		view["template"] = map[string]any{"administrator": t.Administrator, "display_name": t.DisplayName, "types": types, "readers": readers, "erase_with_account": t.EraseWithAccount}
	}
	return view
}

func (h *Handler) apiIssuers(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	if !h.authority.AdministersServer(administrator) {
		return 0, nil, store.Refuse("permission_denied")
	}
	issuers := []map[string]any{}
	for _, issuer := range h.authority.IssuerSummaries() {
		issuers = append(issuers, issuerView(issuer))
	}
	return 200, map[string]any{"issuers": issuers}, nil
}

func (h *Handler) apiChangeIssuer(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if !h.authority.AdministersServer(administrator) {
		return 0, nil, store.Refuse("permission_denied")
	}
	var request struct {
		Cap      *int  `json:"cap"`
		Disabled *bool `json:"disabled"`
	}
	if err := decode(body, &request); err != nil {
		return 0, nil, err
	}
	if request.Cap == nil && request.Disabled == nil {
		return 0, nil, store.Refuse("invalid_request")
	}
	changed, err := h.authority.ChangeIssuer(r.PathValue("issuer"), identity.IssuerChange{Cap: request.Cap, Disabled: request.Disabled})
	if err != nil {
		return 0, nil, err
	}
	return 200, issuerView(changed), nil
}

// apiRotateIssuer replaces an issuer's secret. The new secret is in the
// response and nowhere else; the old one stops working at once.
func (h *Handler) apiRotateIssuer(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	if !h.authority.AdministersServer(administrator) {
		return 0, nil, store.Refuse("permission_denied")
	}
	// The new secret invites whoever holds it into the issuer's datasets.
	for _, issuer := range h.authority.IssuerSummaries() {
		if issuer.ID == r.PathValue("issuer") && !h.authority.MayAdministerDatasets(administrator, issuer.Datasets) {
			return 0, nil, store.Refuse("permission_denied")
		}
	}
	secret, err := h.authority.RotateIssuerSecret(r.PathValue("issuer"))
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"id": r.PathValue("issuer"), "secret": secret}, nil
}

// apiIssueInvitation issues an invitation from an issuer on the
// administrator's own installation, for an administration tool that is an
// application and must not hold an issuer's secret. The answer is the one
// POST /oauth/invitations gives.
func (h *Handler) apiIssueInvitation(w http.ResponseWriter, r *http.Request, _ string, body []byte) (int, any, error) {
	var request struct {
		Account string `json:"account"`
		// Rebind names an existing account to reconnect to a new device.
		Rebind string `json:"rebind"`
	}
	if len(body) > 0 {
		if err := decode(body, &request); err != nil {
			return 0, nil, err
		}
	}
	access, _ := r.Context().Value(installationKey{}).(store.Access)
	if request.Rebind != "" {
		if request.Account != "" {
			return 0, nil, store.Refuse("invalid_field")
		}
		code, expires, err := h.authority.IssueRebindingAs(access, r.PathValue("issuer"), request.Rebind)
		if err != nil {
			return 0, nil, err
		}
		return 201, map[string]any{"invitation": code, "account": request.Rebind, "rebind": true, "expires_at": expires.UTC().Format(time.RFC3339)}, nil
	}
	code, account, expires, err := h.authority.IssueInvitationAs(access, r.PathValue("issuer"), request.Account)
	if err != nil {
		return 0, nil, err
	}
	answer := map[string]any{"invitation": code, "redeem_url": h.authority.RedeemURL(code), "expires_at": expires.UTC().Format(time.RFC3339)}
	if account != "" {
		answer["account"] = account
	}
	return 201, answer, nil
}

func (h *Handler) apiAddIssuer(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if !h.authority.AdministersServer(administrator) {
		return 0, nil, store.Refuse("permission_denied")
	}
	var request struct {
		ID          string       `json:"id"`
		Datasets    []string     `json:"datasets"`
		Grants      grantRequest `json:"grants"`
		Cap         int          `json:"cap"`
		AssignNames bool         `json:"assign_names"`
		// ModerateSharing is the issuer's policy, fixed when it is made:
		// members it makes join under it, and nobody joined under another.
		ModerateSharing bool `json:"moderate_sharing"`
		// DeviceRedemption lets an app redeem this issuer's invitations on a
		// device, with no password and no browser.
		DeviceRedemption bool `json:"device_redemption"`
		Template         *struct {
			Administrator string   `json:"administrator"`
			DisplayName   string   `json:"display_name"`
			Types         []p.Meta `json:"types"`
			Readers       []struct {
				Account string   `json:"account"`
				Types   []string `json:"types"`
			} `json:"readers"`
			EraseWithAccount bool `json:"erase_with_account"`
		} `json:"template"`
	}
	if err := decode(body, &request); err != nil {
		return 0, nil, err
	}
	// An issuer into existing datasets invites whoever holds its secret into
	// them, so configuring one takes administering them.
	if !h.authority.MayAdministerDatasets(administrator, request.Datasets) {
		return 0, nil, store.Refuse("permission_denied")
	}
	issuer := identity.Issuer{ID: request.ID, Datasets: request.Datasets, Cap: request.Cap, AssignNames: request.AssignNames, ModerateSharing: request.ModerateSharing, DeviceRedemption: request.DeviceRedemption,
		Grants: store.Grants{Read: request.Grants.Read, Write: request.Grants.Write, Schema: request.Grants.Schema, Erase: request.Grants.Erase}}
	if request.Template != nil {
		issuer.Template = &identity.Template{Administrator: request.Template.Administrator, DisplayName: request.Template.DisplayName, Types: request.Template.Types, EraseWithAccount: request.Template.EraseWithAccount}
		for _, reader := range request.Template.Readers {
			issuer.Template.Readers = append(issuer.Template.Readers, identity.TemplateReader{Account: reader.Account, Types: reader.Types})
		}
	}
	secret, err := h.authority.AddIssuer(r.Context(), issuer)
	if err != nil {
		return 0, nil, store.Refuse("invalid_field")
	}
	return 201, map[string]any{"id": request.ID, "secret": secret}, nil
}

func (h *Handler) apiBackup(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	if !h.authority.AdministersServer(administrator) {
		return 0, nil, store.Refuse("permission_denied")
	}
	if h.backup == nil {
		return 0, nil, store.Refuse("not_supported")
	}
	path, err := h.backup(r.Context())
	if err != nil {
		return 0, nil, store.Refuse("dataset_unavailable")
	}
	// The file's name, not its path: where backups are kept is the host's.
	return 201, map[string]any{"backup": path[strings.LastIndex(path, "/")+1:]}, nil
}

// A dataset's administrator may rename it: a name is a label for people, and
// the identity a client holds does not change.
func (h *Handler) apiRename(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	dataset := r.PathValue("dataset")
	held, err := h.datasets(r, administrator)
	if err != nil {
		return 0, nil, store.Refuse("dataset_unavailable")
	}
	known := false
	for _, info := range held {
		known = known || info.ID == dataset
	}
	if !known {
		return 0, nil, store.Refuse("permission_denied")
	}
	var request struct {
		DisplayName string `json:"display_name"`
	}
	if err = decode(body, &request); err != nil {
		return 0, nil, err
	}
	engine, ok := h.records.(renamer)
	if !ok {
		return 0, nil, store.Refuse("not_supported")
	}
	if err = engine.RenameDataset(r.Context(), dataset, request.DisplayName); err != nil {
		return 0, nil, err
	}
	return 204, nil, nil
}

// apiRotateService replaces a service's secret, which a deployment restored
// onto a machine where the service's secret file is gone has no other way to
// recover. The new secret is in the response and nowhere else. Holding it is
// reading what the service reads, so the caller must administer all of that.
func (h *Handler) apiRotateService(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	if !h.authority.MayRotateServiceSecret(administrator, r.PathValue("account")) {
		return 0, nil, store.Refuse("permission_denied")
	}
	secret, err := h.authority.RotateServiceSecret(r.PathValue("account"))
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"account": r.PathValue("account"), "secret": secret}, nil
}

// ReaderRoutes is the deployment's owner surface, using an ordinary write
// approval rather than the separately consented administration API.
func (h *Handler) ReaderRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/datasets/{dataset}/readers", h.ownerReaders(h.apiReaders))
	mux.HandleFunc("GET /v1/datasets/{dataset}/readers/{account}", h.ownerReaders(h.apiReader))
	mux.HandleFunc("POST /v1/datasets/{dataset}/readers/{action}", h.ownerReaders(h.apiFreezeReader))
}

func (h *Handler) ownerReaders(handle apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fields := strings.Fields(r.Header.Get("Authorization"))
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
			apiFailure(w, store.Refuse("authentication_failed"))
			return
		}
		access, err := h.authority.Authenticate(r.Context(), fields[1])
		if err == nil {
			err = h.authority.ReaderStanding(r.Context(), access, r.PathValue("dataset"))
		}
		if err != nil {
			apiFailure(w, err)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), installationKey{}, access))
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
		if err != nil {
			apiFailure(w, store.Refuse("invalid_request"))
			return
		}
		status, result, err := handle(w, r, access.Principal, body)
		if err != nil {
			apiFailure(w, err)
			return
		}
		w.WriteHeader(status)
		if result != nil {
			_ = json.NewEncoder(w).Encode(result)
		}
	}
}

func (h *Handler) apiFreezeReader(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if len(body) != 0 {
		return 0, nil, store.Refuse("invalid_request")
	}
	// A group name already contains a colon, so the action is the final suffix.
	name := r.PathValue("action")
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return 0, nil, store.Refuse("not_found")
	}
	account, action := name[:i], name[i+1:]
	if action != "freeze" && action != "unfreeze" {
		return 0, nil, store.Refuse("not_found")
	}
	access, _ := r.Context().Value(installationKey{}).(store.Access)
	err := h.authority.FreezeReader(r.Context(), access, r.PathValue("dataset"), account, action == "unfreeze", strings.HasPrefix(r.URL.Path, "/admin/"))
	return 204, nil, err
}

// apiInstallations lists an account's signed-in devices and applications, for
// an administrator who may administer the account.
func (h *Handler) apiInstallations(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	installations, err := h.authority.AccountInstallations(administrator, r.PathValue("account"))
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"installations": installations}, nil
}

// apiRemoveInstallation removes one of them: its access ends at once and no
// record of it stays.
func (h *Handler) apiRemoveInstallation(w http.ResponseWriter, r *http.Request, administrator string, _ []byte) (int, any, error) {
	if err := h.authority.RemoveAccountInstallation(r.Context(), administrator, r.PathValue("account"), r.PathValue("installation")); err != nil {
		return 0, nil, err
	}
	return 204, nil, nil
}
