// Package server exposes the implemented Tessera HTTP subset for embedding. It does
// not provide a listener, authentication database, OAuth server, or durable store.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/ekarpov/tessera/internal/release"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Authenticate resolves a bearer credential using host-owned authentication.
// Current grants are checked independently by the store on every transaction.
type Authenticate func(context.Context, string) (store.Access, error)
type handler struct {
	engine         store.Store
	authenticate   Authenticate
	contentOrigin  string
	provision      Provision
	provisioned    func(context.Context, store.Access, string) error
	offersCreation func() bool
}

// Provision decides whether a caller may create a dataset. The protocol has no
// grant for it — its grants are all over a dataset that already exists — so the
// decision is the host's and there is no default. A host that supplies none
// leaves the route refusing.
type Provision func(context.Context, store.Access) error

// Options describes authentication methods actually supplied by the host. The
// credential resolver treats service tokens as their own principals; it does
// not borrow the grants of a person who provisioned them.
type Options struct {
	// DeprecatedVersions announces planned retirement of still-served majors.
	// Empty omits the signal; dates never disable routes. The host must satisfy
	// the contract's overlap, notice and continued-client-support obligations.
	DeprecatedVersions []p.DeprecatedVersion
	// DisplayName is the operator's optional label for people, not identity.
	DisplayName          string
	AuthMethods          []string
	AuthorizationServers []string
	ContentOrigin        string
	// Provision opts a deployment into creating datasets over HTTP. Leaving it
	// nil is not an oversight: a server that offers the route is making a
	// promise of its own. The engine must also implement store.Provisioning,
	// or the route stays refused.
	Provision Provision
	// CreationOffered lets a deployment stop offering creation while it is
	// running. Capability is deployment state and authority is per-caller
	// state, so this is separate from Provision: one decides whether anybody
	// may create here, the other whether this caller may.
	//
	// nil means offered whenever the engine can provision and Provision is
	// supplied. Returning false withdraws the advertisement and refuses the
	// route with not_supported — and must leave recovery working, which is
	// what keeps "cannot create" distinguishable from "cannot reconcile".
	CreationOffered func() bool
	// SharedReads advertises that this deployment's authorization server
	// accepts a request to read everything shared with the account. The
	// record protocol serves such an installation like any other; it is the
	// approval that differs, and only the deployment knows whether it offers
	// one.
	SharedReads bool
	// Provisioned tells the deployment about a dataset that now exists, so
	// that whatever it does for the datasets it holds it also does for this
	// one. Without it a dataset created over the wire is one the engine knows
	// and the deployment does not: its creator holds it according to the
	// storage and according to nothing else, and no administration surface
	// can see it until the server is restarted.
	//
	// It runs before the response, on the fresh creation and on a replay
	// alike, so bookkeeping that failed once is retried by the retry the
	// caller was going to make anyway. It must therefore be idempotent, and
	// its failure is reported — the dataset exists and the idempotency key
	// recovers it, so a caller that retries gets both the identity and the
	// bookkeeping.
	Provisioned func(context.Context, store.Access, string) error
}

