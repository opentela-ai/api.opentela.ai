package faucetapi

import (
	"context"
	"strings"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/faucet"
	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/store"
)

const (
	testMintB58 = "EsmcTrdLkFqV3mv4CjLF3AmCx132ixfFSYYRWD78cDzR"
	testSPLB58  = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	testWallet  = "wyXQMgDFSzHvCwz1aK79r6Qr5BxqxKFK4Ra8u7xPBhz"
)

// stubVerifier lets tests inject a Neon JWT result without a real JWKS.
type stubVerifier struct {
	claims neonauth.Claims
}

func (v stubVerifier) Verify(ctx context.Context, raw string) (neonauth.Claims, error) {
	return v.claims, nil
}

// fakeStore is an in-memory faucetStore.
type fakeStore struct {
	claims  map[string]store.FaucetClaim
	wallets map[string][]store.WalletInfo
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		claims:  map[string]store.FaucetClaim{},
		wallets: map[string][]store.WalletInfo{},
	}
}

func (f *fakeStore) GetFaucetClaim(ctx context.Context, accountID string) (store.FaucetClaim, error) {
	if c, ok := f.claims[accountID]; ok {
		return c, nil
	}
	return store.FaucetClaim{}, store.ErrNotFound
}

func (f *fakeStore) ClaimFaucet(ctx context.Context, accountID, wallet, txSignature string, amountRaw int64) (bool, error) {
	existing, ok := f.claims[accountID]
	if ok && existing.TxSignature != "" {
		return false, nil // completed claim, cannot re-claim
	}
	// New claim or refresh of a stale pending claim.
	f.claims[accountID] = store.FaucetClaim{
		AccountID: accountID, Wallet: wallet, AmountRaw: amountRaw, TxSignature: txSignature, ClaimedAt: time.Now().UTC(),
	}
	return true, nil
}

func (f *fakeStore) CompleteFaucetClaim(ctx context.Context, accountID, txSignature string) error {
	c, ok := f.claims[accountID]
	if !ok || c.TxSignature != "" {
		return fmt.Errorf("no pending claim for %s", accountID)
	}
	c.TxSignature = txSignature
	f.claims[accountID] = c
	return nil
}

func (f *fakeStore) ClearFaucetClaim(ctx context.Context, accountID string) error {
	if c, ok := f.claims[accountID]; ok && c.TxSignature == "" {
		delete(f.claims, accountID)
	}
	return nil
}

func (f *fakeStore) ListWalletsByUser(ctx context.Context, accountID string) ([]store.WalletInfo, error) {
	return f.wallets[accountID], nil
}

// solanaRPCServer serves canned JSON-RPC responses for the faucet's calls.
// When ataExists is false, getAccountInfo reports a missing account so the
// faucet builds a create-associated-token-account instruction before the
// transfer.
// solanaRPCServer serves canned JSON-RPC responses for the faucet's calls.
// When ataExists is false, getAccountInfo reports a missing account so the
// faucet builds a create-associated-token-account instruction before the
// transfer. When underfunded is true, the balances reported for the faucet
// wallet are zero so the affordability pre-check fails.
func solanaRPCServer(t *testing.T, ataExists bool, underfunded bool) *httptest.Server {
	t.Helper()
	exists := ataExists
	blockhash := "DpvKbXobmX6cS6kXJ9pFVxMD1Z8WHTDGQjzFqLxWQm1y" // 32 bytes base58
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "getLatestBlockhash":
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1},"value":{"blockhash":%q,"lastValidBlockHeight":1}}}`,
				blockhash)))
		case "getAccountInfo":
			if exists {
				// Report the recipient ATA as existing so the transfer runs
				// without a create-ATA instruction.
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1},"value":{"data":["","base58"],"executable":false,"lamports":2039280,"owner":"11111111111111111111111111111111","rentEpoch":0}}}`))
			} else {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1},"value":null}}`))
			}
		case "sendTransaction":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"3nzFcLxM8FZXkz1V2c1cXKqHK7pFh9rQbJhBz1q1q1q1"}`))
		case "getTokenAccountBalance":
			amount := "1000000000000" // plenty for the pre-check
			if underfunded {
				amount = "0"
			}
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1},"value":{"amount":%q,"decimals":9,"uiAmount":0,"uiAmountString":"0"}}}`,
				amount)))
		case "getBalance":
			lamports := uint64(100000000) // plenty for fees + worst-case rent
			if underfunded {
				lamports = 0
			}
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1},"value":%d}}`, lamports)))
		case "getMinimumBalanceForRentExemption":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":2039280}`))
		default:
			t.Errorf("unexpected rpc method %q", req.Method)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
		}
	}))
}

