// tesserad composes the synthetic development or HTTPS self-hosted deployment.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ekarpov/tessera/internal/devfaults"
	"github.com/ekarpov/tessera/internal/devserver"
	"github.com/ekarpov/tessera/internal/identity"
	"github.com/ekarpov/tessera/internal/release"
	"github.com/ekarpov/tessera/internal/standalone"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/postgres"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	content := flag.Bool("dev-content", false, "enable synthetic filesystem attachments")
	reset := flag.Bool("dev-reset", false, "retire the development dataset, seed a replacement, and exit")
	faults := flag.Bool("dev-faults", false, "allow clients to request synthetic transport faults; needs a devfaults build")
	dev := flag.Bool("dev", false, "run a synthetic-data development deployment")
	address := flag.String("listen", "127.0.0.1:8080", "loopback listening address")
	directory := flag.String("data-dir", ".tessera-dev", "private development dataset and credential directory")
	configPath := flag.String("config", "", "self-hosted configuration file")
	person := flag.String("add-person", "", "provision a person locally, then exit")
	passwordFile := flag.String("password-file", "", "private initial password file")
	dataset := flag.String("dataset", "", "which dataset a provisioned principal is added to; required when the storage holds more than one")
	grantList := flag.String("grants", "read,write", "comma-separated grants for a provisioned person: read, write, schema, erase")
	mayCreate := flag.Bool("may-create-datasets", false, "also grant this person the server right to create datasets")
	mayAdminister := flag.Bool("may-administer-server", false, "also grant this person the right to administer what belongs to the server rather than to a dataset")
	service := flag.String("add-service", "", "provision an entitled service locally, then exit")
	noDataset := flag.Bool("no-dataset", false, "with -add-service: hold no dataset yet, as for a template issuer's reader that gains one per dataset created")
	newDataset := flag.Bool("new-dataset", false, "with -add-service: give the service a new dataset of its own that nobody else holds")
	datasetOutput := flag.String("dataset-output", "", "with -add-service: new private file to write the service's dataset identity to")
	deviceCode := flag.String("device-code", "", "write a single-use code for a person's first device to -secret-output: an app redeems it for an installation carrying the account's server rights and, with -dataset, its grants there; then exit")
	revokeInstallations := flag.Bool("revoke-installations", false, "with -device-code: first revoke every installation the account has, so a lost device stops working")
	rotateService := flag.String("rotate-service-secret", "", "replace a service's secret, writing the new one to -secret-output, then exit")
	secretOutput := flag.String("secret-output", "", "new private service credential output file")
	backup := flag.String("backup", "", "write an encrypted backup, then exit")
	backupKey := flag.String("backup-key-file", "", "private 32-byte backup key file")
	recoverBackupProducer := flag.Bool("recover-backup-producer", false, "offline recovery of backup producer identity and effective settings, then exit")
	expectedBackupProducer := flag.String("expected-backup-producer", "", "independently retained producer identity required for offline recovery")
	backupSettingsBootstrap := flag.String("backup-settings-bootstrap", "", "bootstrap missing native backup settings from a sanitized same-deployment export")
	automation := flag.String("add-token", "", "provision a long-lived automation token for a principal, then exit")
	suspend := flag.String("suspend-account", "", "stop every credential an account holds from authenticating, keeping them, then exit")
	reinstate := flag.String("reinstate-account", "", "let a suspended account's credentials authenticate again, then exit")
	issuer := flag.String("add-issuer", "", "configure an invitation issuer from -issuer-file, write its secret to -secret-output, then exit")
	issuerFile := flag.String("issuer-file", "", "JSON policy for -add-issuer: datasets or template, grants, cap")
	changeIssuer := flag.String("change-issuer", "", "change an issuer's -issuer-cap or -issuer-disabled, then exit; its policy does not change")
	issuerCap := flag.Int("issuer-cap", 0, "with -change-issuer: the new cap, at least 1")
	issuerDisabled := flag.String("issuer-disabled", "", "with -change-issuer: true stops the issuer and its outstanding invitations, false restores them")
	rotateIssuer := flag.String("rotate-issuer-secret", "", "replace an issuer's secret, writing the new one to -secret-output, then exit")
	setLimits := flag.String("set-dataset-limits", "", "change one dataset's limits from -limits-file, then exit")
	limitsFile := flag.String("limits-file", "", "JSON object of limits to change for -set-dataset-limits; members left out are kept")
	resetPassword := flag.String("reset-password", "", "set a new password for a person from -password-file, then exit; their sign-ins keep working")
	remove := flag.String("delete-account", "", "delete an account and its credentials and retire its name, then exit; datasets others also administer are kept")
	eraseDatasets := flag.String("erase-datasets", "", "with -delete-account: comma-separated datasets the account is the last administrator of, confirming they are erased")
	restore := flag.String("restore", "", "restore an encrypted backup into a fresh database and data directory, then exit")
	fence := flag.String("retirement-fence", "", "independent current retirement ledger required for restore")
	version := flag.Bool("version", false, "print what this binary was built from, then exit")
	flag.Parse()
	if *recoverBackupProducer || *expectedBackupProducer != "" {
		if !*recoverBackupProducer || *expectedBackupProducer == "" || *backupSettingsBootstrap == "" || *configPath == "" {
			return errors.New("backup producer recovery requires -recover-backup-producer, -expected-backup-producer, -backup-settings-bootstrap and -config")
		}
		conflicting := flag.NArg() != 0
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "recover-backup-producer", "expected-backup-producer", "backup-settings-bootstrap", "config":
			default:
				conflicting = true
			}
		})
		if conflicting {
			return errors.New("backup producer recovery is an exclusive offline action")
		}
		config, err := standalone.ReadConfig(*configPath)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return standalone.RecoverBackupProducer(ctx, config, *expectedBackupProducer, *backupSettingsBootstrap)
	}
	grantsGiven := false
	flag.Visit(func(f *flag.Flag) { grantsGiven = grantsGiven || f.Name == "grants" })
	*grantList = personGrants(*grantList, grantsGiven, *mayCreate || *mayAdminister, *dataset)
	if *version {
		fmt.Println(release.Version())
		return nil
	}
	if !*dev {
		if *content || *reset || *faults {
			return errors.New("development options require -dev")
		}
		action, account := "", ""
		for name, id := range map[string]string{"suspend": *suspend, "reinstate": *reinstate, "delete": *remove, "reset-password": *resetPassword} {
			if id == "" {
				continue
			}
			if action != "" {
				return errors.New("choose one of -suspend-account, -reinstate-account and -delete-account")
			}
			action, account = name, id
		}
		if action != "" {
			return runAccountAction(*configPath, action, account, *passwordFile, *eraseDatasets)
		}
		if *issuer != "" {
			return runAddIssuer(*configPath, *issuer, *issuerFile, *secretOutput)
		}
		if *setLimits != "" {
			return runSetDatasetLimits(*configPath, *setLimits, *limitsFile)
		}
		if *changeIssuer != "" {
			return runChangeIssuer(*configPath, *changeIssuer, *issuerCap, *issuerDisabled)
		}
		if *rotateIssuer != "" {
			return runRotateIssuer(*configPath, *rotateIssuer, *secretOutput)
		}
		if *rotateService != "" {
			return runRotateService(*configPath, *rotateService, *secretOutput)
		}
		if *deviceCode != "" {
			return runDeviceCode(*configPath, *deviceCode, *dataset, *secretOutput, *revokeInstallations)
		}
		if *revokeInstallations {
			return errors.New("-revoke-installations goes with -device-code")
		}
		if *service != "" && *newDataset && (*noDataset || *dataset != "") {
			return errors.New("-new-dataset makes the service's dataset; do not also give -dataset or -no-dataset")
		}
		return runStandalone(*configPath, *person, *passwordFile, *service, *secretOutput, *backup, *backupKey, *automation, *restore, *fence, *dataset, *grantList, *mayCreate, *mayAdminister, *noDataset || *newDataset, *newDataset, *datasetOutput, *backupSettingsBootstrap)
	}
	host, _, err := net.SplitHostPort(*address)
	if err != nil {
		return errors.New("invalid listen address")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("development listener must use a loopback IP address")
	}
	// Asking for faults from a binary that cannot inject them would otherwise
	// look like a server that simply never faults, which is the one thing a
	// failure-injection run must not silently be.
	if *faults && !devfaults.Enabled() {
		return errors.New("-dev-faults requires a binary built with -tags devfaults")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var listener net.Listener
	origin := ""
	if !*reset {
		listener, err = net.Listen("tcp", *address)
		if err != nil {
			return errors.New("cannot open development listener")
		}
		defer listener.Close()
		origin = "http://" + listener.Addr().String()
	}
	runtime, err := devserver.OpenWithOptions(ctx, *directory, devserver.Options{Content: *content, ContentOrigin: origin, Reset: *reset, Faults: *faults})
	if err != nil {
		return err
	}
	defer runtime.Close()
	if *reset {
		fmt.Println("Development dataset replaced and synthetic fixtures seeded.")
		return nil
	}
	server := &http.Server{Handler: runtime.Handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Printf("Tessera development server: http://%s\nSynthetic data only. Credential file: %s\n", listener.Addr(), runtime.TokenPath)
	if *faults {
		fmt.Printf("Synthetic transport faults enabled. Request one with %s: %s\n", devfaults.Header, strings.Join(devfaults.Available, ", "))
	}
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("development HTTP server stopped")
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		<-done
		if err != nil {
			return errors.New("development server shutdown exceeded its deadline")
		}
		return nil
	}
}

