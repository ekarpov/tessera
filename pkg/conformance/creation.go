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
	"strings"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// CheckCreation exercises dataset creation and the server-scoped receipt that
// makes a lost creation recoverable. It reads what discovery advertises and runs
// the matching half: a server offering no dataset_creation must refuse the route,
// and the refusal is as much the contract as the behaviour is.
//
// Creation leaves datasets behind, so the suite erases what it makes. A server
// whose creator does not hold erase fails here rather than leaking a dataset per
// run, because the contract says the caller that asks becomes administrator.
func CheckCreation(ctx context.Context, f CoreFixture) error {
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
			return nil, false, fmt.Errorf("creation HTTP request failed")
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if err != nil {
			return nil, false, fmt.Errorf("creation response read failed")
		}
		if res.StatusCode != status {
			return nil, false, fmt.Errorf("creation %s expected HTTP %d, received %d", method, status, res.StatusCode)
		}
		return data, res.Header.Get("Idempotency-Replayed") == "true", nil
	}
	uuid := func() (string, error) {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", err
		}
		h := hex.EncodeToString(nonce[:])
		return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
	}

	// Discovery decides which half of this suite runs, and is checked either way.
	var found struct {
		Capabilities []string       `json:"server_capabilities"`
		Limits       map[string]int `json:"server_limits"`
	}
	data, _, err := call("GET", "/.well-known/tessera", f.Token, "", nil, 200)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &found) != nil {
		return fmt.Errorf("discovery is not readable")
	}
	offered := false
	for _, name := range found.Capabilities {
		if name == "dataset_creation" {
			offered = true
		}
	}

	// The window is not conditional on the capability: a server that stops
	// offering creation must still say how long it answers about what it made.
	if window, ok := found.Limits["idempotency_retention_seconds"]; ok {
		if window <= 0 {
			return fmt.Errorf("server_limits advertises a non-positive retention window")
		}
	} else if offered {
		return fmt.Errorf("a server offering creation must advertise its server-scoped retention window")
	}

	var access struct {
		ServerAccess map[string]bool `json:"server_access"`
	}
	data, _, err = call("GET", "/v1/datasets", f.Token, "", nil, 200)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &access) != nil {
		return fmt.Errorf("dataset listing is not readable")
	}
	// Named whatever the answer is, and named on a server offering nothing at
	// all. A client tells "nothing is provisioned for me here" from "make one"
	// by this member, and a member that disappears when the answer is no cannot
	// carry that distinction — it would mean both "not offered" and "not yours"
	// on the axis that exists to answer the second.
	if _, named := access.ServerAccess["create_dataset"]; !named {
		return fmt.Errorf("server_access omits create_dataset instead of reporting it false")
	}

	key, err := uuid()
	if err != nil {
		return err
	}
	body := []byte(`{"display_name":"Conformance"}`)

	if !offered {
		// Withheld is a promise too. The route must refuse rather than answer,
		// and must not answer a different refusal that a client would retry.
		if _, _, err = call("POST", "/v1/datasets", f.Token, key, body, 403); err != nil {
			return fmt.Errorf("a server not offering creation must refuse it with 403 not_supported")
		}
		if access.ServerAccess["create_dataset"] {
			return fmt.Errorf("create_dataset is reported without the capability that would make it usable")
		}
		return nil
	}
	if !access.ServerAccess["create_dataset"] {
		// The fixture's own token has to be able to create, or nothing below
		// distinguishes a correct refusal from an unusable fixture.
		return fmt.Errorf("the fixture token holds no create_dataset right, so creation cannot be exercised")
	}

	// A key is required, and the refusal must precede the work.
	before, err := datasetIDs(ctx, client, f)
	if err != nil {
		return err
	}
	if _, _, err = call("POST", "/v1/datasets", f.Token, "", body, 400); err != nil {
		return fmt.Errorf("creation without an idempotency key was not refused with 400 invalid_request")
	}
	after, err := datasetIDs(ctx, client, f)
	if err != nil {
		return err
	}
	if len(after) != len(before) {
		return fmt.Errorf("a creation refused for a missing key still made a dataset")
	}

	data, replayed, err := call("POST", "/v1/datasets", f.Token, key, body, 201)
	if err != nil {
		return err
	}
	if replayed {
		return fmt.Errorf("a first creation reported itself as a replay")
	}
	var made map[string]json.RawMessage
	if json.Unmarshal(data, &made) != nil {
		return fmt.Errorf("a creation response is not readable")
	}
	var created string
	if json.Unmarshal(made["dataset_id"], &created) != nil || p.ValidateIdentifier(p.DatasetIdentifier, created) != nil {
		return fmt.Errorf("a creation did not report a dataset identity")
	}
	// Identity and nothing else: anything further is discovery state, answered
	// by the listing, and a second place for it to live is a second place to drift.
	if len(made) != 1 {
		return fmt.Errorf("a creation response carried more than the identity")
	}

	// The creator administers it, and sees it at once — a client reads back
	// immediately after creating, so a lagging listing breaks that sequence.
	listed, err := datasetIDs(ctx, client, f)
	if err != nil {
		return err
	}
	if !listed[created] {
		return fmt.Errorf("a created dataset was absent from its creator's listing")
	}
	for id := range before {
		if !listed[id] {
			return fmt.Errorf("creating a dataset removed another from the creator's listing")
		}
	}
	if err = checkCreationWidensTheReach(ctx, call, client, f, uuid, listed); err != nil {
		return err
	}
	if err = checkCreationWithTypes(ctx, call, client, f, uuid); err != nil {
		return err
	}

	if err = checkCreationReplay(call, uuid, f.Token, key, body, created); err != nil {
		return err
	}
	if err = checkCreationOutlivesWithdrawal(ctx, call, f, key, body, created); err != nil {
		return err
	}
	return checkCreationOutlivesErasure(ctx, call, f, key, body, created)
}

