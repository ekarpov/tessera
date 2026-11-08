package standalone

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/ekarpov/tessera/internal/acquisition"
)

func decodeBackupSettingsBootstrap(raw []byte) (*acquisition.SettingsExport, error) {
	return acquisition.DecodeSettingsExport(raw)
}

// RecoverBackupProducer is an offline composition: it does not open record
// storage, provision accounts, start listeners or load collector authority.
func RecoverBackupProducer(ctx context.Context, config Config, expected, exportPath string) error {
	if config.Backup == nil || expected == "" || exportPath == "" {
		return errors.New("recovery requires configured backups, an expected producer and a settings export")
	}
	file, err := os.Open(exportPath)
	if err != nil {
		return errors.New("cannot read backup settings export")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil {
		return errors.New("cannot read backup settings export")
	}
	exported, err := acquisition.DecodeSettingsExport(raw)
	if err != nil {
		return errors.New("invalid backup settings export")
	}
	return acquisition.RecoverProducer(ctx, config.Backup.Directory, expected, exported)
}
