package deploykeys

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/store"
)

// linkStoreFake implements linkStore in memory. Use consumption mirrors the
// conditional-UPDATE semantics of *store.Postgres.
type linkStoreFake struct {
	keys       map[string]store.DeployKeyInfo
	challenges map[string]store.NodeCredentialChallenge
	instances  map[string]store.InstanceInfo // peer_id -> row
	wallets    map[string][]string           // account -> wallets
	nextID     int64
}

func newLinkStoreFake() *linkStoreFake {
	return &linkStoreFake{
		keys:       map[string]store.DeployKeyInfo{},
		challenges: map[string]store.NodeCredentialChallenge{},
		instances:  map[string]store.InstanceInfo{},
		wallets:    map[string][]string{},
	}
}

func (f *linkStoreFake) seedKey(t *testing.T, userID string, maxUses int) string {
	t.Helper()
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	exp := time.Now().UTC().Add(time.Hour)
	f.keys[store.HashKey(tok)] = store.DeployKeyInfo{
		ID: int64(len(f.keys) + 1), UserID: userID, KeyHash: store.HashKey(tok),
		KeyPrefix: Prefix(tok), MaxUses: maxUses, Active: true, ExpiresAt: &exp,
		CreatedAt: time.Now().UTC(),
	}
	return tok
}

func (f *linkStoreFake) FindActiveDeployKey(_ context.Context, keyHash string, now time.Time) (store.DeployKeyInfo, error) {
	k, ok := f.keys[keyHash]
	if !ok || !k.Active || k.RevokedAt != nil || k.Expired(now) {
		return store.DeployKeyInfo{}, store.ErrNotFound
	}
	return k, nil
}

func (f *linkStoreFake) ConsumeDeployKeyUse(_ context.Context, keyHash string, now time.Time) (string, error) {
	k, ok := f.keys[keyHash]
	if !ok || !k.Usable(now) {
		return "", store.ErrNotFound
	}
	k.UseCount++
	f.keys[keyHash] = k
	return k.UserID, nil
}

func (f *linkStoreFake) CreateInstanceLinkChallenge(_ context.Context, ch store.NodeCredentialChallenge) error {
	f.challenges[ch.ID] = ch
	return nil
}

func (f *linkStoreFake) ConsumeNodeCredentialChallenge(_ context.Context, id, peerID, nonce, audience string, now time.Time) (store.NodeCredentialChallenge, error) {
	ch, ok := f.challenges[id]
	if !ok || ch.ConsumedAt != nil || hashNonce(nonce) != ch.NonceHash {
		return store.NodeCredentialChallenge{}, store.ErrNotFound
	}
	if ch.PeerID != peerID || ch.Audience != audience {
		return store.NodeCredentialChallenge{}, store.ErrNotFound
	}
	if now.After(ch.ExpiresAt) {
		return store.NodeCredentialChallenge{}, store.ErrChallengeExpired
	}
	consumed := now
	ch.ConsumedAt = &consumed
	f.challenges[id] = ch
	return ch, nil
}

func hashNonce(nonce string) string { return store.HashKey(nonce) }

func (f *linkStoreFake) GetInstanceByPeerID(_ context.Context, peerID string) (store.InstanceInfo, error) {
	inst, ok := f.instances[peerID]
	if !ok {
		return store.InstanceInfo{}, store.ErrNotFound
	}
	return inst, nil
}

func (f *linkStoreFake) CreateInstance(_ context.Context, in store.InstanceInfo) (store.InstanceInfo, error) {
	if _, exists := f.instances[in.PeerID]; exists {
		return store.InstanceInfo{}, store.ErrConflict
	}
	f.nextID++
	in.ID = f.nextID
	in.CreatedAt = time.Now().UTC()
	in.UpdatedAt = in.CreatedAt
	f.instances[in.PeerID] = in
	return in, nil
}

func (f *linkStoreFake) GetUserWalletSet(_ context.Context, accountID string) ([]string, error) {
	return f.wallets[accountID], nil
}