func runStandalone(configPath, person, passwordFile, service, secretOutput, backup, backupKey, automation, restore, fence, dataset, grantList string, mayCreate, mayAdminister, noDataset, newDataset bool, datasetOutput string, backupSettingsBootstrap ...string) error {
	config, err := standalone.ReadConfig(configPath)
	if err != nil {
		return err
	}
	if len(backupSettingsBootstrap) > 0 {
		config.BackupSettingsBootstrap = backupSettingsBootstrap[0]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if restore != "" {
		key, err := standalone.PrivateFile(backupKey)
		if err != nil {
			return err
		}
		contentDir := ""
		if config.Content {
			contentDir = filepath.Join(config.DataDir, "content")
		}
		if config.Engine == "postgres" {
			dsn, e := standalone.PrivateFile(config.PostgresDSNFile)
			if e != nil {
				return e
			}
			return postgres.RestoreBackup(ctx, restore, strings.TrimSpace(string(dsn)), filepath.Join(config.DataDir, "postgres-fence"), contentDir, fence, key, config.DataDir)
		}
		return sqlite.RestoreBackup(ctx, restore, filepath.Join(config.DataDir, "dataset.db"), contentDir, fence, key, config.DataDir)
	}
	runtime, err := standalone.Open(ctx, config)
	if err != nil {
		return err
	}
	defer runtime.Close()
	// An account given nothing but the right to create needs no dataset, which
	// is how the first one on a server is provisioned.
	if automation != "" || (service != "" && !noDataset) || (person != "" && !((mayCreate || mayAdminister) && grantList == "")) {
		// Naming the dataset is only ceremony when there is one possible
		// answer, so a storage holding one keeps the old command line working.
		if dataset, err = runtime.ResolveDataset(ctx, dataset); err != nil {
			return err
		}
	}
	if automation != "" {
		return runtime.AddAutomation(ctx, automation, secretOutput, dataset)
	}
	if person != "" {
		// An account may be given a server right and no dataset grant, which
		// is the only account a server can have before it has a dataset, and
		// is also how the server's administrator is provisioned.
		grants := store.Grants{}
		if grantList != "" {
			parsed, e := standalone.ParseGrants(grantList)
			if e != nil {
				return e
			}
			grants = parsed
		}
		return runtime.AddPerson(person, passwordFile, dataset, grants, mayCreate, mayAdminister)
	}
	if service != "" {
		return runtime.AddService(ctx, service, secretOutput, dataset, newDataset, datasetOutput)
	}
	if backup != "" {
		key, err := standalone.PrivateFile(backupKey)
		if err != nil {
			return err
		}
		if err = runtime.Engine.Backup(ctx, backup, key, standalone.RegistryFiles(config.DataDir)...); err != nil {
			return err
		}
		_ = runtime.Authority.RecordBackup(time.Now())
		return nil
	}
	if _, err = standalone.PrivateFile(config.TLSKey); err != nil {
		return err
	}
	runtime.StartBackups(ctx)
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return errors.New("cannot open server listener")
	}
	server := &http.Server{Handler: runtime.Handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(listener, config.TLSCertificate, config.TLSKey) }()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("TLS server stopped")
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		<-done
		if err != nil {
			return errors.New("server shutdown exceeded deadline")
		}
		return nil
	}
}