// datasetIDs is the caller's own listing as a set, which several checks compare
// before and after rather than trusting a count.
func datasetIDs(ctx context.Context, client *http.Client, f CoreFixture) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", f.URL+"/v1/datasets", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.Token)
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creation listing request failed")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil || res.StatusCode != 200 {
		return nil, fmt.Errorf("creation listing was not readable")
	}
	var listing struct {
		Datasets []struct {
			ID string `json:"id"`
		} `json:"datasets"`
	}
	if json.Unmarshal(data, &listing) != nil {
		return nil, fmt.Errorf("creation listing is not readable")
	}
	held := map[string]bool{}
	for _, d := range listing.Datasets {
		held[d.ID] = true
	}
	return held, nil
}

type creationCall func(method, path, token, key string, body []byte, status int) ([]byte, bool, error)

// checkCreationReplay covers the three retained outcomes a repeat can meet and
// the digest rule that separates a replay from a different request.
func checkCreationReplay(call creationCall, uuid func() (string, error), token, key string, body []byte, created string) error {
	again, replayed, err := call("POST", "/v1/datasets", token, key, body, 201)
	if err != nil {
		return err
	}
	if !replayed {
		return fmt.Errorf("a replayed creation did not carry Idempotency-Replayed")
	}
	var repeated struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(again, &repeated) != nil || repeated.DatasetID != created {
		return fmt.Errorf("a replayed creation named a different dataset")
	}

	// A different payload under the same key is a different request, whatever
	// the retained outcome was.
	if _, _, err = call("POST", "/v1/datasets", token, key, []byte(`{"display_name":"Different"}`), 409); err != nil {
		return fmt.Errorf("a reused key with a different payload was not refused with 409 conflict")
	}

	// The server-scoped receipt: a creation belongs to no dataset, so it is not
	// addressed under one.
	status, _, err := call("POST", "/v1/operations/"+url.PathEscape(key)+":status", token, "", nil, 200)
	if err != nil {
		return err
	}
	var receipt struct {
		State  string `json:"state"`
		Result struct {
			DatasetID string `json:"dataset_id"`
		} `json:"result"`
	}
	if json.Unmarshal(status, &receipt) != nil || receipt.State != "committed" {
		return fmt.Errorf("a committed creation did not report itself committed")
	}
	if receipt.Result.DatasetID != created {
		return fmt.Errorf("a creation receipt did not carry the identity it made")
	}

	unclaimed, err := uuid()
	if err != nil {
		return err
	}
	status, _, err = call("POST", "/v1/operations/"+url.PathEscape(unclaimed)+":status", token, "", nil, 200)
	if err != nil {
		return err
	}
	if json.Unmarshal(status, &receipt) != nil || receipt.State != "unknown" {
		return fmt.Errorf("an unclaimed key was not answered unknown")
	}
	return nil
}

