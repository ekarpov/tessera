package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	"github.com/ekarpov/tessera/internal/standalone"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// operator is a server on disk, driven the way an operator drives one: each
// command opens it, acts, and closes it again.
type operator struct {
	t      *testing.T
	dir    string
	config string
}

func newOperator(t *testing.T) *operator {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]any{"listen": "127.0.0.1:0", "issuer": "https://tessera.example", "data_dir": data,
		"tls_certificate": "unused.crt", "tls_key": "unused.key", "encrypted_storage_confirmed": true})
	o := &operator{t: t, dir: dir, config: filepath.Join(dir, "server.json")}
	o.write("server.json", string(config))
	return o
}

// write puts a private file where a command will read it.
func (o *operator) write(name, content string) string {
	o.t.Helper()
	path := filepath.Join(o.dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		o.t.Fatal(err)
	}
	return path
}

// inspect opens the server to look at what the commands did, and closes it.
func (o *operator) inspect(look func(*standalone.Runtime)) {
	o.t.Helper()
	config, err := standalone.ReadConfig(o.config)
	if err != nil {
		o.t.Fatal(err)
	}
	runtime, err := standalone.Open(context.Background(), config)
	if err != nil {
		o.t.Fatal(err)
	}
	defer runtime.Close()
	look(runtime)
}

func (o *operator) sole() string {
	o.t.Helper()
	var dataset string
	o.inspect(func(r *standalone.Runtime) {
		held, err := r.Engine.Provisioned(context.Background())
		if err != nil || len(held) != 1 {
			o.t.Fatal("the server should hold one dataset", err)
		}
		dataset = held[0].ID
	})
	return dataset
}