// runAccountAction suspends, reinstates or deletes an account from the command
// line. The operator stands here with full authority, so the administration
// page's standing rules do not apply; the authority's own refusals still do,
// including deleting the last administrator of a dataset.
func runAccountAction(configPath, action, account, passwordFile, eraseDatasets string) error {
	config, err := standalone.ReadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime, err := standalone.Open(ctx, config)
	if err != nil {
		return err
	}
	defer runtime.Close()
	switch action {
	case "reset-password":
		// Read from a private file, as -add-person does, so a password never
		// appears on a command line or in a shell's history.
		secret, e := standalone.PrivateFile(passwordFile)
		if e != nil {
			return e
		}
		err = runtime.Authority.ResetPassword(account, strings.TrimSpace(string(secret)), "operator")
	case "suspend":
		err = runtime.Authority.SuspendAccount(account)
	case "reinstate":
		err = runtime.Authority.ReinstateAccount(account)
	default:
		var erase []string
		for _, dataset := range strings.Split(eraseDatasets, ",") {
			if dataset = strings.TrimSpace(dataset); dataset != "" {
				erase = append(erase, dataset)
			}
		}
		var kept []identity.Kept
		kept, err = runtime.Authority.DeleteAccount(ctx, account, erase)
		// The operator is told what stayed, so a deletion requested as "remove
		// this person's data" is not mistaken for one that did.
		for _, dataset := range kept {
			fmt.Printf("kept %s (%s)\n", dataset.Dataset, dataset.Reason)
		}
	}
	if err == nil {
		return nil
	}
	// ErrorCode answers internal_error for nil, so success is decided above
	// rather than by a case for the empty string, which would never be taken.
	switch p.ErrorCode(err) {
	case "not_found":
		return errors.New("no such account")
	case "conflict":
		if action == "delete" {
			if sole := runtime.Authority.DeletionErases(account); len(sole) > 0 {
				return fmt.Errorf("deleting this account erases the datasets it is the last administrator of; to confirm, pass exactly -erase-datasets %s", strings.Join(sole, ","))
			}
			return errors.New("the account is the last server administrator, or the datasets named do not match what the deletion erases")
		}
		return errors.New("the account is already in that state")
	}
	return err
}

