package identity

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/ekarpov/tessera/pkg/store"
)

func TestReconcileDatasetsPreservesLiveAuthorityAndDisablesRetiredPolicy(t *testing.T) {
	a, engine, live := authFixture(t)
	const gone = "retired"
	principal := a.data.Principals["person"]
	principal.Datasets[gone] = store.Grants{Read: true, Write: true, Erase: true}
	principal.SharedTypes = map[string][]string{gone: {"private"}, live: {"public"}}
	principal.InsertTypes = map[string][]string{gone: {"private"}}
	principal.FrozenReaders = map[string]ReaderFreeze{gone: {}}
	principal.GroupFreezes = map[string]ReaderFreeze{gone: {}}
	principal.ErasedWith = []string{gone, live}
	a.data.Principals["person"] = principal
	a.data.Owners = map[string]string{gone: "person", live: "person"}
	a.data.MadeBy = map[string]string{gone: "issuer"}
	a.data.Authorizations["gone"] = Authorization{Dataset: gone, Principal: "person"}
	a.data.Authorizations["live"] = Authorization{Dataset: live, Principal: "person"}
	a.data.Authorizations["admin"] = Authorization{Principal: "person", Administer: true}
	a.data.Authorizations["shared"] = Authorization{Principal: "person", SharedReads: true, SharedDatasets: []string{gone, live}}
	a.data.Issuers = map[string]Issuer{}
	a.data.Issuers["mixed"] = Issuer{ID: "mixed", Datasets: []string{gone, live}, Issued: 3}
	a.data.Issuers["live"] = Issuer{ID: "live", Datasets: []string{live}}
	a.data.DeviceCodes = map[string]DeviceCode{}
	a.data.DeviceCodes["gone"] = DeviceCode{Account: "person", Dataset: gone}
	a.data.DeviceCodes["server"] = DeviceCode{Account: "person", Administer: true}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	if err := a.ReconcileDatasets(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), a.path, a.issuer, engine)
	if err != nil {
		t.Fatal(err)
	}
	p := reopened.data.Principals["person"]
	if len(p.Datasets) != 1 || !p.Datasets[live].Write || len(p.SharedTypes) != 1 || len(p.InsertTypes) != 0 || len(p.FrozenReaders) != 0 || len(p.GroupFreezes) != 0 || !reflect.DeepEqual(p.ErasedWith, []string{live}) {
		t.Fatal("reconciliation changed live authority or retained stale references")
	}
	if reopened.data.Owners[live] != "person" || len(reopened.data.Owners) != 1 || len(reopened.data.MadeBy) != 0 {
		t.Fatal("stale ownership or template binding retained")
	}
	if !reopened.data.Authorizations["gone"].Revoked || reopened.data.Authorizations["live"].Revoked || reopened.data.Authorizations["admin"].Revoked || !reflect.DeepEqual(reopened.data.Authorizations["shared"].SharedDatasets, []string{live}) {
		t.Fatal("incorrect installation reconciliation")
	}
	issuer := reopened.data.Issuers["mixed"]
	if !issuer.Disabled || issuer.Issued != 3 || !reflect.DeepEqual(issuer.Datasets, []string{gone, live}) || reopened.data.Issuers["live"].Disabled {
		t.Fatal("fixed issuer policy changed or unrelated issuer disabled")
	}
	if _, ok := reopened.data.DeviceCodes["gone"]; ok {
		t.Fatal("retired device bootstrap survived")
	}
	if _, ok := reopened.data.DeviceCodes["server"]; !ok {
		t.Fatal("server-only bootstrap lost")
	}
	// An already reconciled startup must not need another registry write.
	reopened.path = t.TempDir()
	if err := reopened.ReconcileDatasets(t.Context()); err != nil {
		t.Fatal("idempotent reconciliation attempted persistence", err)
	}
}

type unavailableInventory struct{ Engine }

func (e unavailableInventory) Provisioned(context.Context) ([]store.Dataset, error) {
	return nil, errors.New("inventory unavailable")
}

func TestReconcileDatasetsInventoryFailurePreservesRegistry(t *testing.T) {
	a, _, live := authFixture(t)
	before, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	a.engine = unavailableInventory{a.engine}
	if err := a.ReconcileDatasets(t.Context()); err == nil {
		t.Fatal("failed inventory treated as empty")
	}
	after, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || !a.data.Principals["person"].Datasets[live].Write {
		t.Fatal("failed inventory removed authority")
	}
}

func TestReconcileDatasetsPersistenceFailureIsReported(t *testing.T) {
	a, _, _ := authFixture(t)
	p := a.data.Principals["person"]
	p.Datasets["retired"] = store.Grants{Read: true}
	a.data.Principals["person"] = p
	a.path = t.TempDir() // Atomic file replacement cannot replace this directory.
	if err := a.ReconcileDatasets(t.Context()); err == nil {
		t.Fatal("failed persistence reported success")
	}
}