// The account commands do what they say to the account and to nothing else,
// and a deletion that would erase refuses until the datasets are named.
func TestTheAccountCommands(t *testing.T) {
	o := newOperator(t)
	secret := o.write("password", "correct horse battery staple\n")
	full := "read,write,schema,erase"
	if err := runStandalone(o.config, "owner", secret, "", "", "", "", "", "", "", "", full, false, false, false, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := runStandalone(o.config, "member", secret, "", "", "", "", "", "", "", "", "read", false, false, false, false, ""); err != nil {
		t.Fatal(err)
	}
	dataset := o.sole()

	if err := runAccountAction(o.config, "suspend", "member", "", ""); err != nil {
		t.Fatal(err)
	}
	o.inspect(func(r *standalone.Runtime) {
		if _, err := r.Authority.AddAutomation(context.Background(), "member", dataset); err == nil {
			t.Fatal("-suspend-account left the account able to sign in")
		}
	})
	if err := runAccountAction(o.config, "reinstate", "member", "", ""); err != nil {
		t.Fatal(err)
	}
	o.inspect(func(r *standalone.Runtime) {
		if _, err := r.Authority.AddAutomation(context.Background(), "member", dataset); err != nil {
			t.Fatal("-reinstate-account did not restore the account:", err)
		}
	})

	fresh := o.write("fresh-password", "a different password entirely\n")
	if err := runAccountAction(o.config, "reset-password", "member", fresh, ""); err != nil {
		t.Fatal(err)
	}
	o.inspect(func(r *standalone.Runtime) {
		// Asking for a recovery code proves a password.
		if _, err := r.Authority.IssueRecoveryCode(context.Background(), "member", "a different password entirely"); err != nil {
			t.Fatal("-reset-password did not set the new password:", err)
		}
		if _, err := r.Authority.IssueRecoveryCode(context.Background(), "member", "correct horse battery staple"); err == nil {
			t.Fatal("-reset-password left the old password working")
		}
	})

	// The owner is the dataset's only administrator: deleting them erases it,
	// and only when it is named.
	err := runAccountAction(o.config, "delete", "owner", "", "")
	if err == nil || !strings.Contains(err.Error(), "-erase-datasets "+dataset) {
		t.Fatal("deleting the last administrator did not name what it would erase:", err)
	}
	o.inspect(func(r *standalone.Runtime) {
		if len(r.Authority.DeletionErases("owner")) != 1 {
			t.Fatal("a refused deletion deleted the account")
		}
	})
	if err = runAccountAction(o.config, "delete", "owner", "", dataset); err != nil {
		t.Fatal("a confirmed deletion was refused:", err)
	}
	o.inspect(func(r *standalone.Runtime) {
		held, e := r.Engine.Provisioned(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		for _, d := range held {
			if d.ID == dataset {
				t.Fatal("-delete-account with -erase-datasets did not erase the dataset")
			}
		}
	})
	if err = runAccountAction(o.config, "suspend", "owner", "", ""); err == nil {
		t.Fatal("a deleted account could still be acted on")
	}
}

// The issuer commands change an issuer's cap and state and replace its
// secret, and leave its policy alone.
func TestTheIssuerCommands(t *testing.T) {
	o := newOperator(t)
	secret := o.write("password", "correct horse battery staple\n")
	if err := runStandalone(o.config, "owner", secret, "", "", "", "", "", "", "", "", "read,write,schema,erase", false, false, false, false, ""); err != nil {
		t.Fatal(err)
	}
	dataset := o.sole()
	policy := o.write("issuer.json", `{"datasets":["`+dataset+`"],"grants":"read","cap":2}`)
	first := filepath.Join(o.dir, "issuer-secret")
	if err := runAddIssuer(o.config, "portal", policy, first); err != nil {
		t.Fatal(err)
	}
	if err := runChangeIssuer(o.config, "portal", 7, "true"); err != nil {
		t.Fatal(err)
	}
	if err := runChangeIssuer(o.config, "portal", 0, "maybe"); err == nil {
		t.Fatal("-issuer-disabled accepted something other than true or false")
	}
	o.inspect(func(r *standalone.Runtime) {
		summaries := r.Authority.IssuerSummaries()
		if len(summaries) != 1 || summaries[0].Cap != 7 || !summaries[0].Disabled || summaries[0].Datasets[0] != dataset {
			t.Fatal("-change-issuer did not change exactly the cap and the state:", summaries)
		}
	})
	second := filepath.Join(o.dir, "issuer-secret-2")
	if err := runRotateIssuer(o.config, "portal", second); err != nil {
		t.Fatal(err)
	}
	was, _ := os.ReadFile(first)
	now, err := os.ReadFile(second)
	if err != nil || len(strings.TrimSpace(string(now))) == 0 || string(was) == string(now) {
		t.Fatal("-rotate-issuer-secret did not write a new secret")
	}
	if info, err := os.Stat(second); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("a rotated secret was written readable by others")
	}
	if err = runRotateIssuer(o.config, "portal", second); err == nil {
		t.Fatal("rotating a secret overwrote an existing file")
	}
	o.inspect(func(r *standalone.Runtime) {
		enabled := false
		if _, err := r.Authority.ChangeIssuer("portal", identity.IssuerChange{Disabled: &enabled}); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := r.Authority.IssueInvitation("portal", strings.TrimSpace(string(was)), ""); err == nil {
			t.Fatal("the old secret still issued an invitation after rotation")
		}
		if _, _, _, err := r.Authority.IssueInvitation("portal", strings.TrimSpace(string(now)), ""); err != nil {
			t.Fatal("the rotated secret did not issue:", err)
		}
	})
}

// -set-dataset-limits changes the members it names and keeps the others.
func TestTheLimitsCommand(t *testing.T) {
	o := newOperator(t)
	dataset := o.sole()
	var before p.Limits
	o.inspect(func(r *standalone.Runtime) {
		var err error
		if before, err = r.Engine.DatasetLimits(context.Background(), dataset); err != nil {
			t.Fatal(err)
		}
	})
	limits := o.write("limits.json", `{"max_materialized_bytes":134217728}`)
	if err := runSetDatasetLimits(o.config, dataset, limits); err != nil {
		t.Fatal(err)
	}
	o.inspect(func(r *standalone.Runtime) {
		after, err := r.Engine.DatasetLimits(context.Background(), dataset)
		if err != nil {
			t.Fatal(err)
		}
		if after.MaxMaterializedBytes != 134217728 || after.MaxScannedRows != before.MaxScannedRows {
			t.Fatal("-set-dataset-limits changed the wrong members:", after)
		}
	})
	// Lengthened, then brought back: the second is a shortening of a window
	// clients were promised, and refused.
	longer := o.write("longer.json", `{"idempotency_retention_seconds":172800}`)
	if err := runSetDatasetLimits(o.config, dataset, longer); err != nil {
		t.Fatal(err)
	}
	shorter := o.write("shorter.json", `{"idempotency_retention_seconds":100000}`)
	if err := runSetDatasetLimits(o.config, dataset, shorter); err == nil {
		t.Fatal("-set-dataset-limits shortened a promised retention window")
	}
}

// A service given a dataset of its own gets a new one that nobody else holds
// — not whatever single dataset storage happened to hold — and the operator
// learns its identity from a file rather than from the registry.
func TestAServiceCanBeGivenADatasetOfItsOwn(t *testing.T) {
	o := newOperator(t)
	secret := o.write("password", "correct horse battery staple\n")
	if err := runStandalone(o.config, "owner", secret, "", "", "", "", "", "", "", "", "read,write,schema,erase", false, false, false, false, ""); err != nil {
		t.Fatal(err)
	}
	shared := o.sole()
	serviceSecret := filepath.Join(o.dir, "console-secret")
	named := filepath.Join(o.dir, "console-dataset")
	if err := runStandalone(o.config, "", "", "console", serviceSecret, "", "", "", "", "", shared, "", false, false, false, true, named); err == nil {
		t.Fatal("-new-dataset was accepted together with -dataset")
	}
	if err := runStandalone(o.config, "", "", "console", serviceSecret, "", "", "", "", "", "", "", false, false, true, true, named); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(named)
	own := strings.TrimSpace(string(raw))
	if err != nil || own == "" || own == shared {
		t.Fatal("the service was not given a new dataset, or its identity was not written:", own, err)
	}
	if info, err := os.Stat(named); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("the dataset output was not private")
	}
	o.inspect(func(r *standalone.Runtime) {
		if r.Authority.Administers("owner")[own] {
			t.Fatal("the owner holds the service's dataset")
		}
		// Creating it installed full access for the handle that created it,
		// which must hold nothing afterwards.
		if reach, err := r.Engine.Reach(context.Background(), store.Access{Principal: "installer", Authorization: "installer"}); err != nil || len(reach) != 0 {
			t.Fatal("the handle that created the service's dataset still holds it:", reach, err)
		}
		key, _ := os.ReadFile(serviceSecret)
		ask := func(dataset string) int {
			req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader("grant_type=client_credentials&dataset="+dataset))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.SetBasicAuth("console", strings.TrimSpace(string(key)))
			w := httptest.NewRecorder()
			r.Handler.ServeHTTP(w, req)
			return w.Code
		}
		if got := ask(own); got != 200 {
			t.Fatal("the service could not reach its own dataset:", got)
		}
		if got := ask(shared); got != 400 {
			t.Fatal("the service reached the dataset it was not given:", got)
		}
	})
}