// issuerPolicy is the file an operator writes to configure an issuer. Grants
// are named the way -grants names them, and types are schema declarations as
// the protocol writes them.
type issuerPolicy struct {
	Datasets []string `json:"datasets"`
	Template *struct {
		Administrator string   `json:"administrator"`
		DisplayName   string   `json:"display_name"`
		Types         []p.Meta `json:"types"`
		Readers       []struct {
			Account string   `json:"account"`
			Types   []string `json:"types"`
		} `json:"readers"`
		EraseWithAccount bool `json:"erase_with_account"`
	} `json:"template"`
	Grants      string `json:"grants"`
	Cap         int    `json:"cap"`
	AssignNames bool   `json:"assign_names"`
	// ModerateSharing lets the server's administrator end or narrow what
	// accounts this issuer made share from the datasets it made them.
	ModerateSharing bool `json:"moderate_sharing"`
	// DeviceRedemption lets an app redeem the issuer's invitations on a
	// device, with no password and no browser.
	DeviceRedemption bool `json:"device_redemption"`
}

// runAddIssuer configures an invitation issuer. Its secret is written to a new
// private file rather than printed, for the reason a service secret is.
func runAddIssuer(configPath, id, policyPath, secretOutput string) error {
	if policyPath == "" || secretOutput == "" {
		return errors.New("-add-issuer needs -issuer-file and -secret-output")
	}
	raw, err := os.ReadFile(policyPath)
	if err != nil {
		return errors.New("cannot read the issuer file")
	}
	var policy issuerPolicy
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&policy); err != nil {
		return errors.New("the issuer file is not a valid policy")
	}
	grants, err := standalone.ParseGrants(policy.Grants)
	if err != nil {
		return err
	}
	configured := identity.Issuer{ID: id, Datasets: policy.Datasets, Grants: grants, Cap: policy.Cap, AssignNames: policy.AssignNames, ModerateSharing: policy.ModerateSharing, DeviceRedemption: policy.DeviceRedemption}
	if policy.Template != nil {
		configured.Template = &identity.Template{Administrator: policy.Template.Administrator, DisplayName: policy.Template.DisplayName, Types: policy.Template.Types, EraseWithAccount: policy.Template.EraseWithAccount}
		for _, reader := range policy.Template.Readers {
			configured.Template.Readers = append(configured.Template.Readers, identity.TemplateReader{Account: reader.Account, Types: reader.Types})
		}
	}
	config, err := standalone.ReadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime, err := standalone.Open(ctx, config)
	if err != nil {
		return err
	}
	defer runtime.Close()
	file, err := os.OpenFile(secretOutput, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("the secret output must be a new file")
	}
	defer file.Close()
	secret, err := runtime.Authority.AddIssuer(ctx, configured)
	if err != nil {
		_ = os.Remove(secretOutput)
		return err
	}
	_, err = file.WriteString(secret + "\n")
	return err
}

