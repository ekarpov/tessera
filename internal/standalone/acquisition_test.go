package standalone

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ekarpov/tessera/internal/acquisition"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestAdministratorAcquiresExactPairOverHTTPS(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(dir, "backups")
	if err := os.Mkdir(backups, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{9}, 32)
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, key, 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	origin := "https://" + srv.Listener.Addr().String()
	runtime, err := Open(t.Context(), Config{Issuer: origin, DataDir: filepath.Join(dir, "data"), Engine: "sqlite", EncryptedStorage: true, Backup: &BackupConfig{Directory: backups, KeyFile: keyFile, Keep: 8, EveryHours: 24}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	srv.Config.Handler = runtime.Handler
	srv.StartTLS()
	defer srv.Close()
	client := srv.Client()
	client.Jar, _ = cookiejar.New(nil)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	call := func(method, path, body, bearer string, want int) ([]byte, http.Header) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, origin+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(path, "/oauth/") && path != "/oauth/register" && method == "POST" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("HTTPS request failed")
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s expected %d got %d", method, want, resp.StatusCode)
		}
		return raw, resp.Header
	}
	const password = "synthetic-administrator-password"
	if err = runtime.Authority.AddServerAdministrator("owner", password, sole(t, runtime.Engine), store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := call("POST", "/oauth/register", `{"client_name":"Acquisition integration","redirect_uris":["com.example:/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`, "", 201)
	var registration struct {
		ID string `json:"client_id"`
	}
	if json.Unmarshal(raw, &registration) != nil {
		t.Fatal("registration")
	}
	verifier := strings.Repeat("x", 43)
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {registration.ID}, "redirect_uri": {"com.example:/callback"}, "response_type": {"code"}, "state": {"acquisition"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "dataset": {sole(t, runtime.Engine)}}
	hidden := func(raw []byte, name string) string {
		t.Helper()
		m := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindSubmatch(raw)
		if len(m) != 2 {
			t.Fatal("missing OAuth form field")
		}
		return string(m[1])
	}
	raw, _ = call("GET", "/oauth/authorize?"+query.Encode(), "", "", 200)
	signin := url.Values{"flow": {hidden(raw, "flow")}, "csrf": {hidden(raw, "csrf")}, "username": {"owner"}, "password": {password}, "decision": {"continue"}}
	raw, _ = call("POST", "/oauth/authorize", signin.Encode(), "", 200)
	consent := url.Values{"flow": {hidden(raw, "flow")}, "csrf": {hidden(raw, "csrf")}, "dataset": {hidden(raw, "dataset")}, "decision": {"allow"}, "administer": {"yes"}}
	_, headers := call("POST", "/oauth/authorize", consent.Encode(), "", 303)
	target, err := url.Parse(headers.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	exchange := url.Values{"grant_type": {"authorization_code"}, "client_id": {registration.ID}, "redirect_uri": {"com.example:/callback"}, "code": {target.Query().Get("code")}, "code_verifier": {verifier}}
	raw, _ = call("POST", "/oauth/token", exchange.Encode(), "", 200)
	var tokens oauthTokens
	if json.Unmarshal(raw, &tokens) != nil || tokens.Access == "" {
		t.Fatal("token")
	}

	// Provision through person-authorized administration, then discover the native
	// scheduler's real output exclusively through the supported collector routes.
	collectorPath := "/admin/api/v1/backup-collectors/" + strings.Repeat("c", 32)
	raw, _ = call("PUT", collectorPath, `{"expires_in_seconds":3600}`, tokens.Access, 201)
	var collector acquisition.CollectorCredential
	if json.Unmarshal(raw, &collector) != nil || collector.Token == "" {
		t.Fatal("collector issuance")
	}
	call("PUT", collectorPath, `{"expires_in_seconds":3600}`, tokens.Access, 409)
	runtime.StartBackups(t.Context())
	catalogPath := "/admin/api/v1/backup-operations"
	var catalog acquisition.Catalog
	scheduledDeadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ = call("GET", catalogPath+"?limit=1", "", collector.Token, 200)
		if json.Unmarshal(raw, &catalog) != nil {
			t.Fatal("catalog")
		}
		if len(catalog.Results) > 0 {
			break
		}
		if time.Now().After(scheduledDeadline) {
			t.Fatal("scheduled result not discoverable")
		}
		time.Sleep(time.Millisecond)
	}
	scheduledID := catalog.Results[0].ID
	call("GET", catalogPath+"/"+scheduledID, "", collector.Token, 200)
	raw, _ = call("POST", "/admin/api/v1/backups", "", tokens.Access, 201)
	var legacy struct {
		Backup string `json:"backup"`
	}
	if json.Unmarshal(raw, &legacy) != nil || legacy.Backup == "" {
		t.Fatal("legacy backup")
	}
	id := acquisition.NewID()
	path := "/admin/api/v1/backup-operations/" + id
	call("PUT", path, `{"kind":"backup"}`, "", 401)
	call("PUT", path, `{"kind":"backup"}`, tokens.Access, 202)
	call("PUT", path, `{"kind":"evidence"}`, tokens.Access, 409)
	var op acquisition.Operation
	deadline := time.After(5 * time.Second)
	for {
		raw, _ = call("GET", path, "", tokens.Access, 200)
		if json.Unmarshal(raw, &op) != nil {
			t.Fatal("status")
		}
		if op.State != "running" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("acquisition timed out")
		case <-time.After(time.Millisecond):
		}
	}
	if op.State != "complete" || op.Pair == nil || len(op.Artifacts) != 3 {
		t.Fatal("pair not complete")
	}
	raw, _ = call("GET", "/admin/api/v1/overview", "", tokens.Access, 200)
	var overview struct {
		Server struct {
			LastBackupAt *time.Time `json:"last_backup_at"`
		} `json:"server"`
	}
	if json.Unmarshal(raw, &overview) != nil || overview.Server.LastBackupAt == nil || !overview.Server.LastBackupAt.Equal(runtime.Authority.LastBackup()) {
		t.Fatal("asynchronous API publication missing from overview")
	}
	call("PUT", path, `{"kind":"backup"}`, tokens.Access, 202)
	const lease = "11111111111111111111111111111111"
	call("PUT", path+"/leases/"+lease, "", collector.Token, 200)
	for _, artifact := range op.Artifacts {
		call("GET", path+"/artifacts/"+artifact.ID, "", tokens.Access, 410)
		req, _ := http.NewRequestWithContext(t.Context(), "GET", origin+path+"/artifacts/"+artifact.ID, nil)
		req.Header.Set("Authorization", "Bearer "+collector.Token)
		req.Header.Set("Tessera-Backup-Lease", lease)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		hash := sha256.Sum256(raw)
		if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/octet-stream" || int64(len(raw)) != artifact.Size || hex.EncodeToString(hash[:]) != artifact.SHA256 {
			t.Fatal("artifact transfer differs from receipt")
		}
		if artifact.Kind == "metadata" {
			if _, err = acquisition.Verify(raw, key, op.Pair.Deployment); err != nil {
				t.Fatal(err)
			}
		}
	}
	call("PUT", path+"/leases/"+lease, "", collector.Token, 200)
	call("GET", path+"/artifacts/"+strings.Repeat("e", 32), "", collector.Token, 410)
	call("DELETE", path+"/leases/"+lease, "", collector.Token, 200)
	evidenceID := acquisition.NewID()
	evidencePath := catalogPath + "/" + evidenceID
	call("PUT", evidencePath, `{"kind":"evidence"}`, tokens.Access, 202)
	evidenceDeadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ = call("GET", evidencePath, "", tokens.Access, 200)
		var evidence acquisition.Operation
		if json.Unmarshal(raw, &evidence) != nil {
			t.Fatal("evidence status")
		}
		if evidence.State == "complete" {
			break
		}
		if evidence.State != "running" || time.Now().After(evidenceDeadline) {
			t.Fatal("evidence failed")
		}
		time.Sleep(time.Millisecond)
	}
	raw, _ = call("GET", catalogPath, "", collector.Token, 200)
	if json.Unmarshal(raw, &catalog) != nil {
		t.Fatal("rescan")
	}
	found := map[string]bool{}
	foundLegacy := false
	for _, result := range catalog.Results {
		found[result.ID] = true
		for _, artifact := range result.Artifacts {
			if artifact.ID == legacy.Backup {
				foundLegacy = true
			}
		}
	}
	if !found[scheduledID] || !found[id] || !found[evidenceID] || !foundLegacy {
		t.Fatal("catalog omitted a producer")
	}
	call("GET", evidencePath, "", collector.Token, 200)
	// The collector cannot create/cancel operations or acquire general authority.
	call("PUT", catalogPath+"/"+acquisition.NewID(), `{"kind":"backup"}`, collector.Token, 403)
	call("PUT", catalogPath+"/"+acquisition.NewID(), `{"kind":"evidence"}`, collector.Token, 403)
	call("DELETE", path, "", collector.Token, 403)
	call("GET", "/admin/api/v1/overview", "", collector.Token, 401)
	call("GET", "/admin/api/v1/backup-scheduler", "", collector.Token, 401)
	call("GET", "/admin/api/v1/backup-settings-operations/unknown", "", collector.Token, 401)
	call("PUT", "/admin/api/v1/backup-settings-operations/unknown", `{"every_hours":1,"keep":1,"expected_revision":"unknown"}`, collector.Token, 401)
	call("POST", "/admin/api/v1/backups", "", collector.Token, 401)
	call("PUT", collectorPath, `{"expires_in_seconds":3600}`, collector.Token, 401)
	call("DELETE", collectorPath, "", collector.Token, 401)
	call("GET", "/v1/datasets/"+sole(t, runtime.Engine)+"/meta", "", collector.Token, 401)
	call("GET", catalogPath+"?limit=101", "", collector.Token, 400)
	call("GET", catalogPath+"?cursor=invalid", "", collector.Token, 410)
	call("DELETE", collectorPath, "", tokens.Access, 204)
	call("GET", catalogPath, "", collector.Token, 401)
	call("GET", path, "", collector.Token, 401)
	call("PUT", path+"/leases/"+lease, "", collector.Token, 401)
	call("GET", path+"/artifacts/"+op.Artifacts[0].ID, "", collector.Token, 401)
	// Revocation never disables production. Drive the next immediate attempt
	// through explicit disable/enable commands; StartBackups is now idempotent.
	update := func(hours int) {
		t.Helper()
		raw, _ := call("GET", "/admin/api/v1/backup-scheduler", "", tokens.Access, 200)
		var observed acquisition.SchedulerObservation
		if json.Unmarshal(raw, &observed) != nil || observed.Settings == nil || observed.SettingsScope == nil {
			t.Fatal("scheduler observation")
		}
		requestID := *observed.SettingsScope + "." + acquisition.NewID()
		payload, _ := json.Marshal(acquisition.SettingsPayload{ExpectedRevision: observed.Settings.Revision, EveryHours: hours, Keep: 8})
		settingPath := "/admin/api/v1/backup-settings-operations/" + requestID
		call("PUT", settingPath, string(payload), tokens.Access, 202)
		deadline := time.Now().Add(5 * time.Second)
		for {
			raw, _ = call("GET", settingPath, "", tokens.Access, 200)
			var op acquisition.SettingsOperation
			if json.Unmarshal(raw, &op) != nil {
				t.Fatal("settings status")
			}
			if op.State == "effective" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("settings did not become effective")
			}
			time.Sleep(time.Millisecond)
		}
		call("PUT", settingPath, string(payload), tokens.Access, 200)
	}
	update(0)
	update(24)
	afterRevoke := time.Now().Add(5 * time.Second)
	for {
		raw, _ = call("GET", catalogPath, "", tokens.Access, 200)
		var after acquisition.Catalog
		if json.Unmarshal(raw, &after) != nil {
			t.Fatal("admin catalog")
		}
		if len(after.Results) > len(catalog.Results) {
			break
		}
		if time.Now().After(afterRevoke) {
			t.Fatal("revocation stopped scheduler")
		}
		time.Sleep(time.Millisecond)
	}
	// Save exactly the public export, including its timestamp, for the actual
	// standalone bootstrap file path below.
	raw, _ = call("GET", "/admin/api/v1/backup-scheduler", "", tokens.Access, 200)
	var exported struct {
		SettingsExport json.RawMessage `json:"settings_export"`
	}
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatal(err)
	}
	var before acquisition.SchedulerObservation
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	// Fresh authentication and standing are required on observations, retries and
	// status lookups, even after these same credentials admitted settings.
	guarded := []struct{ method, path, body string }{
		{"GET", "/admin/api/v1/backup-scheduler", ""},
		{"PUT", "/admin/api/v1/backup-settings-operations/unknown", `{}`},
		{"GET", "/admin/api/v1/backup-settings-operations/unknown", ""},
	}
	if err := runtime.Authority.SuspendAccount("owner"); err != nil {
		t.Fatal(err)
	}
	for _, g := range guarded {
		call(g.method, g.path, g.body, tokens.Access, 401)
	}
	if err := runtime.Authority.ReinstateAccount("owner"); err != nil {
		t.Fatal(err)
	}
	access, err := runtime.Authority.AdministrationAccess(t.Context(), tokens.Access)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Authority.RemoveOwnInstallation(t.Context(), access, access.Authorization); err != nil {
		t.Fatal(err)
	}
	for _, g := range guarded {
		call(g.method, g.path, g.body, tokens.Access, 401)
		call(g.method, g.path, g.body, "", 401)
	}

	config := runtime.Config
	runtime.Close()
	bootstrap := filepath.Join(dir, "settings-export.json")
	if err := os.WriteFile(bootstrap, exported.SettingsExport, 0600); err != nil {
		t.Fatal(err)
	}
	config.BackupSettingsBootstrap = bootstrap
	config.Backup.EveryHours, config.Backup.Keep = 0, 1
	if opened, err := Open(t.Context(), config); err == nil {
		opened.Close()
		t.Fatal("bootstrap replaced an existing settings journal")
	}
	journal := filepath.Join(backups, ".acquisitions", "settings.json")
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	var wrong acquisition.SettingsExport
	if err := json.Unmarshal(exported.SettingsExport, &wrong); err != nil {
		t.Fatal(err)
	}
	wrong.Deployment = strings.Repeat("0", 32)
	bad, _ := json.Marshal(wrong)
	if err := os.WriteFile(bootstrap, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(t.Context(), config); err == nil {
		opened.Close()
		t.Fatal("bootstrap accepted a different producer identity")
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal("refused bootstrap created a journal")
	}
	var original acquisition.SettingsExport
	if err := json.Unmarshal(exported.SettingsExport, &original); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*acquisition.SettingsExport){
		"version":         func(e *acquisition.SettingsExport) { e.Version = 2 },
		"negative hours":  func(e *acquisition.SettingsExport) { e.EveryHours = -1 },
		"excessive hours": func(e *acquisition.SettingsExport) { e.EveryHours = 87601 },
		"zero keep":       func(e *acquisition.SettingsExport) { e.Keep = 0 },
		"excessive keep":  func(e *acquisition.SettingsExport) { e.Keep = 1025 },
		"revision":        func(e *acquisition.SettingsExport) { e.Revision = "invalid" },
		"zero timestamp":  func(e *acquisition.SettingsExport) { e.ExportedAt = time.Time{} },
	} {
		invalid := original
		mutate(&invalid)
		raw, _ := json.Marshal(invalid)
		if err := os.WriteFile(bootstrap, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if opened, err := Open(t.Context(), config); err == nil {
			opened.Close()
			t.Fatal("accepted invalid export", name)
		}
		if _, err := os.Stat(journal); !os.IsNotExist(err) {
			t.Fatal("invalid export created journal", name)
		}
	}
	if err := os.WriteFile(bootstrap, exported.SettingsExport, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal("standalone rejected the public settings export:", err)
	}
	after := restored.Acquisitions.SchedulerObservation()
	if after.Settings.EveryHours != before.Settings.EveryHours || after.Settings.Keep != before.Settings.Keep || after.Settings.Revision == before.Settings.Revision || *after.SettingsScope == *before.SettingsScope {
		t.Fatal("bootstrap did not restore values with a fresh revision and epoch")
	}
	oldID := *before.SettingsScope + "." + acquisition.NewID()
	if _, err := restored.Acquisitions.SettingsStatus(oldID); p.ErrorCode(err) != "conflict" {
		t.Fatal("old status scope accepted", err)
	}
	if _, err := restored.Acquisitions.SubmitSettings(oldID, acquisition.SettingsPayload{ExpectedRevision: before.Settings.Revision, EveryHours: 0, Keep: 1}); p.ErrorCode(err) != "conflict" {
		t.Fatal("old command scope accepted", err)
	}
	restored.Close()
	config.BackupSettingsBootstrap = ""
	config.Backup.EveryHours, config.Backup.Keep = 0, 1
	restarted, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	persisted := restarted.Acquisitions.SchedulerObservation()
	if *persisted.Settings != *after.Settings || *persisted.SettingsScope != *after.SettingsScope {
		t.Fatal("restored settings did not persist across restart")
	}

}