func New(engine store.Store, authenticate Authenticate) (http.Handler, error) {
	return NewWithOptions(engine, authenticate, Options{AuthMethods: []string{"bearer_token"}})
}
func NewWithOptions(engine store.Store, authenticate Authenticate, options Options) (http.Handler, error) {
	if engine == nil || authenticate == nil {
		return nil, store.Refuse("internal_error")
	}
	if len(options.AuthMethods) == 0 {
		return nil, store.Refuse("internal_error")
	}
	if err := p.ValidateDeprecatedVersions([]string{"1"}, options.DeprecatedVersions); err != nil {
		return nil, err
	}
	deprecated := append([]p.DeprecatedVersion(nil), options.DeprecatedVersions...)
	seen := map[string]bool{}
	for _, method := range options.AuthMethods {
		if seen[method] || (method != "oauth2" && method != "bearer_token" && method != "client_credentials") {
			return nil, store.Refuse("internal_error")
		}
		seen[method] = true
	}
	if (seen["oauth2"] || seen["client_credentials"]) && len(options.AuthorizationServers) == 0 {
		return nil, store.Refuse("internal_error")
	}
	methods := []string{}
	methods = append(methods, options.AuthMethods...)
	// An empty list must encode as [] rather than null. A strict client that
	// counts the entries should find none, not a missing member it has to
	// special-case, and the protocol's own example shows an array.
	authorities := []string{}
	authorities = append(authorities, options.AuthorizationServers...)
	// A server-level capability describes this server, not a dataset, and says
	// only that the route exists. Whether a caller may use it is server_access
	// on the authenticated listing, evaluated per request.
	_, engineCanProvision := engine.(store.Provisioning)
	offersCreation := func() bool {
		if !engineCanProvision || options.Provision == nil {
			return false
		}
		return options.CreationOffered == nil || options.CreationOffered()
	}
	h := &handler{engine: engine, authenticate: authenticate, contentOrigin: options.ContentOrigin, provision: options.Provision, offersCreation: offersCreation, provisioned: options.Provisioned}
	mux := http.NewServeMux()
	// The window is advertised whether or not creation is offered: withdrawing
	// the capability says "you cannot create", and withdrawing the window would
	// say "you cannot reconcile", which is a different statement.
	// Advertised whether or not creation is offered, like the retention window:
	// they describe the route rather than promise it.
	serverLimits := map[string]any{"max_creation_request_bytes": MaxCreationRequestBytes, "max_creation_types": MaxCreationTypes}
	if scoped, ok := engine.(store.ServerScoped); ok {
		serverLimits["idempotency_retention_seconds"] = scoped.ServerRetentionSeconds()
	}
	mux.HandleFunc("GET /.well-known/tessera", func(w http.ResponseWriter, r *http.Request) {
		capabilities := []string{}
		if offersCreation() {
			capabilities = append(capabilities, p.DatasetCreationCapability)
		}
		if options.SharedReads {
			capabilities = append(capabilities, p.SharedReadsCapability)
		}
		document := map[string]any{"protocol_versions": []string{"1"}, "server": map[string]string{"name": "tessera-reference", "version": release.Version()}, "auth_methods": methods, "authorization_servers": authorities, "server_capabilities": capabilities}
		if len(deprecated) > 0 {
			document["deprecated_versions"] = deprecated
		}
		if options.DisplayName != "" {
			document["display_name"] = options.DisplayName
		}
		if len(serverLimits) > 0 {
			document["server_limits"] = serverLimits
		}
		send(w, 200, document)
	})
	mux.HandleFunc("GET /v1/datasets", h.datasets)
	mux.HandleFunc("POST /v1/datasets", h.createDataset)
	mux.HandleFunc("POST /v1/operations/{action}", h.serverOperation)
	mux.HandleFunc("/v1/stores", h.content)
	mux.HandleFunc("/v1/stores/", h.content)
	mux.HandleFunc("/v1/transfers/", h.content)
	mux.HandleFunc("/v1/datasets/{dataset}/", h.resource)
	mux.HandleFunc("/v1/datasets/{action}", h.resource)
	return mux, nil
}

func (h *handler) access(r *http.Request) (store.Access, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return store.Access{}, store.Refuse("authentication_failed")
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return store.Access{}, store.Refuse("authentication_failed")
	}
	a, err := h.authenticate(r.Context(), parts[1])
	if err != nil || !a.Valid() {
		return store.Access{}, store.Refuse("authentication_failed")
	}
	return a, nil
}

// datasetEntry is one dataset on the wire, the same shape wherever it appears.
func datasetEntry(info store.Info) map[string]any {
	capabilities := info.Capabilities
	if capabilities == nil {
		capabilities = []string{}
	}
	entry := map[string]any{"id": info.ID, "display_name": info.DisplayName, "capabilities": capabilities, "access": info.Access, "limits": info.Limits, "watermark": info.Watermark, "schema_version": info.SchemaVersion, "view_version": info.ViewVersion}
	if info.ReplacedDatasetID != "" {
		entry["replaced_dataset_id"] = info.ReplacedDatasetID
	}
	if info.ContentStoreID != "" {
		entry["content_store_id"] = info.ContentStoreID
	}
	return entry
}

func (h *handler) datasets(w http.ResponseWriter, r *http.Request) {
	a, err := h.access(r)
	if err != nil {
		failure(w, err)
		return
	}
	// The engine decides which datasets this caller may use. One it holds no
	// grant on is absent from the listing rather than refused within it.
	datasets, err := h.engine.Datasets(r.Context(), a)
	if err != nil {
		failure(w, err)
		return
	}
	items := []any{}
	for _, info := range datasets {
		items = append(items, datasetEntry(info))
	}
	// Authority over the server itself, which is a different axis from access to
	// any dataset. Reported per request rather than carried in a token, and a
	// right this caller does not hold is false rather than absent, because an
	// absent member must never read as permission.
	//
	// That includes a server that offers no creation at all: the right is still
	// named and still false. Omitting it there would make one member mean two
	// things — "not offered here" and "not yours" — on the axis that exists to
	// answer the second, while server_capabilities already answers the first.
	rights := map[string]bool{"create_dataset": false}
	if h.offersCreation() {
		rights["create_dataset"] = h.provision(r.Context(), a) == nil
	}
	send(w, 200, map[string]any{"server_access": rights, "datasets": items})
}

