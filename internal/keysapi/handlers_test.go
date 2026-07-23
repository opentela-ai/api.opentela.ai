package keysapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentela-ai/api/internal/keysvc"
	"github.com/opentela-ai/api/internal/store"
)

type fakeService struct {
	created   string
	listErr   error
	revokeOK  bool
	createErr error
}

func (f *fakeService) Create(_ context.Context, userID, name string) (string, store.KeyInfo, error) {
	if f.createErr != nil {
		return "", store.KeyInfo{}, f.createErr
	}
	uid := userID
	return "sk-secretsecret", store.KeyInfo{ID: 7, UserID: &uid, Name: name, Prefix: "sk-secrets", Active: true}, nil
}
func (f *fakeService) List(context.Context, string) ([]store.KeyInfo, error) {
	return []store.KeyInfo{{ID: 7, Name: "laptop", Prefix: "sk-secrets", Active: true}}, f.listErr
}
func (f *fakeService) Revoke(context.Context, string, int64) (bool, error) { return f.revokeOK, nil }

func router(svc Service) http.Handler {
	return Router(svc, fakeVerifier{sub: "alice"}, nil)
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateReturnsKeyOnce(t *testing.T) {
	rec := do(t, router(&fakeService{}), http.MethodPost, "/manage/keys", `{"name":"laptop"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201", rec.Code)
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["key"] != "sk-secretsecret" || got["prefix"] != "sk-secrets" || got["id"].(float64) != 7 {
		t.Fatalf("create body = %v", got)
	}
}

func TestListOmitsSecrets(t *testing.T) {
	rec := do(t, router(&fakeService{}), http.MethodGet, "/manage/keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "key_hash") || strings.Contains(strings.ToLower(body), `"key"`) {
		t.Fatalf("list leaked secret material: %s", body)
	}
	if !strings.Contains(body, "sk-secrets") {
		t.Fatalf("list missing prefix: %s", body)
	}
}

func TestCreateOverCapIs409(t *testing.T) {
	rec := do(t, router(&fakeService{createErr: keysvc.ErrTooManyKeys}), http.MethodPost, "/manage/keys", `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409", rec.Code)
	}
}

func TestCreateBadJSONIs400(t *testing.T) {
	rec := do(t, router(&fakeService{}), http.MethodPost, "/manage/keys", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestRevokeOwnerScoped(t *testing.T) {
	// Unknown/other-owner id → 404.
	rec := do(t, router(&fakeService{revokeOK: false}), http.MethodDelete, "/manage/keys/99", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
	// Owned id → 204.
	rec = do(t, router(&fakeService{revokeOK: true}), http.MethodDelete, "/manage/keys/7", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d, want 204", rec.Code)
	}
	// Non-numeric id → 400.
	rec = do(t, router(&fakeService{}), http.MethodDelete, "/manage/keys/abc", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}
