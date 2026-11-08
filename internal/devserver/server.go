// Package devserver composes the synthetic, loopback-only development server.
package devserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ekarpov/tessera/internal/devfaults"
	"github.com/ekarpov/tessera/internal/release"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

type Runtime struct {
	Handler   http.Handler
	TokenPath string
	engine    *sqlite.Store
}

func (r *Runtime) Close() error { return r.engine.Close() }

type Options struct {
	Content       bool
	ContentOrigin string
	Reset         bool
	// Faults lets a client ask this synthetic server to lose a response after
	// committing, throttle, or disconnect mid-response. It does nothing unless
	// the binary was built with the devfaults tag, so a release build cannot
	// be talked into faulting.
	Faults bool
}

func Open(ctx context.Context, directory string) (*Runtime, error) {
	return OpenWithOptions(ctx, directory, Options{})
}

func OpenWithOptions(ctx context.Context, directory string, options Options) (result *Runtime, err error) {
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, errors.New("cannot create development directory")
	}
	tokenPath := filepath.Join(directory, "token")
	token, tokenCreated, err := credential(tokenPath)
	if err != nil {
		return nil, err
	}
	databasePath := filepath.Join(directory, "dataset.db")
	_, statErr := os.Stat(databasePath)
	newDatabase := errors.Is(statErr, os.ErrNotExist)
	engine, err := sqlite.Open(ctx, databasePath, p.DevelopmentLimits())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			engine.Close()
		}
	}()
	access := store.Access{Principal: "development", Authorization: "local-token"}
	// Composition names the dataset the first grant is installed on. No grant
	// could name it, because there is no grant yet.
	dataset, err := engine.SoleDataset(ctx)
	if err != nil {
		return nil, err
	}
	if newDatabase || tokenCreated || options.Reset {
		if err = engine.SetAuthorization(ctx, dataset.ID, access, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
			return nil, err
		}
	}
	// The development server advertises every optional read capability it
	// implements, so a client integrating against it exercises the same
	// advertisement and refusal path a deployment will.
	if err = engine.EnableReads(p.OptionalReadCapabilities...); err != nil {
		return nil, err
	}
	if err = engine.SetContentCleanupDirectory(filepath.Join(directory, "content")); err != nil {
		return nil, err
	}
	if options.Content {
		if err = engine.EnableContent(ctx, filepath.Join(directory, "content")); err != nil {
			return nil, err
		}
	}
	if options.Reset && !newDatabase {
		info, e := sole(ctx, engine, access)
		if e != nil {
			return nil, e
		}
		if _, err = engine.Erase(ctx, access, info.ID, info.ID); err != nil {
			return nil, err
		}
	}
	if newDatabase || options.Reset {
		if err = seed(ctx, engine, access, options.Content); err != nil {
			return nil, err
		}
	}
	digest := sha256.Sum256([]byte(token))
	handler, err := server.NewWithOptions(engine, func(ctx context.Context, supplied string) (store.Access, error) {
		if err := ctx.Err(); err != nil {
			return store.Access{}, err
		}
		candidate := sha256.Sum256([]byte(supplied))
		if subtle.ConstantTimeCompare(candidate[:], digest[:]) != 1 {
			return store.Access{}, store.Refuse("authentication_failed")
		}
		return access, nil
	}, server.Options{AuthMethods: []string{"bearer_token"}, ContentOrigin: options.ContentOrigin})
	if err != nil {
		return nil, err
	}
	if options.Content {
		mux := http.NewServeMux()
		mux.Handle("/", handler)
		mux.HandleFunc("GET /.well-known/tessera-content", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_versions": []string{"1"}, "server": map[string]string{"name": "tessera-content", "version": release.Version()}, "auth_methods": []string{"bearer_token"}, "authorization_servers": []string{}})
		})
		handler = mux
	}
	handler = devfaults.Wrap(handler, options.Faults)
	engine.StartMaintenance(ctx)
	return &Runtime{Handler: handler, TokenPath: tokenPath, engine: engine}, nil
}
func credential(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		info, e := os.Stat(path)
		token := strings.TrimSpace(string(data))
		decoded, e2 := base64.RawURLEncoding.DecodeString(token)
		if e != nil || info.Mode().Perm()&0077 != 0 || e2 != nil || len(decoded) != 32 {
			return "", false, errors.New("development credential file must be private and valid")
		}
		return token, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, errors.New("cannot read development credential")
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return "", false, errors.New("cannot create development credential")
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", false, errors.New("cannot create development credential file")
	}
	_, err = file.WriteString(token + "\n")
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return "", false, errors.New("cannot persist development credential")
	}
	return token, true, nil
}