// createDataset answers a creation, and answers a repeat of one from what was
// retained. The order below is the contract's: authenticate, resolve the claim,
// and only with no claim let capability and permission decide whether work may
// begin. Gating first would stop a caller learning what it already made once its
// right was withdrawn, which is exactly what its key exists to prevent.
func (h *handler) createDataset(w http.ResponseWriter, r *http.Request) {
	provisioning, canProvision := h.engine.(store.Provisioning)
	a, err := h.access(r)
	if err != nil {
		failure(w, err)
		return
	}
	// A creation has no dataset, so its outcome is retained under the server
	// scope instead. Refusing the key here, as this route once did, left a
	// caller whose response was lost with no way to discover whether a dataset
	// existed — the one thing this contract never asks of a caller.
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) > 1 {
		failure(w, store.Refuse("invalid_field"))
		return
	}
	key := ""
	if len(keys) == 1 {
		key = keys[0]
		if err = p.ValidateOperationKey(key); err != nil {
			failure(w, err)
			return
		}
	}
	data, err := h.readBody(w, r, MaxCreationRequestBytes)
	if err != nil {
		failure(w, err)
		return
	}
	var request struct {
		DisplayName string   `json:"display_name"`
		Types       []p.Meta `json:"types,omitempty"`
	}
	if err = p.DecodeRequest(data, &request, MaxCreationRequestBytes); err != nil {
		failure(w, err)
		return
	}
	if len(request.Types) > MaxCreationTypes {
		failure(w, &p.Error{Code: "invalid_request", Message: "Too many types for one creation"})
		return
	}
	digest := p.RequestDigest(r.Method, r.URL.RequestURI(), data)

	// Recovery precedes admission. A repeat is a question about what already
	// happened, so it is answered from the retained claim before capability or
	// permission are consulted — otherwise withdrawing a right would strand a
	// caller holding an outcome it cannot learn.
	if canProvision && key != "" {
		if retained, found, e := provisioning.RecoverCreation(r.Context(), a, key, digest); found || e != nil {
			if e != nil {
				failure(w, e)
				return
			}
			if e = h.announce(r.Context(), a, retained.DatasetID); e != nil {
				failure(w, e)
				return
			}
			w.Header().Set("Idempotency-Replayed", "true")
			send(w, 201, map[string]any{"dataset_id": retained.DatasetID})
			return
		}
	}
	// No retained claim, so this is new work and the gates apply.
	if !h.offersCreation() {
		failure(w, store.Refuse("not_supported"))
		return
	}
	if err = h.provision(r.Context(), a); err != nil {
		failure(w, err)
		return
	}
	// The key is required for this route and nowhere else, and is checked after
	// the gates: a server that does not offer creation says so rather than
	// complaining about a header for an operation it will not perform. Creating
	// an identity with no way to recover a lost response is not an available
	// mode, because the response carries the only record of what was made.
	if key == "" {
		failure(w, &p.Error{Code: "invalid_request", Message: "Creating a dataset requires an idempotency key"})
		return
	}
	made, err := provisioning.CreateDataset(r.Context(), a, request.DisplayName, p.Limits{}, request.Types, key, digest)
	if err != nil {
		failure(w, err)
		return
	}
	// An identity and nothing else. A replay must answer an owner that may no
	// longer read what it made — after a grant was withdrawn, after an erase —
	// so the response carries nothing whose disclosure depends on current
	// authority, and a replay is identical to the original by construction
	// rather than by a check. Current information comes from discovery.
	if err = h.announce(r.Context(), a, made.DatasetID); err != nil {
		failure(w, err)
		return
	}
	send(w, 201, map[string]any{"dataset_id": made.DatasetID})
}

// announce tells the deployment about a dataset that exists now. A deployment
// that asked for no such notice is told nothing.
func (h *handler) announce(ctx context.Context, a store.Access, dataset string) error {
	if h.provisioned == nil || dataset == "" {
		return nil
	}
	return h.provisioned(ctx, a, dataset)
}

