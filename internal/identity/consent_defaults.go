package identity

import (
	"strings"

	"github.com/ekarpov/tessera/pkg/store"
)

// With a script-free dataset chooser, a preselection must be safe for every
// offered dataset, including a later choice. The final submitted consent is
// still intersected with the chosen dataset's current grants by newAuthorization.
func consentDefaults(f flow, principal Principal, offered []datasetOption) (store.Grants, string) {
	current := func(id string) store.Grants {
		g := principal.Datasets[id]
		if !g.Current() {
			return store.Grants{}
		}
		return g
	}
	held := current(f.Dataset)
	if f.Dataset == "" && len(offered) > 0 {
		held = current(offered[0].ID)
		for _, d := range offered[1:] {
			g := current(d.ID)
			held = store.Grants{Read: held.Read && g.Read, Write: held.Write && g.Write,
				Insert: held.Insert && g.Insert, Schema: held.Schema && g.Schema, Erase: held.Erase && g.Erase}
		}
	}
	if !f.HasScope {
		return store.Grants{Read: held.Read, Insert: held.Insert}, ""
	}
	defaults := store.Grants{}
	missing := []string{}
	seen := map[string]bool{}
	for _, name := range strings.Split(f.Scope, " ") {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		available := false
		switch name {
		case "read":
			available, defaults.Read = held.Read, held.Read
		case "write":
			available, defaults.Write = held.Write, held.Write
		case "insert":
			available, defaults.Insert = held.Insert, held.Insert
		case "schema":
			available = held.Schema
		case "erase":
			available = held.Erase
		case "administer":
			available = principal.MayAdministerServer || len(administered(principal)) > 0
		case "create_dataset":
			available = principal.MayCreateDataset
		case "shared_reads":
			available = f.SharedReads
		}
		if !available {
			missing = append(missing, name)
		}
	}
	return defaults, strings.Join(missing, ", ")
}

func consentSummary(name string, g store.Grants) string {
	actions := []string{}
	if g.Read {
		actions = append(actions, "see your data")
	}
	if g.Write {
		actions = append(actions, "add, change and delete entries")
	}
	if g.Insert {
		actions = append(actions, "add entries where you contribute")
	}
	if len(actions) == 0 {
		return name + " has no data permissions selected."
	}
	return name + " will " + strings.Join(actions, " and ") + "."
}
