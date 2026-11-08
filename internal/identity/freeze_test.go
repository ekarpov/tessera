package identity

import (
	"testing"

	"github.com/ekarpov/tessera/pkg/store"
)

func TestReaderOwnerRecordedAtCreationAndCarriedAcrossErasure(t *testing.T) {
	a, engine, _ := authFixture(t)
	const dataset = "new-dataset"
	grants := store.Grants{Read: true, Write: true, Schema: true, Erase: true}
	if err := a.RecordCreation("person", dataset, grants); err != nil {
		t.Fatal(err)
	}
	if a.data.Owners[dataset] != "person" {
		t.Fatal("creator not recorded")
	}
	// Grant reduction and a replay must preserve both ownership and reduction.
	if err := a.GrantDataset("person", dataset, store.Grants{Read: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordCreation("person", dataset, grants); err != nil {
		t.Fatal(err)
	}
	if a.data.Principals["person"].Datasets[dataset].Write {
		t.Fatal("replay restored grants")
	}
	if err := a.RecordErasure(store.Access{Principal: "other"}, dataset, "replacement"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), a.path, a.issuer, engine)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.data.Owners["replacement"] != "person" || reopened.data.Owners[dataset] != "" {
		t.Fatal("erasure lost ownership")
	}
}

func TestUnfreezeRequiresRecordedOwnerOrNonAdministratorLegacyWriter(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Principals["writer"] = Principal{ID: "writer", Kind: "person", Datasets: map[string]store.Grants{dataset: {Write: true}}}
	for _, id := range []string{"person", "writer"} {
		a.data.Authorizations[id] = Authorization{Principal: id, Dataset: dataset, Grants: store.Grants{Write: true}}
	}
	for _, owner := range []string{"person", "writer", ""} {
		a.data.Owners = map[string]string{dataset: owner}
		for _, id := range []string{"person", "writer"} {
			access := store.Access{Principal: id, Authorization: id}
			want := owner == id || (owner == "" && id == "writer")
			if got := a.ownerInstallation(t.Context(), access, dataset); got != want {
				t.Fatalf("owner=%q account=%q: got %v want %v", owner, id, got, want)
			}
			if err := a.ReaderStanding(t.Context(), access, dataset); err != nil {
				t.Fatal("writer listing refused", err)
			}
		}
	}
	a.data.Owners = map[string]string{dataset: "person"}
	auth := a.data.Authorizations["person"]
	auth.Grants = store.Grants{Read: true}
	a.data.Authorizations["person"] = auth
	if a.ownerInstallation(t.Context(), store.Access{Principal: "person", Authorization: "person"}, dataset) {
		t.Fatal("owner without write approval accepted")
	}
}