// newTestService builds the API (optionally with a live faucet pointed at a
// canned RPC) wrapped in the real principal middleware with a stub verifier.
// When ataMissing is true the canned RPC reports the recipient's associated
// token account as absent, forcing the faucet to create it first.
func newTestService(t *testing.T, fs *fakeStore, enabled bool, verified bool, ataMissing ...bool) http.Handler {
	return newTestServiceOpts(t, fs, enabled, verified, serviceOpts{ataMissing: len(ataMissing) > 0 && ataMissing[0]})
}

type serviceOpts struct{ ataMissing, underfunded bool }

func newTestServiceOpts(t *testing.T, fs *fakeStore, enabled bool, verified bool, opts serviceOpts) http.Handler {
	t.Helper()
	var svc *faucet.Service
	if enabled {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		rpcSrv := solanaRPCServer(t, !opts.ataMissing, opts.underfunded)
		t.Cleanup(rpcSrv.Close)
		svc, err = faucet.New(rpcSrv.URL, testMintB58, testSPLB58, priv, 1_000_000_000)
		if err != nil {
			t.Fatal(err)
		}
	}
	api := New(fs, svc, 1_000_000_000, 9)
	return principal.Middleware(
		stubVerifier{claims: neonauth.Claims{Subject: "user-1", Email: "a@example.com", EmailVerified: verified}},
		nil, nil,
	)(api.Routes())
}

func doRequest(t *testing.T, h http.Handler, method string) *httptest.ResponseRecorder {
	t.Helper()
	var url string
	if method == http.MethodPost {
		url = "http://example.test/manage/faucet/claim"
	} else {
		url = "http://example.test/manage/faucet"
	}
	req := httptest.NewRequest(method, url, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStatusDisabled(t *testing.T) {
	fs := newFakeStore()
	h := newTestService(t, fs, false, true)
	rec := doRequest(t, h, http.MethodGet)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Enabled {
		t.Error("enabled = true, want false when faucet is disabled")
	}
	if !out.EmailVerified {
		t.Error("email_verified = false, want true")
	}
	if out.Claimed {
		t.Error("claimed = true, want false")
	}
}

func TestStatusUnverified(t *testing.T) {
	fs := newFakeStore()
	h := newTestService(t, fs, true, false)
	rec := doRequest(t, h, http.MethodGet)
	var out statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.EmailVerified {
		t.Error("email_verified = true, want false")
	}
}

func TestStatusClaimed(t *testing.T) {
	fs := newFakeStore()
	fs.claims["user-1"] = store.FaucetClaim{
		AccountID: "user-1", Wallet: testWallet, AmountRaw: 1_000_000_000, TxSignature: "sig-1", ClaimedAt: time.Now().UTC(),
	}
	h := newTestService(t, fs, true, true)
	rec := doRequest(t, h, http.MethodGet)
	var out statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Claimed || out.TxSignature != "sig-1" || out.AmountUI != "1" {
		t.Fatalf("status = %+v, want claimed with amount 1", out)
	}
}

func TestClaimRequiresVerifiedEmail(t *testing.T) {
	fs := newFakeStore()
	fs.wallets["user-1"] = []store.WalletInfo{{ID: 1, AccountID: "user-1", Wallet: testWallet, Primary: true}}
	h := newTestService(t, fs, true, false)
	rec := doRequest(t, h, http.MethodPost)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("claim with unverified email = %d, want 403", rec.Code)
	}
	if len(fs.claims) != 0 {
		t.Error("no claim should be recorded for an unverified email")
	}
}

func TestClaimRequiresWallet(t *testing.T) {
	fs := newFakeStore()
	h := newTestService(t, fs, true, true)
	rec := doRequest(t, h, http.MethodPost)
	if rec.Code != http.StatusConflict {
		t.Fatalf("claim without wallet = %d, want 409", rec.Code)
	}
}

func TestClaimAlreadyClaimed(t *testing.T) {
	fs := newFakeStore()
	fs.wallets["user-1"] = []store.WalletInfo{{ID: 1, AccountID: "user-1", Wallet: testWallet, Primary: true}}
	fs.claims["user-1"] = store.FaucetClaim{AccountID: "user-1", Wallet: testWallet, TxSignature: "old"}
	h := newTestService(t, fs, true, true)
	rec := doRequest(t, h, http.MethodPost)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate claim = %d, want 409", rec.Code)
	}
}

