package identity

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

// Submit the rendered checkbox values, so this catches broken defaults as well
// as grants that accidentally bypass the person's actual submitted selection.
func TestConsentHintDefaultsAndEffectiveAccess(t *testing.T) {
	for _, tc := range []struct {
		name, scope, checked, missing      string
		hint, insert, untickWrite, unnamed bool
	}{
		{name: "study", hint: true, scope: "read write", checked: "read write"},
		{name: "untick_write", hint: true, scope: "read write", checked: "read write", untickWrite: true},
		{name: "no_hint_without_insert", checked: "read"},
		{name: "safe_defaults", insert: true, checked: "read insert"},
		{name: "insert_unavailable", hint: true, scope: "insert", missing: "insert"},
		{name: "explicit_empty", hint: true},
		{name: "dangerous_hints", hint: true, scope: "read write schema erase administer create_dataset shared_reads", checked: "read write", missing: "create_dataset, shared_reads"},
		{name: "insert_hint", hint: true, scope: "insert insert", insert: true, checked: "insert"},
		{name: "chooser", hint: true, scope: "read write", checked: "read write", unnamed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, engine, dataset := authFixture(t)
			principal := a.data.Principals["person"]
			principal.Datasets[dataset] = store.Grants{Read: true, Write: true, Schema: true, Erase: true, Insert: tc.insert}
			a.data.Principals["person"] = principal
			a.data.Clients["client"] = Client{ID: "client", Name: "Synthetic app", Redirects: []string{"https://client.example/callback"}}
			query := pageQuery(dataset)
			if tc.unnamed {
				query.Del("dataset")
			}
			if tc.hint {
				query.Set("scope", tc.scope)
			}
			w := signInRequest(t, a, query, nil)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var id string
			for id = range a.flows {
			}
			w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "username": {"person"}, "password": {"synthetic-password-only"}, "decision": {"continue"}}, w.Result().Cookies()[0])
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			body := w.Body.String()
			consent := url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "decision": {"allow"}, "dataset": {dataset}}
			for _, name := range []string{"read", "write", "insert", "schema", "erase", "administer", "create_dataset", "shared_reads"} {
				checked := regexp.MustCompile(`name="` + name + `"[^>]* checked`).MatchString(body)
				want := strings.Contains(" "+tc.checked+" ", " "+name+" ")
				if checked != want {
					t.Errorf("%s checked=%v, want %v", name, checked, want)
				}
				if checked {
					consent.Set(name, "yes")
				}
			}
			if tc.missing != "" && (!strings.Contains(body, `role="status"`) || !strings.Contains(body, "<strong>"+tc.missing+"</strong>")) {
				t.Fatal("missing visible unavailable hints", body)
			}
			if tc.untickWrite {
				consent.Del("write")
			}
			w = formCall(a, "/oauth/authorize", consent, w.Result().Cookies()[0])
			if w.Code != 303 {
				t.Fatal(w.Code, w.Body.String())
			}
			target, _ := url.Parse(w.Header().Get("Location"))
			w = formCall(a, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"client"}, "redirect_uri": {query.Get("redirect_uri")}, "code": {target.Query().Get("code")}, "code_verifier": {strings.Repeat("x", 43)}}, nil)
			var tokens struct {
				AccessToken string `json:"access_token"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tokens) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			access, err := a.Authenticate(t.Context(), tokens.AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			want := store.Grants{Read: consent.Has("read"), Write: consent.Has("write"), Insert: consent.Has("insert")}
			for _, auth := range a.data.Authorizations {
				if auth.Grants != want || auth.Administer || auth.CreateDataset || auth.SharedReads {
					t.Fatal("unapproved authority", auth)
				}
			}
			if want.Read || want.Write || want.Insert {
				info, err := engine.Info(t.Context(), access, dataset)
				if err != nil {
					t.Fatal(err)
				}
				if info.Access.Read != want.Read || info.Access.Write != want.Write || info.Access.Insert != want.Insert || info.Access.Schema || info.Access.Erase {
					t.Fatal("client cannot detect actual access", info.Access)
				}
			}
		})
	}
}

func TestConsentDefaultsAcrossDatasets(t *testing.T) {
	p := Principal{Datasets: map[string]store.Grants{"one": {Read: true, Write: true, Insert: true}, "two": {Read: true}}}
	for _, f := range []flow{{}, {HasScope: true, Scope: "read write insert"}} {
		defaults, _ := consentDefaults(f, p, []datasetOption{{ID: "one"}, {ID: "two"}})
		if defaults != (store.Grants{Read: true}) {
			t.Fatal("chooser preselected an unheld grant", defaults)
		}
	}
}

func TestInvalidConsentScope(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Redirects: []string{"https://client.example/callback"}}
	for _, scope := range []string{"read unknown", "READ", "read\twrite", strings.Repeat("read ", 60)} {
		q := pageQuery(dataset)
		q.Set("scope", scope)
		w := signInRequest(t, a, q, nil)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"invalid_scope"`) || len(a.flows) != 0 {
			t.Fatal("invalid scope accepted", scope, w.Code)
		}
	}
}

