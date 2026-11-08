package identity

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/pkg/store"
)

func TestOrdinaryConsentSummaryAndReuse(t *testing.T) {
	for _, tc := range []struct {
		name, scope                                                                         string
		force, revoked, withdrawn, missingWrite, redirectChanged, wantReuse, shared, noHint bool
	}{
		{name: "previous_dataset_and_shared_reads", scope: "read write", wantReuse: true, shared: true},
		{name: "no_hint_preserves_prior_data_scope", noHint: true, wantReuse: true},
		{name: "same_request", scope: "read write", wantReuse: true},
		{name: "narrower_request", scope: "read", wantReuse: true},
		{name: "additional_permission", scope: "read schema"},
		{name: "force_review", scope: "read write", force: true},
		{name: "revoked", scope: "read write", revoked: true},
		{name: "withdrawn", scope: "read write", withdrawn: true},
		{name: "role_lost_write", scope: "read write", missingWrite: true},
		{name: "different_callback", scope: "read write", redirectChanged: true},
		{name: "empty_hint", scope: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, dataset := authFixture(t)
			callback := "com.example.synthetic:/callback"
			a.data.Clients["client"] = Client{ID: "client", Name: "Notes", Redirects: []string{callback, "https://notes.example/callback"}}
			query := pageQuery(dataset)
			query.Set("redirect_uri", callback)
			query.Set("scope", "read write")
			if tc.shared {
				query.Set("shared_reads", "true")
			}
			start := func(q url.Values) *httptest.ResponseRecorder {
				w := signInRequest(t, a, q, map[string]string{"Accept": "text/html"})
				if w.Code != 200 {
					t.Fatal(w.Code)
				}
				var id string
				for id = range a.flows {
				}
				return browserPost(a, url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "username": {"person"}, "password": {"synthetic-password-only"}, "decision": {"continue"}}, w.Result().Cookies()[0])
			}
			w := start(query)
			body := w.Body.String()
			if w.Code != 200 || !strings.Contains(body, "Notes will see your data and add, change and delete entries.") || !strings.Contains(body, "<details><summary>Change what the app may do</summary>") || strings.Contains(body, "<details open") {
				t.Fatal("not simple collapsed consent", body)
			}
			var id string
			for id = range a.flows {
			}
			approved := url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "decision": {"allow"}, "read": {"yes"}, "write": {"yes"}}
			if tc.shared {
				approved.Set("shared_reads", "yes")
			}
			w = browserPost(a, approved, w.Result().Cookies()[0])
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			var priorID string
			for priorID = range a.data.Authorizations {
			}
			prior := a.data.Authorizations[priorID]
			if prior.ConsentRedirect != callback {
				t.Fatal("consent callback not recorded")
			}
			if tc.revoked {
				if err := a.revoke(t.Context(), priorID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.withdrawn {
				if err := a.WithdrawGrants(t.Context(), priorID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.missingWrite {
				if err := a.GrantDataset("person", dataset, store.Grants{Read: true}); err != nil {
					t.Fatal(err)
				}
			}
			query.Set("scope", tc.scope)
			if tc.noHint {
				query.Del("scope")
			}
			if tc.shared {
				query.Del("dataset")
			}
			if tc.force {
				query.Set("prompt", "consent")
			}
			if tc.redirectChanged {
				query.Set("redirect_uri", "https://notes.example/callback")
			}
			w = start(query)
			reused := strings.Contains(w.Body.String(), `data-page="complete"`)
			if w.Code != 200 || reused != tc.wantReuse {
				t.Fatal("unexpected consent reuse", tc.name, w.Code, w.Body.String())
			}
			if tc.wantReuse {
				if !strings.Contains(w.Body.String(), "Return to app") || strings.Contains(w.Body.String(), "http-equiv=\"refresh\"") {
					t.Fatal("native completion changed")
				}
				for authID, auth := range a.data.Authorizations {
					if authID != priorID && (!auth.Grants.Read || auth.Grants.Write != (tc.scope == "read write" || tc.noHint) || auth.Grants.Schema || auth.Grants.Erase || auth.Administer) {
						t.Fatal("reuse returned broader grants", auth.Grants)
					}
				}
			} else if !strings.Contains(w.Body.String(), `data-page="consent"`) {
				t.Fatal("missing renewed review")
			}
		})
	}
}

func TestConsentReuseNeverRevivesOlderBroaderApproval(t *testing.T) {
	a, _, dataset := authFixture(t)
	callback := "com.example.notes:/callback"
	a.data.Clients["client"] = Client{ID: "client", Name: "Notes", Redirects: []string{callback}}
	a.data.Authorizations["older"] = Authorization{ID: "older", Principal: "person", Client: "client", Dataset: dataset, ConsentRedirect: callback, Grants: store.Grants{Read: true, Write: true}, Created: 1}
	a.data.Authorizations["newer"] = Authorization{ID: "newer", Principal: "person", Client: "client", Dataset: dataset, ConsentRedirect: callback, Grants: store.Grants{Read: true}, Created: 2}
	f := flow{Client: "client", Redirect: callback, Dataset: dataset, HasScope: true, Scope: "read write"}
	if _, reused, err := a.reuseConsent(t.Context(), f, a.data.Principals["person"], nil, "absent"); err != nil || reused {
		t.Fatal("older broader choice revived", err)
	}
}

func TestExplicitPrivilegedConsentReuse(t *testing.T) {
	a, _, dataset := authFixture(t)
	callback := "com.example.notes:/callback"
	a.data.Clients["client"] = Client{ID: "client", Name: "Notes", Redirects: []string{callback}}
	a.data.Authorizations["prior"] = Authorization{ID: "prior", Principal: "person", Client: "client", Dataset: dataset, ConsentRedirect: callback, Grants: store.Grants{Read: true, Schema: true, Erase: true}, Administer: true, Created: 1}
	f := flow{Client: "client", Redirect: callback, Dataset: dataset, HasScope: true, Scope: "read schema erase administer"}
	auth, reused, err := a.reuseConsent(t.Context(), f, a.data.Principals["person"], nil, "absent")
	if err != nil || !reused || !auth.Grants.Read || !auth.Grants.Schema || !auth.Grants.Erase || !auth.Administer || auth.Grants.Write {
		t.Fatal("explicit previously granted rights not preserved", err)
	}
}