// A lost service secret is replaced from the command line; the old one stops.
func TestTheServiceSecretCommand(t *testing.T) {
	o := newOperator(t)
	first := filepath.Join(o.dir, "first")
	if err := runStandalone(o.config, "", "", "reader", first, "", "", "", "", "", "", "", false, false, true, true, ""); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(o.dir, "second")
	if err := runRotateService(o.config, "reader", second); err != nil {
		t.Fatal(err)
	}
	if err := runRotateService(o.config, "nobody", filepath.Join(o.dir, "third")); err == nil {
		t.Fatal("a secret was rotated for a service that does not exist")
	}
	was, _ := os.ReadFile(first)
	now, _ := os.ReadFile(second)
	if strings.TrimSpace(string(now)) == "" || string(now) == string(was) {
		t.Fatal("-rotate-service-secret did not write a new secret")
	}
}

// An owner given server rights and no grants holds no data: -grants' default
// is for somebody joining the storage's dataset, not for them.
func TestAnOwnerWithServerRightsHoldsNoDatasetByDefault(t *testing.T) {
	for _, c := range []struct {
		grants      string
		given       bool
		serverRight bool
		dataset     string
		want        string
	}{
		{"read,write", false, true, "", ""},
		{"read,write", true, true, "", "read,write"},
		{"read,write", false, true, "s-named", "read,write"},
		{"read,write", false, false, "", "read,write"},
	} {
		if got := personGrants(c.grants, c.given, c.serverRight, c.dataset); got != c.want {
			t.Fatal("wrong grants for", c, "got", got)
		}
	}
	// Through the command itself, as the flag default leaves it.
	o := newOperator(t)
	secret := o.write("password", "correct horse battery staple\n")
	if err := runStandalone(o.config, "owner", secret, "", "", "", "", "", "", "", "", personGrants("read,write", false, true, ""), true, true, false, false, ""); err != nil {
		t.Fatal(err)
	}
	shared := o.sole()
	o.inspect(func(r *standalone.Runtime) {
		// Holding anything on it, read included, would let this succeed.
		if _, err := r.Authority.AddAutomation(context.Background(), "owner", shared); err == nil {
			t.Fatal("the owner was given the storage's dataset")
		}
	})
}
