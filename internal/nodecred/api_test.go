package nodecred

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/store"
)

type challengeStoreStub struct {
	challengeStore
	instance  store.InstanceInfo
	createErr error
}

func (s challengeStoreStub) GetInstanceByPeerID(context.Context, string) (store.InstanceInfo, error) {
	return s.instance, nil
}

func (s challengeStoreStub) CreateNodeCredentialChallenge(context.Context, store.NodeCredentialChallenge) error {
	return s.createErr
}

type challengeMeshStub struct {
	observation mesh.PeerObservation
}

func (s challengeMeshStub) LookupPeer(context.Context, string) (mesh.PeerObservation, error) {
	return s.observation, nil
}

func TestCredentialEndpointsRequireInternalToken(t *testing.T) {
	t.Parallel()
	svc := NewService(nil, nil, nil, nil, 0, "internal-secret")

	for name, handler := range map[string]http.Handler{
		"challenge": handlerOrFatal(t, svc.ChallengeHandler()),
		"issue":     handlerOrFatal(t, svc.IssueHandler()),
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/node-credentials", strings.NewReader("{"))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d, want 401", rec.Code)
			}
			if rec.Header().Get("X-Otela-Control-Auth-Failed") != "true" {
				t.Fatal("missing control-auth failure marker")
			}

			req = httptest.NewRequest(http.MethodPost, "/internal/node-credentials", strings.NewReader("{"))
			req.Header.Set("Authorization", "Bearer internal-secret")
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("authenticated malformed status=%d, want 400", rec.Code)
			}
		})
	}
}

func TestCredentialEndpointsStayClosedWithoutConfiguredToken(t *testing.T) {
	t.Parallel()
	svc := NewService(nil, nil, nil, nil, 0, "")
	req := httptest.NewRequest(http.MethodPost, "/internal/node-credentials/challenge", strings.NewReader("{"))
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	svc.ChallengeHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
}

func TestChallengeRateLimitReturns429(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	pg := challengeStoreStub{
		instance: store.InstanceInfo{
			PeerID: "peer-worker", OwnerWallet: "wallet-owner",
			Membership: &store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "active", NodeRole: "worker"},
		},
		createErr: store.ErrConflict,
	}
	svc := NewService(pg, challengeMeshStub{observation: mesh.PeerObservation{
		PeerID: "peer-worker", Wallet: "wallet-owner", ObservedAt: now,
	}}, nil, nil, time.Minute, "internal-secret")
	svc.now = func() time.Time { return now }
	req := httptest.NewRequest(http.MethodPost, "/internal/node-credentials/challenges", strings.NewReader(`{"peer_id":"peer-worker"}`))
	req.Header.Set("Authorization", "Bearer internal-secret")
	rec := httptest.NewRecorder()
	svc.ChallengeHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("status=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func handlerOrFatal(t *testing.T, handler http.Handler) http.Handler {
	t.Helper()
	if handler == nil {
		t.Fatal("nil handler")
	}
	return handler
}