// checkCreationOutlivesErasure is the boundary two rounds of review had to
// correct in the specification, so the suite asserts it rather than trusting the
// prose: a receipt names a dataset but is not a question about that dataset, and
// its owner outlives its access. Erasing what a creation made must not take the
// record of having made it.
// checkCreationWidensTheReach settles what a second creation does to a
// listing. An authorization is approved for at most one dataset and reaches
// what it creates, so creating twice reaches both: it does not move, and it
// does not reach only the newest.
//
// This exists because the specification contradicted itself. The
// authorization rules said a listing returns exactly the one dataset the
// authorization is over; creation required the created one to appear in that
// same listing. Both were requirements. The check above asserts only that the
// created one appeared, which leaves a server implementing either reading
// passing — and one of the readings is where three security defects came
// from. A case that cannot tell two readings apart is not evidence for the
// one that happens to be implemented.
//
// It creates its own dataset rather than leaning on the fixture already
// holding one, because the fixture's caller may hold none. That is the
// bootstrap case, and it is exactly where a listing of one and a listing of
// "only the newest" look identical — which is how the first version of this
// check came to assert nothing at all.
func checkCreationWidensTheReach(ctx context.Context, call creationCall, client *http.Client, f CoreFixture, uuid func() (string, error), held map[string]bool) error {
	key, err := uuid()
	if err != nil {
		return err
	}
	data, _, err := call("POST", "/v1/datasets", f.Token, key, []byte(`{"display_name":"Conformance reach"}`), 201)
	if err != nil {
		return err
	}
	var made struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(data, &made) != nil || made.DatasetID == "" {
		return fmt.Errorf("a second creation did not report a dataset identity")
	}
	listed, err := datasetIDs(ctx, client, f)
	if err != nil {
		return err
	}
	if !listed[made.DatasetID] {
		return fmt.Errorf("a second created dataset was absent from its creator's listing")
	}
	for id := range held {
		if !listed[id] {
			return fmt.Errorf("creating a second dataset dropped an earlier one from the creator's listing")
		}
	}
	// Erased here rather than left behind, for the reason the rest of this
	// suite erases what it makes: a check that accumulates datasets is one
	// nobody can run twice against the same server.
	base := "/v1/datasets/" + url.PathEscape(made.DatasetID)
	if _, _, err = call("POST", base+":erase", f.Token, "", []byte(`{"confirm":"`+made.DatasetID+`"}`), 200); err != nil {
		return fmt.Errorf("the creating caller could not erase the second dataset it made")
	}
	return nil
}

func checkCreationOutlivesErasure(ctx context.Context, call creationCall, f CoreFixture, key string, body []byte, created string) error {
	// A live dataset this caller may not use is the third answer, and the one
	// whose behaviour changed most: it is concealed rather than refused, so a
	// direct address cannot undo what the listing conceals. The created dataset
	// before it was erased is the case — this token administers it and the
	// refused one holds nothing on it.
	if f.RefusedToken != "" {
		second, _, e := call("GET", "/v1/datasets/"+url.PathEscape(created)+"/meta", f.RefusedToken, "", nil, 404)
		if e != nil {
			return fmt.Errorf("a dataset another caller holds nothing on was not concealed: %w", e)
		}
		if strings.Contains(string(second), "not_found") {
			return fmt.Errorf("a live dataset the caller may not use answered not_found, claiming a retirement that did not happen")
		}
	}

	base := "/v1/datasets/" + url.PathEscape(created)
	if _, _, err := call("POST", base+":erase", f.Token, "", []byte(`{"confirm":"`+created+`"}`), 200); err != nil {
		return fmt.Errorf("the creating caller could not erase what it made, so it is not its administrator")
	}

	// The identity is still answerable, by both routes, with no grant left.
	status, _, err := call("POST", "/v1/operations/"+url.PathEscape(key)+":status", f.Token, "", nil, 200)
	if err != nil {
		return fmt.Errorf("a creation receipt stopped answering when its dataset was erased")
	}
	var receipt struct {
		State  string `json:"state"`
		Result struct {
			DatasetID string `json:"dataset_id"`
		} `json:"result"`
	}
	if json.Unmarshal(status, &receipt) != nil || receipt.State != "committed" || receipt.Result.DatasetID != created {
		return fmt.Errorf("a creation receipt lost its identity when its dataset was erased")
	}
	again, replayed, err := call("POST", "/v1/datasets", f.Token, key, body, 201)
	if err != nil {
		return fmt.Errorf("a creation could not be replayed after its dataset was erased")
	}
	var repeated struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(again, &repeated) != nil || repeated.DatasetID != created || !replayed {
		return fmt.Errorf("a replay after erasure did not return the retained identity")
	}

	// And the erased identity itself now answers not_found, which is the one
	// answer that means proven retirement. An identity that names nothing must
	// not borrow it.
	if _, _, err = call("GET", base+"/meta", f.Token, "", nil, 404); err != nil {
		return fmt.Errorf("an erased dataset did not answer 404")
	}
	data, _, err := call("GET", base+"/meta", f.Token, "", nil, 404)
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "not_found") {
		return fmt.Errorf("an erased dataset did not answer not_found, which is how a client learns it is gone")
	}
	var absent [16]byte
	if _, err = rand.Read(absent[:]); err != nil {
		return err
	}
	data, _, err = call("GET", "/v1/datasets/s-"+hex.EncodeToString(absent[:])+"/meta", f.Token, "", nil, 404)
	if err != nil {
		return err
	}
	if strings.Contains(string(data), "not_found") {
		return fmt.Errorf("an identity naming nothing answered not_found, which claims a retirement the server cannot have proven")
	}
	return nil
}