func (f *linkStoreFake) InsertDeployKey(_ context.Context, userID, keyHash, keyPrefix, name string, maxUses int, expiresAt *time.Time) (store.DeployKeyInfo, error) {
	f.nextID++
	k := store.DeployKeyInfo{ID: f.nextID, UserID: userID, KeyHash: keyHash, KeyPrefix: keyPrefix, Name: name, MaxUses: maxUses, ExpiresAt: expiresAt, Active: true, CreatedAt: time.Now().UTC()}
	f.keys[keyHash] = k
	return k, nil
}
func (f *linkStoreFake) ListDeployKeysByUser(_ context.Context, userID string) ([]store.DeployKeyInfo, error) {
	var out []store.DeployKeyInfo
	for _, k := range f.keys {
		if k.UserID == userID {
			out = append(out, k)
		}
	}
	// Mirror the store's ORDER BY created_at DESC, id DESC (ids are
	// monotonic in the fake, so sorting by id desc is equivalent).
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}
func (f *linkStoreFake) CountActiveDeployKeysByUser(_ context.Context, userID string) (int, error) {
	n := 0
	for _, k := range f.keys {
		if k.UserID == userID && k.RevokedAt == nil {
			n++
		}
	}
	return n, nil
}
func (f *linkStoreFake) RevokeDeployKeyByIDForUser(_ context.Context, userID string, id int64) (string, bool, error) {
	for h, k := range f.keys {
		if k.ID == id && k.UserID == userID && k.RevokedAt == nil {
			now := time.Now().UTC()
			k.RevokedAt = &now
			k.Active = false
			f.keys[h] = k
			return h, true, nil
		}
	}
	return "", false, nil
}

type meshFake struct {
	obs mesh.PeerObservation
	err error
}

func (m meshFake) LookupPeer(context.Context, string) (mesh.PeerObservation, error) {
	return m.obs, m.err
}

func newTestService(t *testing.T, st *linkStoreFake, m linkMesh) *LinkService {
	t.Helper()
	return NewLinkService(st, m, 30*time.Minute, 20)
}

// peerKeypair generates a libp2p keypair and returns the peer ID plus a
// signer producing (public_key, signature) fields for the link request.
func peerKeypair(t *testing.T) (string, func(string) (string, string)) {
	t.Helper()
	priv, pub, err := crypto.GenerateKeyPair(crypto.Ed25519, 256)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	peerID, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("derive peer id: %v", err)
	}
	pubRaw, err := crypto.MarshalPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal pubkey: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pubRaw)
	sign := func(message string) (string, string) {
		sig, err := priv.Sign([]byte(message))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return pubB64, base64.StdEncoding.EncodeToString(sig)
	}
	return peerID.String(), sign
}

