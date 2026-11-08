package acquisition

import (
	"encoding/json"
	"errors"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// DecodeSettingsExport strictly decodes scalar wire members before parsing the
// timestamp: DecodeRequest checks
// struct members recursively and time.Time is encoded as a JSON string.
func DecodeSettingsExport(raw []byte) (*SettingsExport, error) {
	var wire struct {
		Version    *int    `json:"version"`
		Deployment *string `json:"deployment"`
		EveryHours *int    `json:"every_hours"`
		Keep       *int    `json:"keep"`
		Revision   *string `json:"revision"`
		ExportedAt *string `json:"exported_at"`
	}
	if err := p.DecodeRequest(raw, &wire, 16384); err != nil {
		return nil, err
	}
	if wire.Version == nil || wire.Deployment == nil || wire.EveryHours == nil || wire.Keep == nil || wire.Revision == nil || wire.ExportedAt == nil {
		return nil, errors.New("missing backup settings export member")
	}
	var at time.Time
	timestamp, _ := json.Marshal(*wire.ExportedAt)
	if err := at.UnmarshalJSON(timestamp); err != nil {
		return nil, err
	}
	return &SettingsExport{Version: *wire.Version, Deployment: *wire.Deployment, EveryHours: *wire.EveryHours, Keep: *wire.Keep, Revision: *wire.Revision, ExportedAt: at}, nil
}