// runSetDatasetLimits changes one dataset's limits. It is how an operator
// answers 507 dataset_full: the bound a dataset outgrew is stored on it, so
// raising the configured default does not reach a dataset that already exists.
func runSetDatasetLimits(configPath, dataset, limitsPath string) error {
	if limitsPath == "" {
		return errors.New("-set-dataset-limits needs -limits-file")
	}
	raw, err := os.ReadFile(limitsPath)
	if err != nil {
		return errors.New("cannot read the limits file")
	}
	config, err := standalone.ReadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime, err := standalone.Open(ctx, config)
	if err != nil {
		return err
	}
	defer runtime.Close()
	current, err := runtime.Engine.DatasetLimits(ctx, dataset)
	if err != nil {
		return errors.New("no such dataset")
	}
	changed, err := p.MergeLimits(current, raw)
	if err != nil {
		return errors.New("limits must be positive, name only known limits, and keep idempotency_retention_seconds at 86400 or more")
	}
	if err = runtime.Engine.SetDatasetLimits(ctx, dataset, changed); err != nil {
		if p.ErrorCode(err) == "conflict" {
			return errors.New("idempotency_retention_seconds may only grow: clients were promised the current window")
		}
		return err
	}
	return nil
}

// openRuntime opens the configured server for a one-off operation.
func openRuntime(configPath string) (context.Context, *standalone.Runtime, func(), error) {
	config, err := standalone.ReadConfig(configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	runtime, err := standalone.Open(ctx, config)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	return ctx, runtime, func() { runtime.Close(); stop() }, nil
}

func runChangeIssuer(configPath, id string, capacity int, disabled string) error {
	var change identity.IssuerChange
	if capacity != 0 {
		change.Cap = &capacity
	}
	switch disabled {
	case "":
	case "true", "false":
		value := disabled == "true"
		change.Disabled = &value
	default:
		return errors.New("-issuer-disabled is true or false")
	}
	if change.Cap == nil && change.Disabled == nil {
		return errors.New("name -issuer-cap, -issuer-disabled or both")
	}
	_, runtime, done, err := openRuntime(configPath)
	if err != nil {
		return err
	}
	defer done()
	changed, err := runtime.Authority.ChangeIssuer(id, change)
	if err == nil {
		fmt.Printf("issuer %s: cap %d, issued %d, disabled %t\n", changed.ID, changed.Cap, changed.Issued, changed.Disabled)
		return nil
	}
	switch p.ErrorCode(err) {
	case "not_found":
		return errors.New("no such issuer")
	case "invalid_field":
		return errors.New("a cap is at least 1; disable an issuer to stop it")
	}
	return err
}

func runRotateIssuer(configPath, id, secretOutput string) error {
	file, err := os.OpenFile(secretOutput, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("the secret output must be a new file")
	}
	defer file.Close()
	_, runtime, done, err := openRuntime(configPath)
	if err != nil {
		_ = os.Remove(secretOutput)
		return err
	}
	defer done()
	secret, err := runtime.Authority.RotateIssuerSecret(id)
	if err != nil {
		_ = os.Remove(secretOutput)
		if p.ErrorCode(err) == "not_found" {
			return errors.New("no such issuer")
		}
		return err
	}
	_, err = file.WriteString(secret + "\n")
	return err
}

// runDeviceCode writes a single-use code for a person's first device, having
// first revoked every installation the account has when asked to: a lost
// phone's access ends in the same step that makes its replacement's code, so
// there is no moment with neither. Like every host command it runs with the
// server stopped.
func runDeviceCode(configPath, account, dataset, secretOutput string, revoke bool) error {
	file, err := os.OpenFile(secretOutput, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("the secret output must be a new file")
	}
	defer file.Close()
	_, runtime, done, err := openRuntime(configPath)
	if err != nil {
		_ = os.Remove(secretOutput)
		return err
	}
	defer done()
	code, _, err := runtime.Authority.AddHostDeviceCode(context.Background(), account, dataset, revoke)
	if err != nil {
		_ = os.Remove(secretOutput)
		if p.ErrorCode(err) == "not_found" {
			return errors.New("no such person, or the dataset is not theirs")
		}
		if p.ErrorCode(err) == "invalid_field" {
			return errors.New("the account holds no server right; name one of its datasets with -dataset")
		}
		return err
	}
	_, err = file.WriteString(code + "\n")
	return err
}

func runRotateService(configPath, id, secretOutput string) error {
	file, err := os.OpenFile(secretOutput, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("the secret output must be a new file")
	}
	defer file.Close()
	_, runtime, done, err := openRuntime(configPath)
	if err != nil {
		_ = os.Remove(secretOutput)
		return err
	}
	defer done()
	secret, err := runtime.Authority.RotateServiceSecret(id)
	if err != nil {
		_ = os.Remove(secretOutput)
		if p.ErrorCode(err) == "not_found" {
			return errors.New("no such service")
		}
		return err
	}
	_, err = file.WriteString(secret + "\n")
	return err
}

// personGrants is what -add-person grants on a dataset. -grants defaults to
// read and write for somebody joining the storage's dataset; but an account
// given a server right — the server's owner, or one who may create datasets —
// and neither -grants nor -dataset is being provisioned to hold no data, and
// the default used to put them in the storage's first dataset anyway, able to
// read and write whatever was later kept there.
func personGrants(grantList string, given, serverRight bool, dataset string) string {
	if serverRight && !given && dataset == "" {
		return ""
	}
	return grantList
}