func TestClaimDisabled(t *testing.T) {
	fs := newFakeStore()
	fs.wallets["user-1"] = []store.WalletInfo{{ID: 1, AccountID: "user-1", Wallet: testWallet, Primary: true}}
	h := newTestService(t, fs, false, true)
	rec := doRequest(t, h, http.MethodPost)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("claim when disabled = %d, want 404", rec.Code)
	}
}

func TestClaimSuccess(t *testing.T) {
	fs := newFakeStore()
	fs.wallets["user-1"] = []store.WalletInfo{{ID: 1, AccountID: "user-1", Wallet: testWallet, Primary: true}}
	h := newTestService(t, fs, true, true)
	rec := doRequest(t, h, http.MethodPost)
	if rec.Code != http.StatusCreated {
		body := rec.Body.String()
		t.Fatalf("claim = %d (%s), want 201", rec.Code, body)
	}
	claim, err := fs.GetFaucetClaim(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("claim not recorded: %v", err)
	}
	if claim.Wallet != testWallet {
		t.Errorf("claim wallet = %s, want %s", claim.Wallet, testWallet)
	}
	if claim.TxSignature != "3nzFcLxM8FZXkz1V2c1cXKqHK7pFh9rQbJhBz1q1q1q1" {
		t.Errorf("claim tx = %s, want the canned rpc signature", claim.TxSignature)
	}
	if claim.AmountRaw != 1_000_000_000 {
		t.Errorf("claim amount = %d, want 1000000000", claim.AmountRaw)
	}
}

func TestClaimSuccessCreatesATA(t *testing.T) {
	fs := newFakeStore()
	fs.wallets["user-1"] = []store.WalletInfo{{ID: 1, AccountID: "user-1", Wallet: testWallet, Primary: true}}
	h := newTestService(t, fs, true, true, true) // recipient ATA missing
	rec := doRequest(t, h, http.MethodPost)
	if rec.Code != http.StatusCreated {
		t.Fatalf("claim with missing ATA = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	claim, err := fs.GetFaucetClaim(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("claim not recorded: %v", err)
	}
	if claim.TxSignature != "3nzFcLxM8FZXkz1V2c1cXKqHK7pFh9rQbJhBz1q1q1q1" {
		t.Errorf("claim tx = %s, want the canned rpc signature", claim.TxSignature)
	}
}

func TestClaimSendFailureClearsPending(t *testing.T) {
	fs := newFakeStore()
	fs.wallets["user-1"] = []store.WalletInfo{{ID: 1, AccountID: "user-1", Wallet: testWallet, Primary: true}}
	// A faucet pointed at an unreachable RPC so Send fails.
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := faucet.New("http://127.0.0.1:1", testMintB58, testSPLB58, priv, 1_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	api := New(fs, svc, 1_000_000_000, 9)
	h := principal.Middleware(
		stubVerifier{claims: neonauth.Claims{Subject: "user-1", Email: "a@example.com", EmailVerified: true}},
		nil, nil,
	)(api.Routes())
	rec := doRequest(t, h, http.MethodPost)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("claim with failed send = %d (%s), want 503", rec.Code, rec.Body.String())
	}
	// The pending reservation must be cleared so the account can retry.
	if _, err := fs.GetFaucetClaim(context.Background(), "user-1"); err != store.ErrNotFound {
		t.Fatalf("pending claim not cleared after send failure (err=%v), want ErrNotFound", err)
	}
}

func TestFormatAmount(t *testing.T) {
	cases := []struct {
		raw      uint64
		decimals int
		want     string
	}{
		{1_000_000_000, 9, "1"},
		{1_500_000_000, 9, "1.5"},
		{1_234_567_890, 9, "1.23456789"},
		{500, 2, "5"},
		{123, 0, "123"},
		{0, 9, "0"},
		{10, 9, "0.00000001"},
	}
	for _, c := range cases {
		if got := formatAmount(c.raw, c.decimals); got != c.want {
			t.Errorf("formatAmount(%d, %d) = %q, want %q", c.raw, c.decimals, got, c.want)
		}
	}
}

func TestClaimUnderfundedFailsFast(t *testing.T) {
	fs := newFakeStore()
	fs.wallets["user-1"] = []store.WalletInfo{{Wallet: "wyXQMgDFSzHvCwz1aK79r6Qr5BxqxKFK4Ra8u7xPBhz", Primary: true}}
	h := newTestServiceOpts(t, fs, true, true, serviceOpts{underfunded: true})

	rec := doRequest(t, h, http.MethodPost)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "underfunded") {
		t.Fatalf("body = %q, want an underfunded message", body)
	}
	// The claim must NOT be reserved: a retry after the top-up should work.
	if len(fs.claims) != 0 {
		t.Fatalf("claim reserved despite pre-check: %+v", fs.claims)
	}
}