func postJSON(t *testing.T, h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func runLink(t *testing.T, svc *LinkService, token, peerID string, sign func(string) (string, string), label string) *httptest.ResponseRecorder {
	t.Helper()
	chRec := postJSON(t, svc.LinkChallengeHandler(), "/internal/instances/link/challenges", token, `{"peer_id":"`+peerID+`"}`)
	if chRec.Code != http.StatusCreated {
		return chRec
	}
	var ch challengeResponse
	if err := json.Unmarshal(chRec.Body.Bytes(), &ch); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	pub, sig := sign(ch.Message)
	body := `{"peer_id":"` + peerID + `","challenge_id":"` + ch.ChallengeID + `","nonce":"` + ch.Nonce +
		`","public_key":"` + pub + `","signature":"` + sig + `","label":"` + label + `"}`
	return postJSON(t, svc.LinkHandler(), "/internal/instances/link", token, body)
}

func TestLinkHappyPath(t *testing.T) {
	st := newLinkStoreFake()
	st.wallets["user-1"] = []string{"WALLET1"}
	svc := newTestService(t, st, meshFake{err: errors.New("mesh down")})
	tok := st.seedKey(t, "user-1", 1)
	peerID, sign := peerKeypair(t)

	rec := runLink(t, svc, tok, peerID, sign, "gpu-box")
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	inst, ok := st.instances[peerID]
	if !ok {
		t.Fatal("instance not created")
	}
	if inst.AccountID != "user-1" || inst.OwnerWallet != "WALLET1" {
		t.Fatalf("owner binding: account=%q wallet=%q", inst.AccountID, inst.OwnerWallet)
	}
	if inst.AccessMode != "restricted" || inst.OwnershipStatus != "active" {
		t.Fatalf("mode/status: %q/%q", inst.AccessMode, inst.OwnershipStatus)
	}
	if inst.ObservedWallet != nil {
		t.Fatal("deploy-key instances must carry no wallet observation")
	}
	if st.keys[store.HashKey(tok)].UseCount != 1 {
		t.Fatalf("use not consumed: %+v", st.keys[store.HashKey(tok)])
	}
}

func TestLinkRequiresDeployKey(t *testing.T) {
	svc := newTestService(t, newLinkStoreFake(), meshFake{})
	rec := postJSON(t, svc.LinkChallengeHandler(), "/internal/instances/link/challenges", "", `{"peer_id":"5"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code=%d", rec.Code)
	}
	rec = postJSON(t, svc.LinkChallengeHandler(), "/internal/instances/link/challenges", "sk-notadeploykey", `{"peer_id":"5"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("sk- key: code=%d", rec.Code)
	}
}

func TestLinkRejectsBadProof(t *testing.T) {
	st := newLinkStoreFake()
	st.wallets["user-1"] = []string{"WALLET1"}
	svc := newTestService(t, st, meshFake{})
	tok := st.seedKey(t, "user-1", 3)
	peerID, sign := peerKeypair(t)
	_, other := peerKeypair(t)

	rec := runLink(t, svc, tok, peerID, other, "") // signed by the WRONG node key
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong signer: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(st.instances) != 0 {
		t.Fatal("no instance must be created on bad proof")
	}
	_ = sign
}

func TestLinkExhaustedKey(t *testing.T) {
	st := newLinkStoreFake()
	st.wallets["user-1"] = []string{"WALLET1"}
	svc := newTestService(t, st, meshFake{})
	tok := st.seedKey(t, "user-1", 1)
	peerID, sign := peerKeypair(t)
	peer2, sign2 := peerKeypair(t)

	if rec := runLink(t, svc, tok, peerID, sign, ""); rec.Code != http.StatusCreated {
		t.Fatalf("first link: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := runLink(t, svc, tok, peer2, sign2, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("second link with max_uses=1: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLinkIdempotentRelinkSameAccount(t *testing.T) {
	st := newLinkStoreFake()
	st.wallets["user-1"] = []string{"WALLET1"}
	svc := newTestService(t, st, meshFake{})
	tok := st.seedKey(t, "user-1", 1)
	peerID, sign := peerKeypair(t)

	if rec := runLink(t, svc, tok, peerID, sign, ""); rec.Code != http.StatusCreated {
		t.Fatalf("first: code=%d", rec.Code)
	}
	rec := runLink(t, svc, tok, peerID, sign, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("relink: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"relinked":true`) {
		t.Fatalf("relink flag missing: %s", rec.Body.String())
	}
	if st.keys[store.HashKey(tok)].UseCount != 1 {
		t.Fatalf("relink must not spend a use: %d", st.keys[store.HashKey(tok)].UseCount)
	}
}

func TestLinkRejectsForeignClaim(t *testing.T) {
	st := newLinkStoreFake()
	st.instances["5Qpeerclaimed"] = store.InstanceInfo{ID: 7, PeerID: "5Qpeerclaimed", AccountID: "user-2", OwnerWallet: "W2"}
	svc := newTestService(t, st, meshFake{})
	tok := st.seedKey(t, "user-1", 3)

	// A peer already owned by another account is refused even with a valid
	// proof; use a syntactically valid peer id for the claim attempt.
	peerID, sign := peerKeypair(t)
	rec := runLink(t, svc, tok, peerID, sign, "")
	// peerID differs from the seeded one, so this exercises the create path,
	// not the foreign-claim one; force the foreign case directly:
	st.instances[peerID] = store.InstanceInfo{ID: 8, PeerID: peerID, AccountID: "user-9", OwnerWallet: "W9"}
	rec = runLink(t, svc, tok, peerID, sign, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("foreign claim: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLinkRejectsDifferentWalletObservation(t *testing.T) {
	st := newLinkStoreFake()
	st.wallets["user-1"] = []string{"WALLET1"}
	svc := newTestService(t, st, meshFake{obs: mesh.PeerObservation{Wallet: "SOMEONEELSE", ObservedAt: time.Now().UTC()}})
	tok := st.seedKey(t, "user-1", 3)
	peerID, sign := peerKeypair(t)

	rec := runLink(t, svc, tok, peerID, sign, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("wallet mismatch: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(st.instances) != 0 {
		t.Fatal("no instance must be created on wallet mismatch")
	}
}

func TestLinkRequiresLinkedWallet(t *testing.T) {
	st := newLinkStoreFake() // no wallets for user-1
	svc := newTestService(t, st, meshFake{})
	tok := st.seedKey(t, "user-1", 3)
	peerID, sign := peerKeypair(t)

	rec := runLink(t, svc, tok, peerID, sign, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no wallet: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// verifierStub satisfies principal.Verifier for manage-plane handler tests.
type verifierStub struct{}

func (verifierStub) Verify(context.Context, string) (neonauth.Claims, error) {
	return neonauth.Claims{Subject: "user-alice"}, nil
}

// manageAuthed wraps ManageRoutes in the same principal middleware the
// manage plane applies in production, so handler tests can act as a user.
func manageAuthed(svc *LinkService) http.Handler {
	return principal.Middleware(verifierStub{}, nil, nil)(svc.ManageRoutes())
}

func manageReq(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestManageCreateUnlimitedUses(t *testing.T) {
	svc := newTestService(t, newLinkStoreFake(), meshFake{})
	h := manageAuthed(svc)

	// max_uses: 0 mints an unlimited, expiry-free key (migration 0016).
	rec := manageReq(t, h, http.MethodPost, "/manage/deploy-keys",
		`{"name":"farm","max_uses":0,"ttl_seconds":0}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("unlimited create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created createResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.MaxUses != 0 {
		t.Fatalf("created MaxUses=%d, want 0 (unlimited)", created.MaxUses)
	}
	if created.ExpiresAt != nil {
		t.Fatalf("ttl_seconds=0 must mint an expiry-free key, got %v", created.ExpiresAt)
	}

	// An omitted max_uses keeps the historic finite default.
	rec = manageReq(t, h, http.MethodPost, "/manage/deploy-keys", `{"name":"defaulted"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("default create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var def createResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &def); err != nil {
		t.Fatalf("decode default response: %v", err)
	}
	if def.MaxUses != defaultMaxUses {
		t.Fatalf("omitted max_uses: MaxUses=%d, want %d", def.MaxUses, defaultMaxUses)
	}

	// Negative budgets stay invalid.
	if rec := manageReq(t, h, http.MethodPost, "/manage/deploy-keys", `{"max_uses":-3}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative max_uses: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// The list reports the unlimited key with uses_left = -1.
	rec = manageReq(t, h, http.MethodGet, "/manage/deploy-keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []deployKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("list rows=%d, want 2", len(rows))
	}
	var unlimited *deployKeyResponse
	for i := range rows {
		if rows[i].MaxUses == 0 {
			unlimited = &rows[i]
			break
		}
	}
	if unlimited == nil {
		t.Fatalf("no unlimited row in list: %+v", rows)
	}
	if unlimited.UsesLeft != -1 {
		t.Fatalf("unlimited row uses_left=%d, want -1", unlimited.UsesLeft)
	}
}

func TestManageCRUDLifecycle(t *testing.T) {
	st := newLinkStoreFake()
	svc := newTestService(t, st, meshFake{})

	// The manage plane's principal middleware gates these routes (covered by
	// manageapi's router tests); here we exercise the handler logic directly.
	tok, info, err := svc.crud.Create(context.Background(), "user-1", "box", 2, time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(tok, "otd-") || info.ID == 0 {
		t.Fatalf("minted key shape: %q %+v", tok, info)
	}
	if changed, err := svc.crud.Revoke(context.Background(), "user-1", info.ID); err != nil || !changed {
		t.Fatalf("revoke: %v %v", changed, err)
	}
	// Revoked keys stop authenticating immediately.
	peerID, sign := peerKeypair(t)
	rec := runLink(t, svc, tok, peerID, sign, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: code=%d body=%s", rec.Code, rec.Body.String())
	}
}