// checkCreationOutlivesWithdrawal is the boundary this implementation had
// backwards and the specification had to state twice: a repeat is a question
// about what already happened, so it is answered from the retained claim before
// capability and permission are consulted. Withdrawing a right stops new work.
// It does not strand a caller holding an outcome it cannot learn.
//
// Both transitions are exercised because they refuse differently and must
// recover identically: one withdraws this caller's authority, the other the
// server's capability.
func checkCreationOutlivesWithdrawal(ctx context.Context, call creationCall, f CoreFixture, key string, body []byte, created string) error {
	// A server advertising the capability owes the suite a way to take it
	// away, for the same reason it owes Share: otherwise nothing proves what
	// happens when the right that produced an outcome is gone.
	if f.WithdrawCreation == nil || f.DisableCreation == nil {
		return fmt.Errorf("a server offering creation must supply WithdrawCreation and DisableCreation, or nothing proves an outcome survives either")
	}
	recovers := func(why string) error {
		again, replayed, err := call("POST", "/v1/datasets", f.Token, key, body, 201)
		if err != nil {
			return fmt.Errorf("a creation could not be replayed %s", why)
		}
		var repeated struct {
			DatasetID string `json:"dataset_id"`
		}
		if json.Unmarshal(again, &repeated) != nil || repeated.DatasetID != created || !replayed {
			return fmt.Errorf("a replay %s did not return the retained identity", why)
		}
		status, _, err := call("POST", "/v1/operations/"+url.PathEscape(key)+":status", f.Token, "", nil, 200)
		if err != nil {
			return fmt.Errorf("a creation receipt stopped answering %s", why)
		}
		var receipt struct {
			State  string `json:"state"`
			Result struct {
				DatasetID string `json:"dataset_id"`
			} `json:"result"`
		}
		if json.Unmarshal(status, &receipt) != nil || receipt.State != "committed" || receipt.Result.DatasetID != created {
			return fmt.Errorf("a creation receipt lost its identity %s", why)
		}
		return nil
	}

	// The caller's own right goes away. New work is refused as a permission
	// answer, because the server still offers creation to somebody.
	restore, err := f.WithdrawCreation(ctx)
	if err != nil {
		return fmt.Errorf("the fixture could not withdraw create_dataset")
	}
	withdrawnErr := func() error {
		defer restore()
		fresh, e := uuidLike()
		if e != nil {
			return e
		}
		if _, _, e = call("POST", "/v1/datasets", f.Token, fresh, body, 403); e != nil {
			return fmt.Errorf("a creation with the right withdrawn was not refused 403 permission_denied")
		}
		var access struct {
			ServerAccess map[string]bool `json:"server_access"`
		}
		listing, _, e := call("GET", "/v1/datasets", f.Token, "", nil, 200)
		if e != nil {
			return e
		}
		if json.Unmarshal(listing, &access) != nil || access.ServerAccess["create_dataset"] {
			return fmt.Errorf("server_access still reports create_dataset after it was withdrawn")
		}
		return recovers("after the caller's create_dataset right was withdrawn")
	}()
	if withdrawnErr != nil {
		return withdrawnErr
	}

	// The capability goes away. New work is refused as not_supported, because
	// the server offers creation to nobody — a different answer a client must
	// be able to tell from the one above, while recovery is unchanged.
	restore, err = f.DisableCreation(ctx)
	if err != nil {
		return fmt.Errorf("the fixture could not disable creation")
	}
	defer restore()
	fresh, err := uuidLike()
	if err != nil {
		return err
	}
	if _, _, err = call("POST", "/v1/datasets", f.Token, fresh, body, 403); err != nil {
		return fmt.Errorf("a creation with the capability disabled was not refused 403 not_supported")
	}
	document, _, err := call("GET", "/.well-known/tessera", f.Token, "", nil, 200)
	if err != nil {
		return err
	}
	var found struct {
		Capabilities []string       `json:"server_capabilities"`
		Limits       map[string]int `json:"server_limits"`
	}
	if json.Unmarshal(document, &found) != nil {
		return fmt.Errorf("discovery is not readable with creation disabled")
	}
	for _, name := range found.Capabilities {
		if name == "dataset_creation" {
			return fmt.Errorf("dataset_creation is still advertised after creation was disabled")
		}
	}
	// The right is still named, and now false. This is the only configuration
	// where the distinction is observable: a client tells "nothing is
	// provisioned for me here" from "make one" by this member, so a member that
	// disappears when the server offers nothing makes one absence mean two
	// things on the axis that exists to answer the second.
	listing, _, err := call("GET", "/v1/datasets", f.Token, "", nil, 200)
	if err != nil {
		return err
	}
	var disabled struct {
		ServerAccess map[string]bool `json:"server_access"`
	}
	if json.Unmarshal(listing, &disabled) != nil {
		return fmt.Errorf("the listing is not readable with creation disabled")
	}
	if named, present := disabled.ServerAccess["create_dataset"]; !present || named {
		return fmt.Errorf("with creation disabled, server_access must still name create_dataset and report it false")
	}
	// The window stays. Withdrawing the capability says "you cannot create";
	// withdrawing the window would say "you cannot reconcile", and a client
	// that cannot tell them apart has no safe action after a lost response.
	if window, ok := found.Limits["idempotency_retention_seconds"]; !ok || window <= 0 {
		return fmt.Errorf("the server-scoped retention window stopped being advertised when creation was disabled")
	}
	return recovers("after the deployment stopped offering creation")
}