// serverOperation answers for an operation that belongs to no dataset. Creation
// is the only one, and without this route its receipt would be unreachable:
// every other status path is addressed under the dataset the work acted on.
func (h *handler) serverOperation(w http.ResponseWriter, r *http.Request) {
	recovery, durable := h.engine.(store.Recovery)
	if !durable {
		failure(w, store.Refuse("not_implemented"))
		return
	}
	a, err := h.access(r)
	if err != nil {
		failure(w, err)
		return
	}
	action := r.PathValue("action")
	if !strings.HasSuffix(action, ":status") || r.URL.RawQuery != "" || r.Header.Get("Idempotency-Key") != "" {
		failure(w, store.Refuse("invalid_field"))
		return
	}
	if err = emptyBody(w, r); err != nil {
		failure(w, err)
		return
	}
	receipt, err := recovery.Status(r.Context(), a, store.ServerScope, strings.TrimSuffix(action, ":status"))
	if err != nil {
		failure(w, err)
		return
	}
	send(w, 200, receipt)
}

func (h *handler) resource(w http.ResponseWriter, r *http.Request) {
	a, err := h.access(r)
	if err != nil {
		failure(w, err)
		return
	}
	dataset := r.PathValue("dataset")
	transactionPath := false
	lifecycleAction := ""
	if dataset == "" {
		action := r.PathValue("action")
		if strings.HasSuffix(action, ":erase-preview") || strings.HasSuffix(action, ":erase") {
			cut := strings.LastIndex(action, ":")
			dataset = action[:cut]
			lifecycleAction = action[cut+1:]
		} else if strings.HasSuffix(action, ":transact") {
			dataset = strings.TrimSuffix(action, ":transact")
			transactionPath = true
		}
	}
	if err = p.ValidateIdentifier(p.DatasetIdentifier, dataset); err != nil {
		failure(w, store.Refuse("not_found"))
		return
	}
	parts := []string{}
	if !transactionPath && lifecycleAction == "" {
		parts, err = segments(r)
		if err != nil {
			failure(w, err)
			return
		}
	}
	recovery, durable := h.engine.(store.Recovery)
	keys := r.Header.Values("Idempotency-Key")
	key := ""
	if len(keys) > 1 {
		failure(w, store.Refuse("invalid_field"))
		return
	}
	if len(keys) == 1 {
		key = keys[0]
		if !durable {
			failure(w, store.Refuse("not_implemented"))
			return
		}
		if err = p.ValidateOperationKey(key); err != nil {
			failure(w, err)
			return
		}
	}
	if len(parts) == 2 && parts[0] == "operations" && strings.HasSuffix(parts[1], ":status") && r.Method == "POST" {
		if !durable {
			failure(w, store.Refuse("not_implemented"))
			return
		}
		if r.URL.RawQuery != "" || key != "" {
			failure(w, store.Refuse("invalid_field"))
			return
		}
		if err = emptyBody(w, r); err != nil {
			failure(w, err)
			return
		}
		receipt, e := recovery.Status(r.Context(), a, dataset, strings.TrimSuffix(parts[1], ":status"))
		if e != nil {
			failure(w, e)
			return
		}
		send(w, 200, receipt)
		return
	}
	if len(parts) == 2 && parts[0] == "pages" {
		pager, ok := h.engine.(store.Paging)
		if !ok {
			failure(w, store.Refuse("not_implemented"))
			return
		}
		if key != "" || r.URL.RawQuery != "" {
			failure(w, store.Refuse("invalid_field"))
			return
		}
		if err = emptyBody(w, r); err != nil {
			failure(w, err)
			return
		}
		if r.Method == "POST" && strings.HasSuffix(parts[1], ":next") {
			body, e := pager.NextPage(r.Context(), a, dataset, strings.TrimSuffix(parts[1], ":next"))
			if e != nil {
				failure(w, e)
				return
			}
			sendReply(w, store.Reply{Status: 200, Body: body})
			return
		}
		if r.Method == "DELETE" {
			if e := pager.EndPage(r.Context(), a, dataset, parts[1]); e != nil {
				failure(w, e)
				return
			}
			w.WriteHeader(204)
			return
		}
		failure(w, store.Refuse("not_found"))
		return
	}
	if lifecycleAction != "" {
		lifecycle, ok := h.engine.(store.Lifecycle)
		if !ok {
			failure(w, store.Refuse("not_implemented"))
			return
		}
		// Erasure is the one mutation that does not honour an idempotency key:
		// confirmation and replacement discovery are its recovery mechanism, and
		// a receipt would belong to a dataset that has ceased to exist. Refused
		// before any retirement happens.
		if key != "" {
			failure(w, store.Refuse("invalid_request"))
			return
		}
		if r.Method != "POST" || r.URL.RawQuery != "" {
			failure(w, store.Refuse("invalid_field"))
			return
		}
		data, e := h.readBody(w, r, 4096)
		if e != nil {
			failure(w, e)
			return
		}
		if lifecycleAction == "erase-preview" {
			var body struct{}
			if e = p.DecodeRequest(data, &body, 4096); e != nil {
				failure(w, e)
				return
			}
			result, e := lifecycle.PreviewErase(r.Context(), a, dataset)
			if e != nil {
				failure(w, e)
				return
			}
			send(w, 200, result)
			return
		}
		var body struct {
			Confirm string `json:"confirm"`
		}
		if e = p.DecodeRequest(data, &body, 4096); e != nil {
			failure(w, e)
			return
		}
		result, e := lifecycle.Erase(r.Context(), a, dataset, body.Confirm)
		if e != nil {
			failure(w, e)
			return
		}
		send(w, 200, result)
		return
	}
	var data []byte
	bodyRead := false
	if key != "" {
		switch r.Method {
		case "POST", "PUT", "PATCH":
			data, err = h.readBody(w, r, recovery.RecoveryLimits().MaxRequestBytes)
			bodyRead = true
		case "DELETE":
			err = emptyBody(w, r)
		default:
			err = store.Refuse("invalid_field")
		}
		if err != nil {
			failure(w, err)
			return
		}
		reply, found, e := recovery.Replay(r.Context(), a, dataset, key, p.RequestDigest(r.Method, r.URL.RequestURI(), data))
		if e != nil {
			failure(w, e)
			return
		}
		if found {
			sendReply(w, reply)
			return
		}
	}
	// The engine refuses an identity it does not serve; the handler no longer
	// has to know that there is only one to compare against.
	info, err := h.engine.Info(r.Context(), a, dataset)
	if err != nil {
		failure(w, err)
		return
	}
	if !bodyRead && (r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH") {
		data, err = h.readBody(w, r, info.Limits.MaxRequestBytes)
		if err != nil {
			failure(w, err)
			return
		}
	}
	if r.Method == "DELETE" {
		if err = emptyBody(w, r); err != nil {
			failure(w, err)
			return
		}
	}
	query, err := urlQuery(r)
	if err != nil {
		failure(w, err)
		return
	}
	objectRead := !transactionPath && len(parts) == 3 && parts[0] == "objects" && r.Method == "GET"
	metaDelete := !transactionPath && len(parts) == 2 && parts[0] == "meta" && r.Method == "DELETE"
	for k, vs := range query {
		if len(vs) != 1 || !(objectRead && k == "fields" || metaDelete && k == "confirm") {
			failure(w, store.Refuse("invalid_field"))
			return
		}
	}
	if len(parts) == 2 && parts[0] == "objects" && strings.HasSuffix(parts[1], ":page") && r.Method == "POST" {
		pager, ok := h.engine.(store.Paging)
		if !ok {
			failure(w, store.Refuse("not_implemented"))
			return
		}
		if key != "" {
			failure(w, store.Refuse("invalid_field"))
			return
		}
		var body struct {
			Query *p.Query `json:"query"`
			Size  *int     `json:"page_size,omitempty"`
		}
		if err = p.DecodeRequest(data, &body, info.Limits.MaxRequestBytes); err != nil {
			failure(w, err)
			return
		}
		if body.Query == nil {
			failure(w, store.Refuse("invalid_field"))
			return
		}
		size := info.Limits.MaxPageSize
		if body.Size != nil {
			size = *body.Size
		}
		cursor, e := pager.BeginPage(r.Context(), a, dataset, strings.TrimSuffix(parts[1], ":page"), *body.Query, size)
		if e != nil {
			failure(w, e)
			return
		}
		send(w, 200, cursor)
		return
	}
	var request p.Transaction
	mutation := false
	status := 200
	if transactionPath {
		if r.Method != "POST" {
			failure(w, store.Refuse("not_found"))
			return
		}
		request, err = p.DecodeTransaction(data, info.Limits)
		mutation = true
	} else {
		var op *p.Operation
		switch {
		case len(parts) == 1 && parts[0] == "meta" && r.Method == "POST", len(parts) == 2 && parts[0] == "meta" && r.Method == "PUT":
			var body struct {
				Type    *p.Meta `json:"type"`
				Confirm string  `json:"confirm,omitempty"`
			}
			err = p.DecodeRequest(data, &body, info.Limits.MaxRequestBytes)
			if err == nil && body.Type == nil {
				err = store.Refuse("invalid_field")
			}
			if err == nil {
				kind := "define_type"
				if r.Method == "PUT" {
					kind = "update_type"
					if body.Type.ID != parts[1] {
						err = store.Refuse("meta_schema_error")
					}
				} else {
					status = 201
					if body.Confirm != "" {
						err = store.Refuse("invalid_field")
					}
				}
				op = &p.Operation{Op: kind, Meta: body.Type, Confirm: body.Confirm}
			}
		case metaDelete:
			if query.Get("confirm") != parts[1] {
				err = store.Refuse("invalid_filter")
			}
			op = &p.Operation{Op: "delete_type", Type: parts[1], Cascade: true}
		case len(parts) == 2 && parts[0] == "objects" && r.Method == "POST" && !strings.Contains(parts[1], ":"):
			var body struct {
				Fields map[string]json.RawMessage `json:"fields"`
			}
			err = p.DecodeRequest(data, &body, info.Limits.MaxRequestBytes)
			if err == nil && body.Fields == nil {
				err = store.Refuse("invalid_field")
			}
			op = &p.Operation{Op: "insert", Type: parts[1], Fields: body.Fields}
			status = 201
		case len(parts) == 3 && parts[0] == "objects" && r.Method == "PATCH":
			var body struct {
				Fields map[string]json.RawMessage `json:"fields"`
			}
			err = p.DecodeRequest(data, &body, info.Limits.MaxRequestBytes)
			if err == nil && body.Fields == nil {
				err = store.Refuse("invalid_field")
			}
			op = &p.Operation{Op: "update", Target: &p.Target{Type: parts[1], ID: parts[2]}, Fields: body.Fields}
		case len(parts) == 3 && parts[0] == "objects" && r.Method == "DELETE":
			op = &p.Operation{Op: "delete", Target: &p.Target{Type: parts[1], ID: parts[2]}}
		case len(parts) == 2 && parts[0] == "objects" && r.Method == "POST" && (strings.HasSuffix(parts[1], ":update") || strings.HasSuffix(parts[1], ":delete")):
			var body struct {
				Fields    map[string]json.RawMessage `json:"fields,omitempty"`
				Filter    json.RawMessage            `json:"filter,omitempty"`
				ObjectIDs []string                   `json:"object_ids,omitempty"`
			}
			err = p.DecodeRequest(data, &body, info.Limits.MaxRequestBytes)
			kind := "delete_where"
			typ := strings.TrimSuffix(parts[1], ":delete")
			if strings.HasSuffix(parts[1], ":update") {
				kind = "update_where"
				typ = strings.TrimSuffix(parts[1], ":update")
				if body.Fields == nil || body.ObjectIDs != nil {
					err = store.Refuse("invalid_field")
				}
			} else if body.Fields != nil {
				err = store.Refuse("invalid_field")
			}
			op = &p.Operation{Op: kind, Type: typ, Fields: body.Fields, Filter: body.Filter, ObjectIDs: body.ObjectIDs}
		}
		if op != nil && err == nil {
			// Preserve annotation bytes while translating conveniences. The original
			// request bytes, never this translation, are the future receipt digest.
			request = p.Transaction{Operations: []p.Operation{*op}}
			var raw []byte
			raw, err = p.MarshalWire(request)
			if err == nil {
				// The transport limit applies to the received body, not to the
				// extra envelope/target we added for the shared executor.
				limits := info.Limits
				if len(raw) > limits.MaxRequestBytes {
					limits.MaxRequestBytes = len(raw)
				}
				request, err = p.DecodeTransaction(raw, limits)
			}
			mutation = true
		}
	}
	if err != nil {
		failure(w, err)
		return
	}
	// A key belongs to a mutation, whose outcome it lets a caller recover.
	// On a read it is a header the request's grammar has no place for, and
	// refused as that — invalid_field sent callers hunting for a bad field
	// in a body that had none.
	if key != "" && !mutation {
		failure(w, &p.Error{Code: "invalid_request", Message: "A read takes no Idempotency-Key",
			Details: map[string]any{"reason": "unexpected_idempotency_key"}})
		return
	}
	// RFC 7240. A preference rather than an instruction: a server may decline,
	// and says so by not sending Preference-Applied. This one is declined for a
	// caller that may write and not read, because a representation handed to
	// somebody who may not read it is a disclosure.
	represent := false
	for _, value := range r.Header.Values("Prefer") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "return=representation") {
				represent = true
			}
		}
	}
	// Declined for a caller that may write and not read, which is exact
	// because the grants are already in hand: a representation handed to
	// somebody who may not read it would be a disclosure, and saying it was
	// applied when it was not would send a client home without the read-back
	// it still needs.
	//
	// Read alone is not enough. A contributor may also read, by type, and
	// what it inserts need not be a type it reads; only a caller that writes
	// holds the whole dataset in view, so only there does read cover the row.
	represent = represent && info.Access.Read && info.Access.Write
	if represent {
		w.Header().Set("Preference-Applied", "return=representation")
	}
	if mutation && durable {
		reply, e := recovery.Mutate(r.Context(), a, store.Mutation{Dataset: dataset, Key: key, Digest: p.RequestDigest(r.Method, r.URL.RequestURI(), data), Transaction: request, Single: !transactionPath, SuccessStatus: status, Represent: represent})
		if e != nil {
			failure(w, e)
			return
		}
		sendReply(w, reply)
		return
	}
	var result any
	// A request that changes nothing is evaluated as a read where the engine
	// offers one: it takes no writer's turn and builds no state to save. An
	// engine without the boundary answers the same, through Transact.
	evaluate := h.engine.Transact
	if reader, ok := h.engine.(store.Reader); ok && !mutation {
		evaluate = reader.Read
		// Describing a type reads no records, and an engine that keeps them
		// in storage should not fetch any to answer it.
		if len(parts) > 0 && parts[0] == "meta" {
			evaluate = reader.ReadSchema
		}
		// Reading one record by identity reads that record.
		if objectRead {
			evaluate = reader.ReadRecord
		}
	}
	err = evaluate(r.Context(), a, dataset, func(tx store.Tx) error {
		if mutation {
			response, e := store.ExecuteWith(tx, request, info.ID, info.Limits, store.Represent(represent))
			if e != nil {
				return e
			}
			if transactionPath {
				result = response
			} else {
				raw, e := p.MarshalWire(response.Results[0])
				if e != nil {
					return e
				}
				var fields map[string]json.RawMessage
				if e = json.Unmarshal(raw, &fields); e != nil {
					return e
				}
				watermark, _ := json.Marshal(response.Watermark)
				fields["watermark"] = watermark
				result = fields
			}
			return nil
		}
		var e error
		switch {
		case len(parts) == 1 && parts[0] == "meta" && r.Method == "GET":
			var types []p.Meta
			types, e = tx.ListMetadata()
			result = map[string]any{"types": types, "watermark": tx.Watermark()}
		case len(parts) == 2 && parts[0] == "meta" && (r.Method == "GET" || r.Method == "HEAD"):
			var meta p.Meta
			meta, e = tx.Metadata(parts[1])
			result = map[string]any{"type": meta, "watermark": tx.Watermark()}
		case len(parts) == 3 && parts[0] == "meta" && parts[2] == "children" && r.Method == "GET":
			if _, e = tx.Metadata(parts[1]); e != nil {
				return e
			}
			types, e := tx.ListMetadata()
			if e != nil {
				return e
			}
			children := []p.Meta{}
			for _, m := range types {
				if m.ParentID != nil && *m.ParentID == parts[1] {
					children = append(children, m)
				}
			}
			result = map[string]any{"types": children, "watermark": tx.Watermark()}
		case objectRead:
			var fields []string
			if vs, ok := query["fields"]; ok {
				fields = []string{}
				if vs[0] != "" {
					fields = strings.Split(vs[0], ",")
				}
			}
			result, e = tx.Object(parts[1], parts[2], fields)
		case len(parts) == 2 && parts[0] == "objects" && r.Method == "POST" && strings.HasSuffix(parts[1], ":query"):
			var body struct {
				Query *p.Query `json:"query"`
				Limit *int     `json:"limit,omitempty"`
			}
			if e = p.DecodeRequest(data, &body, info.Limits.MaxRequestBytes); e != nil {
				return e
			}
			if body.Query == nil {
				return store.Refuse("invalid_field")
			}
			limit := info.Limits.MaxOverallLimit
			if body.Limit != nil {
				limit = *body.Limit
			}
			result, e = tx.Query(strings.TrimSuffix(parts[1], ":query"), *body.Query, limit)
		case len(parts) == 2 && parts[0] == "objects" && r.Method == "POST" && strings.HasSuffix(parts[1], ":count"):
			if e = tx.Authorize(store.Grants{Read: true}); e != nil {
				return e
			}
			var body struct {
				Filter json.RawMessage `json:"filter,omitempty"`
			}
			if e = p.DecodeRequest(data, &body, info.Limits.MaxRequestBytes); e != nil {
				return e
			}
			predicate, e := p.ParsePredicate(body.Filter, info.Limits)
			if e != nil {
				return e
			}
			rows, e := tx.Matching(strings.TrimSuffix(parts[1], ":count"), predicate)
			if e != nil {
				return e
			}
			result = map[string]any{"count": len(rows), "watermark": tx.Watermark()}
		case len(parts) == 2 && parts[0] == "objects" && r.Method == "POST" && strings.HasSuffix(parts[1], ":aggregate"):
			if e = tx.Authorize(store.Grants{Read: true}); e != nil {
				return e
			}
			var body struct {
				Filter  json.RawMessage `json:"filter,omitempty"`
				Field   string          `json:"field,omitempty"`
				GroupBy string          `json:"group_by,omitempty"`
			}
			if e = p.DecodeRequest(data, &body, info.Limits.MaxRequestBytes); e != nil {
				return e
			}
			predicate, e := p.ParsePredicate(body.Filter, info.Limits)
			if e != nil {
				return e
			}
			aggregates, e := tx.Aggregate(strings.TrimSuffix(parts[1], ":aggregate"), body.Field, body.GroupBy, predicate)
			if e != nil {
				return e
			}
			result = map[string]any{"aggregates": aggregates, "watermark": tx.Watermark()}
		case len(parts) == 2 && parts[0] == "objects" && r.Method == "POST" && strings.HasSuffix(parts[1], ":distinct-references"):
			if e = tx.Authorize(store.Grants{Read: true}); e != nil {
				return e
			}
			var body struct {
				Filter       json.RawMessage `json:"filter,omitempty"`
				Field        string          `json:"field"`
				MaximumCount int             `json:"maximum_count"`
			}
			if e = p.DecodeRequest(data, &body, info.Limits.MaxRequestBytes); e != nil {
				return e
			}
			predicate, e := p.ParsePredicate(body.Filter, info.Limits)
			if e != nil {
				return e
			}
			references, e := tx.DistinctReferences(strings.TrimSuffix(parts[1], ":distinct-references"), body.Field, body.MaximumCount, predicate)
			if e != nil {
				return e
			}
			result = map[string]any{"references": references, "watermark": tx.Watermark()}
		default:
			return store.Refuse("not_implemented")
		}
		return e
	})
	if err != nil {
		failure(w, err)
		return
	}
	if r.Method == "HEAD" {
		w.WriteHeader(status)
		return
	}
	send(w, status, result)
}

