package devserver

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

// Seed only a new or explicitly reset dataset; restarting never repairs or
// overwrites a client's edits. Import replay recovers a lost commit response.
// fixtureImage is a 12x6 PNG gradient, small enough to inline and real enough
// to resolve a thumbnail from. Without an image in the fixture a client cannot
// exercise the thumbnails capability the store advertises.
func fixtureImage() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 12, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 12; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 20), G: uint8(y * 40), B: 90, A: 255})
		}
	}
	out := bytes.NewBuffer(nil)
	if png.Encode(out, img) != nil {
		return nil
	}
	return out.Bytes()
}

// sole names the one dataset this development server opens. The engine may hold
// more than one; the development composition provisions exactly one, and would
// rather say so than pick.
func sole(ctx context.Context, engine *sqlite.Store, access store.Access) (store.Info, error) {
	held, err := engine.Datasets(ctx, access)
	if err != nil {
		return store.Info{}, err
	}
	if len(held) != 1 {
		return store.Info{}, store.Refuse("dataset_unavailable")
	}
	return held[0], nil
}

func seed(ctx context.Context, engine *sqlite.Store, access store.Access, content bool) error {
	media, picture := "", ""
	dataset, err := sole(ctx, engine, access)
	if err != nil {
		return err
	}
	if content {
		info := dataset
		opened, err := engine.ContentControl(ctx, access, "begin", info.ContentStoreID, "", []byte(`{"operation_id":"development-fixture-v1","content_type":"text/plain","byte_count":3,"maximum_bytes":3}`))
		if err != nil {
			return err
		}
		imported := opened.(map[string]any)
		if imported["state"] != "committed" {
			token := strings.TrimPrefix(imported["url"].(string), "/v1/transfers/")
			if _, err = engine.ContentTransfer(ctx, token, "PATCH", "bytes 0-2/3", strings.NewReader("abc")); err != nil {
				return err
			}
			committed, err := engine.ContentControl(ctx, access, "commit", info.ContentStoreID, imported["upload_id"].(string), nil)
			if err != nil {
				return err
			}
			imported = committed.(map[string]any)
		}
		media = info.ContentStoreID + "//" + imported["item_id"].(string)

		encoded := fixtureImage()
		size := strconv.Itoa(len(encoded))
		opened, err = engine.ContentControl(ctx, access, "begin", info.ContentStoreID, "",
			[]byte(`{"operation_id":"development-fixture-image-v1","content_type":"image/png","byte_count":`+size+`,"maximum_bytes":`+size+`}`))
		if err != nil {
			return err
		}
		reserved := opened.(map[string]any)
		if reserved["state"] != "committed" {
			token := strings.TrimPrefix(reserved["url"].(string), "/v1/transfers/")
			if _, err = engine.ContentTransfer(ctx, token, "PATCH", "bytes 0-"+strconv.Itoa(len(encoded)-1)+"/"+size, bytes.NewReader(encoded)); err != nil {
				return err
			}
			committed, err := engine.ContentControl(ctx, access, "commit", info.ContentStoreID, reserved["upload_id"].(string), nil)
			if err != nil {
				return err
			}
			reserved = committed.(map[string]any)
		}
		picture = info.ContentStoreID + "//" + reserved["item_id"].(string)
	}
	return engine.Transact(ctx, access, dataset.ID, func(tx store.Tx) error {
		const base = "com.example.note"
		const child = "com.example.entry"
		parent := base
		fields := []p.Field{{ID: "body", MetaID: base, Name: "Body", Type: p.TextType}}
		if _, err := tx.CreateMetadata(p.Meta{ID: base, Name: "Notes", Fields: fields, Roles: []string{"example.base"}, Annotations: map[string]json.RawMessage{"example.layout": json.RawMessage(`{ "z": 1e0, "a": "\u0061" }`)}}); err != nil {
			return err
		}
		fields = []p.Field{{ID: "number", MetaID: child, Name: "Exact integer", Type: p.IntegerType}, {ID: "fraction", MetaID: child, Name: "Float", Type: p.FloatType}, {ID: "payload", MetaID: child, Name: "Opaque JSON", Type: p.JSONType}}
		if content {
			fields = append(fields, p.Field{ID: "attachment", MetaID: child, Name: "Attachment", Type: p.MediaType})
			fields = append(fields, p.Field{ID: "picture", MetaID: child, Name: "Picture", Type: p.MediaType})
		}
		if _, err := tx.CreateMetadata(p.Meta{ID: child, ParentID: &parent, Name: "Entries", Fields: fields, Roles: []string{"example.child"}, Annotations: map[string]json.RawMessage{"example.child": json.RawMessage(`true`)}}); err != nil {
			return err
		}
		body, err := p.NewValue(p.TextType, "Synthetic example — safe to replace")
		if err != nil {
			return err
		}
		if _, err = tx.Insert(base, map[string]p.Value{"body": body}); err != nil {
			return err
		}
		// Decode literal wire values to retain exact integer and signed-zero behavior.
		var values map[string]p.Value
		if err = json.Unmarshal([]byte(`{"body":{"type":"text","value":"Synthetic child"},"number":{"type":"integer","value":9007199254740993},"fraction":{"type":"float","value":-0.0},"payload":{"type":"json","value":"{ \"example\": true }"}}`), &values); err != nil {
			return err
		}
		if content {
			v, e := p.NewValue(p.MediaType, media)
			if e != nil {
				return e
			}
			values["attachment"] = v
			image, e := p.NewValue(p.MediaType, picture)
			if e != nil {
				return e
			}
			values["picture"] = image
		}
		_, err = tx.Insert(child, values)
		return err
	})
}