func uuidLike() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	h := hex.EncodeToString(nonce[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// A dataset created with types has them from the moment it exists, and a
// creation whose schema is invalid makes nothing and spends nothing.
//
// The second half is the one a server can get wrong while looking right. If
// the key were claimed before the types were checked, a caller that sent a
// bad schema and then corrected it would find its key bound to the failure and
// have to invent a new one — the rule every other operation follows is that a
// malformed request is refused before its key is claimed.
func checkCreationWithTypes(ctx context.Context, call creationCall, client *http.Client, f CoreFixture, uuid func() (string, error)) error {
	key, err := uuid()
	if err != nil {
		return err
	}
	const typeID = "conformance.created.note"
	invalid := []byte(`{"display_name":"Typed","types":[{"id":"` + typeID + `","name":"Note","fields":[{"id":"body","meta_id":"` + typeID + `","type":"no_such_type"}]}]}`)
	before, err := datasetIDs(ctx, client, f)
	if err != nil {
		return err
	}
	if _, _, err = call("POST", "/v1/datasets", f.Token, key, invalid, 400); err != nil {
		return fmt.Errorf("a creation carrying an invalid type was not refused: %w", err)
	}
	after, err := datasetIDs(ctx, client, f)
	if err != nil {
		return err
	}
	if len(after) != len(before) {
		return fmt.Errorf("a creation refused for an invalid type still made a dataset")
	}
	// The same key, now with a valid schema: it was never claimed, so this is
	// new work rather than a conflict with the refusal.
	valid := []byte(`{"display_name":"Typed","types":[{"id":"` + typeID + `","name":"Note","fields":[{"id":"body","meta_id":"` + typeID + `","type":"text"}]}]}`)
	data, _, err := call("POST", "/v1/datasets", f.Token, key, valid, 201)
	if err != nil {
		return fmt.Errorf("an invalid schema spent the idempotency key: %w", err)
	}
	var made struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(data, &made) != nil || made.DatasetID == "" {
		return fmt.Errorf("a typed creation did not report a dataset identity")
	}
	// Present immediately, read the ordinary way.
	if _, _, err = call("GET", "/v1/datasets/"+made.DatasetID+"/meta/"+typeID, f.Token, "", nil, 200); err != nil {
		return fmt.Errorf("a dataset created with a type does not have it: %w", err)
	}
	// And a replay answers with the same dataset rather than making a second.
	data, replayed, err := call("POST", "/v1/datasets", f.Token, key, valid, 201)
	if err != nil {
		return err
	}
	var again struct {
		DatasetID string `json:"dataset_id"`
	}
	if !replayed || json.Unmarshal(data, &again) != nil || again.DatasetID != made.DatasetID {
		return fmt.Errorf("replaying a typed creation did not answer with the dataset it made")
	}
	return nil
}