func TestHintSurvivesAccountCreationAndRecovery(t *testing.T) {
	a, _, dataset := authFixture(t)
	secret, err := a.AddIssuer(t.Context(), Issuer{ID: "portal", Datasets: []string{dataset}, Grants: store.Grants{Read: true, Write: true, Erase: true}})
	if err != nil {
		t.Fatal(err)
	}
	code, _, _, err := a.IssueInvitation("portal", secret, "")
	if err != nil {
		t.Fatal(err)
	}
	// A pending sign-in can span account creation. The app retains its OAuth
	// request while the independent invitation page creates the account once.
	a.data.Clients["client"] = Client{ID: "client", Redirects: []string{"https://client.example/callback"}}
	q := pageQuery(dataset)
	q.Set("scope", "read write")
	w := signInRequest(t, a, q, map[string]string{"Accept": "text/html"})
	cookie := w.Result().Cookies()[0]
	var id string
	for id = range a.flows {
	}
	w = formCall(a, "/oauth/invitation", url.Values{"code": {code}, "account": {"invitee"}, "password": {"synthetic-password-only"}}, nil)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `data-account="invitee"`) {
		t.Fatal("account creation failed", w.Code, w.Body.String())
	}
	wrong := browserPost(a, url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "username": {"invitee"}, "password": {"wrong"}, "decision": {"continue"}}, cookie)
	if wrong.Code != 401 || a.flows[id].Scope != "read write" {
		t.Fatal("wrong-password retry lost hint")
	}
	for attempt := 0; attempt < 2; attempt++ {
		w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "username": {"invitee"}, "password": {"synthetic-password-only"}, "decision": {"continue"}}, cookie)
		for _, name := range []string{"read", "write"} {
			if w.Code != 200 || !strings.Contains(w.Body.String(), `name="`+name+`" value="yes" checked`) {
				t.Fatal("hint lost", attempt, w.Code, w.Body.String())
			}
		}
		if attempt == 1 {
			break
		}
		cookie = w.Result().Cookies()[0]
		f := a.flows[id]
		f.Expires = time.Now().Add(-time.Minute)
		a.flows[id] = f
		a.recoveries[id] = f
		w = browserPost(a, url.Values{"flow": {id}, "csrf": {f.CSRF}, "decision": {"allow"}}, cookie)
		assertRestart(t, a, w, q.Encode())
		restart := pageLink(t, w)
		w = httptest.NewRecorder()
		a.Handler().ServeHTTP(w, httptest.NewRequest("GET", restart, nil))
		cookie = w.Result().Cookies()[0]
		for candidate := range a.flows {
			id = candidate
		}
	}
	// Recovery never redeems the invitation again, even though consent has not
	// completed and the app might still be unable to join.
	if _, err := a.RedeemInvitation(t.Context(), code, "invitee", "synthetic-password-only"); err == nil {
		t.Fatal("invitation reused")
	}
	if len(a.data.Authorizations) != 0 {
		t.Fatal("recovery authorized without consent")
	}
}

func TestExpiredGrantsAreNotConsentDefaults(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	p := Principal{Datasets: map[string]store.Grants{"one": {Read: true, Write: true, Insert: true, ExpiresAt: &expired}}}
	for _, f := range []flow{{Dataset: "one"}, {Dataset: "one", HasScope: true, Scope: "read write insert"}} {
		defaults, _ := consentDefaults(f, p, nil)
		if defaults != (store.Grants{}) {
			t.Fatal("expired grants preselected", defaults)
		}
	}
}

func TestRecoveryPreservesEmptyScope(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Redirects: []string{"https://client.example/callback"}}
	q := pageQuery(dataset)
	q.Set("scope", "")
	w := signInRequest(t, a, q, map[string]string{"Accept": "text/html"})
	var id string
	for id = range a.flows {
	}
	f := a.flows[id]
	f.Expires = time.Now().Add(-time.Minute)
	a.flows[id] = f
	a.recoveries[id] = f
	w = browserPost(a, url.Values{"flow": {id}, "csrf": {f.CSRF}, "decision": {"continue"}}, w.Result().Cookies()[0])
	assertRestart(t, a, w, q.Encode())
}