func (h *handler) readBody(w http.ResponseWriter, r *http.Request, maxBytes int) ([]byte, error) {
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		return nil, store.Refuse("invalid_field")
	}
	if r.Header.Get("Content-Encoding") != "" {
		return nil, store.Refuse("invalid_field")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(maxBytes)))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, &requestTooLarge{}
		}
		return nil, store.Refuse("invalid_field")
	}
	return data, nil
}

type requestTooLarge struct{}

func (*requestTooLarge) Error() string { return "request body exceeds configured limit" }

func send(w http.ResponseWriter, status int, body any) {
	data, err := p.MarshalWire(body)
	if err != nil {
		failure(w, store.Refuse("internal_error"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
func failure(w http.ResponseWriter, err error) {
	status := 500
	var problem *p.Error
	if !errors.As(err, &problem) {
		problem = &p.Error{Code: "internal_error", Message: "Operation could not be completed"}
	}
	status = p.ErrorStatus(problem.Code)
	var tooLarge *requestTooLarge
	if errors.As(err, &tooLarge) {
		status = 413
		problem = &p.Error{Code: "query_result_limit_exceeded", Message: "Request body exceeds configured limit"}
	}
	if status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer`)
	}
	send(w, status, map[string]any{"error": problem})
}

func sendReply(w http.ResponseWriter, reply store.Reply) {
	if reply.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(reply.Status)
	_, _ = w.Write(reply.Body)
}

// emptyBody refuses a body on a route that takes none.
//
// As a malformed request, which is what it is, and saying so. It used to be
// invalid_field with no detail, on routes where no field is involved: a
// consumer sending {} to continue a page read that as "this dataset lacks the
// field", skipped the dataset, and drew an empty chart rather than failing.
func emptyBody(w http.ResponseWriter, r *http.Request) error {
	unexpected := &p.Error{Code: "invalid_request", Message: "This route takes no request body",
		Details: map[string]any{"reason": "unexpected_body"}}
	if r.Header.Get("Content-Encoding") != "" {
		return unexpected
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || len(data) > 0 {
		return unexpected
	}
	return nil
}

// The bounds on a creation request. It precedes any dataset, so no dataset's
// limits can govern it, and it may now carry a whole schema — which is why
// both are advertised in server_limits rather than left for a client to find
// by being refused.
const (
	MaxCreationRequestBytes = 256 << 10
	MaxCreationTypes        = 128
)
