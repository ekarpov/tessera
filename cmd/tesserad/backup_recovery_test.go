package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ekarpov/tessera/internal/acquisition"
	"github.com/ekarpov/tessera/internal/standalone"
	"github.com/ekarpov/tessera/pkg/store"
)

type recoveryHTTP struct {
	t      *testing.T
	client *http.Client
	origin string
}

func (h recoveryHTTP) call(method, path, body, token string, status int) ([]byte, http.Header) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), method, h.origin+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if method == "POST" && strings.HasPrefix(path, "/oauth/") && path != "/oauth/register" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal("HTTPS request failed", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != status {
		h.t.Fatalf("%s %s: expected %d got %d", method, path, status, resp.StatusCode)
	}
	return raw, resp.Header
}
func (h recoveryHTTP) login() string {
	h.t.Helper()
	h.client.Jar, _ = cookiejar.New(nil)
	h.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	raw, _ := h.call("POST", "/oauth/register", `{"client_name":"Recovery test","redirect_uris":["com.example:/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`, "", 201)
	var reg struct {
		ID string `json:"client_id"`
	}
	if json.Unmarshal(raw, &reg) != nil {
		h.t.Fatal("registration")
	}
	verifier := strings.Repeat("x", 43)
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {reg.ID}, "redirect_uri": {"com.example:/callback"}, "response_type": {"code"}, "state": {"recovery"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}}
	hidden := func(raw []byte, name string) string {
		m := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindSubmatch(raw)
		if len(m) != 2 {
			h.t.Fatal("missing OAuth field", name)
		}
		return string(m[1])
	}
	raw, _ = h.call("GET", "/oauth/authorize?"+query.Encode(), "", "", 200)
	raw, _ = h.call("POST", "/oauth/authorize", url.Values{"flow": {hidden(raw, "flow")}, "csrf": {hidden(raw, "csrf")}, "username": {"owner"}, "password": {"synthetic-recovery-password"}, "decision": {"continue"}}.Encode(), "", 200)
	_, headers := h.call("POST", "/oauth/authorize", url.Values{"flow": {hidden(raw, "flow")}, "csrf": {hidden(raw, "csrf")}, "decision": {"allow"}, "administer": {"yes"}}.Encode(), "", 303)
	target, err := url.Parse(headers.Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	raw, _ = h.call("POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {reg.ID}, "redirect_uri": {"com.example:/callback"}, "code": {target.Query().Get("code")}, "code_verifier": {verifier}}.Encode(), "", 200)
	var token struct {
		Access string `json:"access_token"`
	}
	if json.Unmarshal(raw, &token) != nil || token.Access == "" {
		h.t.Fatal("token")
	}
	return token.Access
}
func (h recoveryHTTP) observation(token string) acquisition.SchedulerObservation {
	h.t.Helper()
	raw, _ := h.call("GET", "/admin/api/v1/backup-scheduler", "", token, 200)
	var got acquisition.SchedulerObservation
	if err := json.Unmarshal(raw, &got); err != nil || got.SettingsExport == nil {
		h.t.Fatal("scheduler observation", err)
	}
	return got
}

// This journey saves only public exports and operator-provided config/identity.
// It never copies, deletes or edits a private journal to make recovery succeed.
func TestBackupProducerRecoveryCLI(t *testing.T) {
	dir := t.TempDir()
	write := func(path string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	normal, faulting := filepath.Join(dir, "tesserad"), filepath.Join(dir, "tesserad-faults")
	for _, build := range []struct{ path, tags string }{{normal, ""}, {faulting, "devfaults"}} {
		args := []string{"build", "-o", build.path}
		if build.tags != "" {
			args = append(args, "-tags", build.tags)
		}
		args = append(args, ".")
		if out, err := exec.CommandContext(t.Context(), "go", args...).CombinedOutput(); err != nil {
			t.Fatalf("build CLI: %v %s", err, out)
		}
	}
	sourceBackups := filepath.Join(dir, "source-backups")
	if err := os.Mkdir(sourceBackups, 0700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "backup.key")
	write(keyFile, bytes.Repeat([]byte{7}, 32))
	source := httptest.NewUnstartedServer(nil)
	sourceRuntime, err := standalone.Open(t.Context(), standalone.Config{Issuer: "https://" + source.Listener.Addr().String(), DataDir: filepath.Join(dir, "source-data"), Engine: "sqlite", EncryptedStorage: true, Backup: &standalone.BackupConfig{Directory: sourceBackups, KeyFile: keyFile, EveryHours: 0, Keep: 8}})
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRuntime.Close()
	if err := sourceRuntime.Authority.AddServerAdministrator("owner", "synthetic-recovery-password", "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	source.Config.Handler = sourceRuntime.Handler
	source.StartTLS()
	defer source.Close()
	h := recoveryHTTP{t, source.Client(), source.URL}
	sourceToken := h.login()
	independentlyRetained := h.observation(sourceToken).SettingsExport.Deployment
	before := h.observation(sourceToken)
	// Install an acknowledged policy through the supported route before export.
	id := *before.SettingsScope + "." + acquisition.NewID()
	payload, _ := json.Marshal(acquisition.SettingsPayload{ExpectedRevision: before.Settings.Revision, EveryHours: 0, Keep: 7})
	h.call("PUT", "/admin/api/v1/backup-settings-operations/"+id, string(payload), sourceToken, 202)
	deadline := time.Now().Add(5 * time.Second)
	for {
		before = h.observation(sourceToken)
		if before.Settings.Keep == 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("policy did not become effective")
		}
		time.Sleep(time.Millisecond)
	}
	raw, _ := h.call("GET", "/admin/api/v1/backup-scheduler", "", sourceToken, 200)
	var response struct {
		Export json.RawMessage `json:"settings_export"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(dir, "export.json")
	write(exportPath, response.Export)
	collectorPath := "/admin/api/v1/backup-collectors/" + strings.Repeat("c", 32)
	collectorRaw, _ := h.call("PUT", collectorPath, `{"expires_in_seconds":3600}`, sourceToken, 201)
	var collector acquisition.CollectorCredential
	if err := json.Unmarshal(collectorRaw, &collector); err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	write(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: source.TLS.Certificates[0].Certificate[0]}))
	privateKey, err := x509.MarshalPKCS8PrivateKey(source.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	write(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}))
	// Each destination is fresh. Only public configuration and export files are created.
	destination := func(name string) (string, string, string) {
		t.Helper()
		base := filepath.Join(dir, name)
		if err := os.Mkdir(base, 0700); err != nil {
			t.Fatal(err)
		}
		backups := filepath.Join(base, "backups")
		if err := os.Mkdir(backups, 0700); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		listener.Close()
		configPath := filepath.Join(base, "config.json")
		config, _ := json.Marshal(map[string]any{"listen": address, "issuer": "https://" + address, "data_dir": filepath.Join(base, "data"), "tls_certificate": certPath, "tls_key": keyPath, "encrypted_storage_confirmed": true, "backup": map[string]any{"directory": backups, "key_file": keyFile, "every_hours": 24, "keep": 1}})
		write(configPath, config)
		return configPath, backups, "https://" + address
	}
	invoke := func(binary, config, stage string, args ...string) error {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), binary, append([]string{"-config", config}, args...)...)
		cmd.Env = append(os.Environ(), "TESSERA_DEV_RECOVERY_INTERRUPT="+stage)
		return cmd.Run()
	}
	recoveryArgs := []string{"-recover-backup-producer", "-expected-backup-producer", independentlyRetained, "-backup-settings-bootstrap", exportPath}
	config, _, origin := destination("recovered")
	if err := invoke(normal, config, "intent", recoveryArgs...); err != nil {
		t.Fatal("normal binary must ignore devfault injection", err)
	}
	// Completed retry confirms the existing result before ordinary use creates inventory.
	if err := invoke(normal, config, "", recoveryArgs...); err != nil {
		t.Fatal("completed retry", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(config), "data")); !os.IsNotExist(err) {
		t.Fatal("offline recovery opened record storage")
	}
	password := filepath.Join(dir, "password")
	write(password, []byte("synthetic-recovery-password"))
	if err := invoke(normal, config, "", "-add-person", "owner", "-password-file", password, "-may-administer-server"); err != nil {
		t.Fatal("fresh administrator provisioning", err)
	}
	start := func(configPath, serverOrigin string) func() {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), normal, "-config", configPath)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		stopped := false
		stop := func() {
			if !stopped {
				stopped = true
				_ = cmd.Process.Signal(syscall.SIGTERM)
				_ = cmd.Wait()
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, err := h.client.Get(serverOrigin + "/.well-known/oauth-authorization-server")
			if err == nil {
				resp.Body.Close()
				break
			}
			if time.Now().After(deadline) {
				stop()
				t.Fatal("CLI did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
		return stop
	}
	stop := start(config, origin)
	restoredHTTP := recoveryHTTP{t, source.Client(), origin}
	token := restoredHTTP.login()
	got := restoredHTTP.observation(token)
	if got.SettingsExport.Deployment != independentlyRetained || got.Settings.EveryHours != 0 || got.Settings.Keep != 7 || got.Settings.Revision == before.Settings.Revision || *got.SettingsScope == *before.SettingsScope || got.PendingRequestID != nil {
		t.Fatal("recovery values/lineage/scope")
	}
	restoredHTTP.call("GET", "/admin/api/v1/backup-scheduler", "", sourceToken, 401)
	restoredHTTP.call("GET", "/admin/api/v1/backup-operations", "", collector.Token, 401)
	restoredHTTP.call("GET", "/admin/api/v1/backup-settings-operations/"+id, "", token, 409)
	restoredHTTP.call("PUT", "/admin/api/v1/backup-settings-operations/"+id, string(payload), token, 409)
	// New destination authority explicitly reprovisions/rebinds a collector.
	restoredHTTP.call("PUT", collectorPath, `{"expires_in_seconds":3600}`, token, 201)
	stop()
	stop = start(config, origin)
	persisted := restoredHTTP.observation(token)
	if *persisted.Settings != *got.Settings || *persisted.SettingsScope != *got.SettingsScope {
		t.Fatal("recovery did not survive CLI restart")
	}
	stop()
	if err := invoke(normal, config, "", recoveryArgs...); err == nil {
		t.Fatal("completed replay overwrote subsequently initialized state")
	}
	for _, stage := range []string{"marker", "intent", "deployment.json", "settings.json", "complete"} {
		t.Run("interrupted-"+stage, func(t *testing.T) {
			config, _, _ := destination("interrupted-" + strings.ReplaceAll(stage, ".", "-"))
			if err := invoke(faulting, config, stage, recoveryArgs...); err == nil {
				t.Fatal("fault did not interrupt")
			}
			if stage != "complete" {
				// A provisioning action opens the ordinary runtime; it must not default
				// its way past incomplete recovery or start accepting work.
				if err := invoke(normal, config, "", "-add-person", "blocked", "-password-file", password, "-may-administer-server"); err == nil {
					t.Fatal("startup accepted incomplete recovery")
				}
			}

			if stage != "marker" {
				altered := *before.SettingsExport
				altered.ExportedAt = altered.ExportedAt.Add(time.Hour)
				bad, _ := json.Marshal(altered)
				path := filepath.Join(filepath.Dir(config), "different-export.json")
				write(path, bad)
				args := []string{"-recover-backup-producer", "-expected-backup-producer", independentlyRetained, "-backup-settings-bootstrap", path}
				if err := invoke(normal, config, "", args...); err == nil {
					t.Fatal("interrupted retry accepted another full export")
				}
			}
			wrongArgs := []string{"-recover-backup-producer", "-expected-backup-producer", strings.Repeat("f", 32), "-backup-settings-bootstrap", exportPath}
			if err := invoke(normal, config, "", wrongArgs...); err == nil {
				t.Fatal("interrupted retry accepted another expected identity")
			}
			if err := invoke(normal, config, "", recoveryArgs...); err != nil {
				t.Fatal("same CLI retry failed", err)
			}
			if err := invoke(normal, config, "", recoveryArgs...); err != nil {
				t.Fatal("completed CLI retry failed", err)
			}
		})
	}

	for name, input := range map[string]string{
		"unknown":        strings.Replace(string(response.Export), `"version":1`, `"extra":1,"version":1`, 1),
		"duplicate":      strings.Replace(string(response.Export), `"keep":7`, `"keep":7,"keep":8`, 1),
		"missing hours":  strings.Replace(string(response.Export), `"every_hours":0,`, ``, 1),
		"null keep":      strings.Replace(string(response.Export), `"keep":7`, `"keep":null`, 1),
		"timestamp":      strings.Replace(string(response.Export), before.SettingsExport.ExportedAt.Format(time.RFC3339Nano), "invalid-time", 1),
		"invalid hours":  strings.Replace(string(response.Export), `"every_hours":0`, `"every_hours":-1`, 1),
		"wrong identity": strings.Replace(string(response.Export), independentlyRetained, strings.Repeat("f", 32), 1),
	} {
		t.Run("invalid-"+name, func(t *testing.T) {
			// Rebuild timestamp case from the exact public bytes, whose observation
			// timestamp is later than the earlier typed observation.
			if name == "timestamp" {
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(response.Export, &fields)
				fields["exported_at"] = json.RawMessage(`"invalid-time"`)
				bad, _ := json.Marshal(fields)
				input = string(bad)
			}
			config, backups, _ := destination("invalid-" + strings.ReplaceAll(name, " ", "-"))
			badPath := filepath.Join(filepath.Dir(config), "bad-export.json")
			write(badPath, []byte(input))
			args := []string{"-recover-backup-producer", "-expected-backup-producer", independentlyRetained, "-backup-settings-bootstrap", badPath}
			if err := invoke(normal, config, "", args...); err == nil {
				t.Fatal("CLI accepted invalid export")
			}
			if _, err := os.Lstat(filepath.Join(backups, ".acquisitions")); !os.IsNotExist(err) {
				t.Fatal("invalid CLI input initialized destination")
			}
			if err := invoke(normal, config, "", recoveryArgs...); err != nil {
				t.Fatal("invalid input prevented later valid recovery", err)
			}
		})
	}

	// Initialize a separate destination through ordinary CLI provisioning, then
	// prove both a conflicting identity and its own matching export cannot replace it.
	otherConfig, _, otherOrigin := destination("already-initialized")
	if err := invoke(normal, otherConfig, "", "-add-person", "owner", "-password-file", password, "-may-administer-server"); err != nil {
		t.Fatal(err)
	}
	stopOther := start(otherConfig, otherOrigin)
	otherHTTP := recoveryHTTP{t, source.Client(), otherOrigin}
	otherToken := otherHTTP.login()
	otherBefore := otherHTTP.observation(otherToken)
	stopOther()
	if err := invoke(normal, otherConfig, "", recoveryArgs...); err == nil {
		t.Fatal("CLI replaced conflicting initialized producer")
	}
	otherExport, _ := json.Marshal(otherBefore.SettingsExport)
	otherPath := filepath.Join(filepath.Dir(otherConfig), "own-export.json")
	write(otherPath, otherExport)
	ownArgs := []string{"-recover-backup-producer", "-expected-backup-producer", otherBefore.SettingsExport.Deployment, "-backup-settings-bootstrap", otherPath}
	if err := invoke(normal, otherConfig, "", ownArgs...); err == nil {
		t.Fatal("CLI replaced existing settings journal even with matching identity")
	}
	stopOther = start(otherConfig, otherOrigin)
	otherAfter := otherHTTP.observation(otherToken)
	stopOther()
	if otherAfter.SettingsExport.Deployment != otherBefore.SettingsExport.Deployment || *otherAfter.Settings != *otherBefore.Settings || *otherAfter.SettingsScope != *otherBefore.SettingsScope {
		t.Fatal("refused CLI recovery changed existing producer/settings")
	}
	for _, args := range [][]string{{"-add-person", "other"}, {"-restore", "unused"}, {"-dev"}, {"-version"}, {"-grants", "read"}} {
		if err := invoke(normal, config, "", append(append([]string{}, recoveryArgs...), args...)...); err == nil {
			t.Fatal("recovery accepted conflicting action", args[0])
		}
	}
}
