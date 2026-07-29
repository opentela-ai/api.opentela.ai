package keysapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/opentela-ai/api/internal/corsmw"
	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/keysvc"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/store"
)

const maxNameLen = 100
const maxBodyBytes = 4 << 10

// Service is the key-management behavior the handlers depend on. Its methods
// match *keysvc.Service exactly, so the concrete service satisfies it directly.
type Service interface {
	Create(ctx context.Context, userID, name string) (string, store.KeyInfo, error)
	List(ctx context.Context, userID string) ([]store.KeyInfo, error)
	Revoke(ctx context.Context, userID string, id int64) (bool, error)
}

// *keysvc.Service must satisfy Service; catch drift at compile time.
var _ Service = (*keysvc.Service)(nil)

type createRequest struct {
	Name string `json:"name"`
}

type createResponse struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`
	Prefix    string    `json:"prefix"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type keyResponse struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

// Routes builds the /manage/keys handler tree. Shared management middleware is
// applied by the caller so keys, wallets, and instances all share one stack.
func Routes(svc Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /manage/keys", func(w http.ResponseWriter, r *http.Request) {
		userID, _ := UserID(r.Context())
		var req createRequest
		if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if len(req.Name) > maxNameLen {
			http.Error(w, "name too long", http.StatusBadRequest)
			return
		}
		token, info, err := svc.Create(r.Context(), userID, req.Name)
		if errors.Is(err, keysvc.ErrTooManyKeys) {
			http.Error(w, "key limit reached", http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		httputil.WriteJSON(w, http.StatusCreated, createResponse{
			ID: info.ID, Key: token, Prefix: info.Prefix, Name: info.Name, CreatedAt: info.CreatedAt,
		})
	})
	mux.HandleFunc("GET /manage/keys", func(w http.ResponseWriter, r *http.Request) {
		userID, _ := UserID(r.Context())
		keys, err := svc.List(r.Context(), userID)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		out := make([]keyResponse, 0, len(keys))
		for _, k := range keys {
			out = append(out, keyResponse{
				ID: k.ID, Name: k.Name, Prefix: k.Prefix, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt,
			})
		}
		httputil.WriteJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("DELETE /manage/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		userID, _ := UserID(r.Context())
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid key id", http.StatusBadRequest)
			return
		}
		changed, err := svc.Revoke(r.Context(), userID, id)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		if !changed {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

// Router preserves the legacy standalone wiring used by existing tests and by
// older single-surface setups.
func Router(svc Service, v Verifier, corsOrigins []string) http.Handler {
	return corsmw.Middleware(corsOrigins)(principal.Middleware(v, nil, nil)(Routes(svc)))
}
