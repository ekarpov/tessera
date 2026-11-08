package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// CheckRecovery exercises durable-recovery HTTP semantics. The fixture is the
// same disposable dataset used by CheckCore. Persistence/crash tests additionally
// belong to the engine's integration suite; an HTTP pass cannot prove durability.
func CheckRecovery(ctx context.Context, f CoreFixture) error {
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	call := func(method, path, token, key string, body []byte, status int) ([]byte, bool, error) {
		req, err := http.NewRequestWithContext(ctx, method, f.URL+path, bytes.NewReader(body))
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, false, fmt.Errorf("recovery HTTP request failed")
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		if err != nil {
			return nil, false, fmt.Errorf("recovery response read failed")
		}
		if res.StatusCode != status {
			return nil, false, fmt.Errorf("recovery %s expected HTTP %d, received %d", method, status, res.StatusCode)
		}
		return data, res.Header.Get("Idempotency-Replayed") == "true", nil
	}
	var listing struct {
		Datasets []struct {
			ID string `json:"id"`
		} `json:"datasets"`
	}
	data, _, err := call("GET", "/v1/datasets", f.Token, "", nil, 200)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &listing) != nil || len(listing.Datasets) != 1 {
		return fmt.Errorf("recovery fixture needs one dataset")
	}
	base := "/v1/datasets/" + url.PathEscape(listing.Datasets[0].ID)
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	hexID := hex.EncodeToString(nonce[:])
	key := hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:]
	nextKey := func(n byte) string {
		nonce[15] ^= n
		h := hex.EncodeToString(nonce[:])
		return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	}
	typ := "test.recovery.t" + hexID
	create := []byte(`{"type":{"id":"` + typ + `","fields":[{"id":"note","meta_id":"` + typ + `","type":"text"}]}}`)
	original, _, err := call("POST", base+"/meta", f.Token, key, create, 201)
	if err != nil {
		return err
	}
	replay, replayed, err := call("POST", base+"/meta", f.Token, key, create, 201)
	if err != nil {
		return err
	}
	if !replayed || !bytes.Equal(original, replay) {
		return fmt.Errorf("schema receipt did not replay original response")
	}
	// A key on a read has no outcome to recover, and is refused as a header
	// the request has no place for rather than as a bad field.
	// A key of its own, not from nextKey, whose flips accumulate: taking one
	// here would move every key the checks below depend on.
	var readNonce [16]byte
	if _, err = rand.Read(readNonce[:]); err != nil {
		return err
	}
	rh := hex.EncodeToString(readNonce[:])
	refused, _, err := call("POST", base+"/objects/"+typ+":query", f.Token, rh[:8]+"-"+rh[8:12]+"-"+rh[12:16]+"-"+rh[16:20]+"-"+rh[20:], []byte(`{"query":{}}`), 400)
	if err != nil {
		return fmt.Errorf("a read carrying Idempotency-Key was not refused: %w", err)
	}
	var keyedRead struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(refused, &keyedRead) != nil || keyedRead.Error.Code != "invalid_request" || keyedRead.Error.Details["reason"] != "unexpected_idempotency_key" {
		return fmt.Errorf("a read carrying Idempotency-Key was refused with %s, not invalid_request and unexpected_idempotency_key", refused)
	}
	insertKey := nextKey(1)
	body := []byte(`{"fields":{"note":{"type":"text","value":"synthetic"}}}`)
	original, _, err = call("POST", base+"/objects/"+typ, f.Token, insertKey, body, 201)
	if err != nil {
		return err
	}
	var inserted struct {
		Objects   []p.Identity `json:"objects"`
		Watermark string       `json:"watermark"`
	}
	if json.Unmarshal(original, &inserted) != nil || len(inserted.Objects) != 1 {
		return fmt.Errorf("invalid recovery insert response")
	}
	objectPath := base + "/objects/" + typ + "/" + url.PathEscape(inserted.Objects[0].ID)
	if _, _, err = call("PATCH", objectPath, f.Token, "", []byte(`{"fields":{"note":{"type":"text","value":"changed"}}}`), 200); err != nil {
		return err
	}
	replay, replayed, err = call("POST", base+"/objects/"+typ, f.Token, insertKey, body, 201)
	if err != nil {
		return err
	}
	if !replayed || !bytes.Equal(original, replay) {
		return fmt.Errorf("receipt replay changed its historical result or watermark")
	}
	statusPath := base + "/operations/" + insertKey + ":status"
	data, _, err = call("POST", statusPath, f.Token, "", nil, 200)
	if err != nil {
		return err
	}
	var status struct {
		Watermark      string          `json:"watermark"`
		State          string          `json:"state"`
		Result         json.RawMessage `json:"result"`
		ResultWithheld bool            `json:"result_withheld"`
		Objects        []p.Reference   `json:"objects"`
	}
	if json.Unmarshal(data, &status) != nil || status.State != "committed" || status.Watermark != inserted.Watermark || !bytes.Equal(status.Result, original) {
		return fmt.Errorf("status does not carry retained success")
	}
	conflict, _, err := call("POST", base+"/objects/"+typ, f.Token, insertKey, append(append([]byte{}, body...), ' '), 409)
	if err != nil {
		return err
	}
	var problem struct {
		Error p.Error `json:"error"`
	}
	if json.Unmarshal(conflict, &problem) != nil || problem.Error.Details["reason"] != "idempotency_key_reused" {
		return fmt.Errorf("changed body did not conflict with retained key")
	}
	if _, _, err = call("POST", statusPath, f.RefusedToken, "", nil, 403); err != nil {
		return err
	}
	if _, _, err = call("POST", base+"/objects/"+typ, f.RefusedToken, insertKey, body, 403); err != nil {
		return err
	}
	// A malformed request must not claim a key.
	malformedKey := nextKey(2)
	if _, _, err = call("POST", base+":transact", f.Token, malformedKey, []byte(`{"operations":[],"operations":[]}`), 400); err != nil {
		return err
	}
	data, _, err = call("POST", base+"/operations/"+malformedKey+":status", f.Token, "", nil, 200)
	if err != nil {
		return err
	}
	var unknown struct {
		State string `json:"state"`
	}
	if json.Unmarshal(data, &unknown) != nil || unknown.State != "unknown" {
		return fmt.Errorf("malformed request claimed its key")
	}
	failedKey := nextKey(4)
	assertion := []byte(`{"operations":[{"op":"expect","target":{"type":"` + typ + `","id":"` + inserted.Objects[0].ID + `"},"fields":{"note":{"type":"text","value":"not-current"}}}]}`)
	failed, _, err := call("POST", base+":transact", f.Token, failedKey, assertion, 409)
	if err != nil {
		return err
	}
	if _, _, err = call("PATCH", objectPath, f.Token, "", []byte(`{"fields":{"note":{"type":"text","value":"not-current"}}}`), 200); err != nil {
		return err
	}
	replay, replayed, err = call("POST", base+":transact", f.Token, failedKey, assertion, 409)
	if err != nil {
		return err
	}
	if !replayed || !bytes.Equal(failed, replay) {
		return fmt.Errorf("failed check was reexecuted")
	}
	deleteKey := nextKey(8)
	deleted, _, err := call("DELETE", objectPath, f.Token, deleteKey, nil, 200)
	if err != nil {
		return err
	}
	replay, replayed, err = call("DELETE", objectPath, f.Token, deleteKey, nil, 200)
	if err != nil {
		return err
	}
	if !replayed || !bytes.Equal(deleted, replay) {
		return fmt.Errorf("delete was reexecuted")
	}
	data, _, err = call("POST", statusPath, f.Token, "", nil, 200)
	if err != nil {
		return err
	}
	status.Result = nil
	if json.Unmarshal(data, &status) != nil || status.State != "committed" || !status.ResultWithheld || status.Result != nil || len(status.Objects) != 1 || status.Objects[0].MetaID != typ || status.Objects[0].ObjectID != inserted.Objects[0].ID {
		return fmt.Errorf("deleted object erased receipt identity or leaked stored body")
	}
	return nil
}
