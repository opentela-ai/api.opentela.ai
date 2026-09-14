package billingapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/billingapi"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/store"
)

type verifierStub struct{ sub string }

func (v verifierStub) Verify(context.Context, string) (neonauth.Claims, error) {
	return neonauth.Claims{Subject: v.sub, Email: "alice@example.com", EmailVerified: true}, nil
}

// authed routes the Service through the same principal.Middleware the manage
// router applies, fixing the owning account id.
func authed(t *testing.T, svc *billingapi.Service, sub string) http.Handler {
	t.Helper()
	return principal.Middleware(verifierStub{sub: sub}, nil, func() time.Time {
		return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	})(svc.Routes())
}

func ptr64(v int64) *int64 { return &v }

// newTestStore connects to its OWN Postgres database (a sibling of
// TEST_DATABASE_URL, suffixed `_api`) so that billingapi integration tests
// never collide with the `internal/store` package's DB-backed tests when
// `go test` runs the two packages in parallel. It applies the full migration
// set on a fresh database and skips when TEST_DATABASE_URL is unset so the
// default `go test ./...` run stays hermetic.
func newTestStore(t *testing.T) *store.Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()

	// Derive a sibling DSN with the path's trailing database name replaced.
	// e.g. postgres://u:p@h:5433/otela_billing_test -> .../otela_billing_test_api
	// so billingapi and the `internal/store` package never share a database
	// when `go test` runs them in parallel. We create it once (best effort;
	// ignore "already exists") via a maintenance connection to `postgres`.
	var apiDSN string
	if base, name, query, ok := splitDatabaseName(dsn); ok {
		apiDSN = base + name + "_api" + query
		if admin, err := store.NewPostgres(ctx, base+"postgres"+query); err == nil {
			_ = admin.Migrate(ctx, "CREATE DATABASE "+name+"_api;")
			admin.Close()
		}
	}
	if apiDSN == "" {
		apiDSN = dsn // serial runs are still fine.
	}

	p, err := store.NewPostgres(ctx, apiDSN)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(p.Close)
	if err := p.Migrate(ctx, `
		DROP SCHEMA IF EXISTS public CASCADE;
		CREATE SCHEMA public;
	`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	sort.Strings(files)
	for _, file := range files {
		ddl, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read migration %s: %v", file, err)
		}
		if err := p.Migrate(ctx, string(ddl)); err != nil {
			t.Fatalf("migrate %s: %v", file, err)
		}
	}
	return p
}

// splitDatabaseName splits a libpq URL DSN at its trailing path segment
// (the database name) and returns (prefix, name, ok). prefix includes the
// trailing slash. ok is false if the DSN has no path segment.
// splitDatabaseName splits a Postgres DSN of the form
// "postgres://user:pass@host:port/dbname?params" into the part up to and
// including the final slash (without the query), the database name, and the
// trailing query string. The query is returned separately so derived sibling
// DSNs keep the same connection options (e.g. ?sslmode=disable); a query is
// no longer treated as an unparseable name.
func splitDatabaseName(dsn string) (base, name, query string, ok bool) {
	idx := strings.LastIndex(dsn, "/")
	if idx < 0 {
		return "", "", "", false
	}
	tail := dsn[idx+1:]
	if q := strings.Index(tail, "?"); q >= 0 {
		return dsn[:idx+1], tail[:q], tail[q:], true
	}
	return dsn[:idx+1], tail, "", true
}

func TestDBStateAndPreferencesRoundTrip(t *testing.T) {
	pg := newTestStore(t)
	ctx := context.Background()
	acct := "acct-db-1"

	// Seed a credit account with caps and a credited deposit so the wallet
	// state has a non-zero balance to report.
	if _, err := pg.SetAccountCaps(ctx, acct, billing.Caps{
		InputPerMillion: ptr64(2000), OutputPerMillion: ptr64(6000),
	}); err != nil {
		t.Fatalf("SetAccountCaps: %v", err)
	}
	if _, err := pg.InsertDepositEvent(ctx, billing.DepositEvent{
		TransactionSignature: "sig-seed", InstructionIndex: 0, Slot: 100,
		FromWallet: "WalletSeed", AmountRaw: 5_000_000, SeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertDepositEvent: %v", err)
	}
	if _, err := pg.ApplyDeposit(ctx, "sig-seed", 0, acct, time.Now().UTC()); err != nil {
		t.Fatalf("ApplyDeposit: %v", err)
	}

	svc := billingapi.New(pg, config.BillingEnforce, "TreasuryATA", "Mint", "TokenProg", 9, true, nil)

	// GET /manage/billing → seeded cap and the deposit credit.
	req := httptest.NewRequest(http.MethodGet, "/manage/billing", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, acct).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET state code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Balance struct {
			CreditRaw string `json:"credit_raw"`
		} `json:"balance"`
		Caps struct {
			InputPerMillion       json.RawMessage `json:"input_per_million"`
			OutputPerMillion      json.RawMessage `json:"output_per_million"`
			CachedInputPerMillion json.RawMessage `json:"cached_input_per_million"`
		} `json:"caps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Balance.CreditRaw != "5000000" {
		t.Fatalf("credit=%q, want 5000000", got.Balance.CreditRaw)
	}
	if string(got.Caps.InputPerMillion) != "2000" {
		t.Fatalf("input cap=%s, want 2000", got.Caps.InputPerMillion)
	}
	if string(got.Caps.CachedInputPerMillion) != "null" {
		t.Fatalf("cached cap=%s, want null", got.Caps.CachedInputPerMillion)
	}

	// PATCH /manage/billing/preferences → updates the DB and returns new caps.
	patch := httptest.NewRequest(http.MethodPatch, "/manage/billing/preferences",
		strings.NewReader(`{"max_input_per_million":3000,"max_output_per_million":9000}`))
	patch.Header.Set("Authorization", "Bearer token")
	prec := httptest.NewRecorder()
	authed(t, svc, acct).ServeHTTP(prec, patch)
	if prec.Code != http.StatusOK {
		t.Fatalf("PATCH code=%d body=%s", prec.Code, prec.Body.String())
	}
	got2, err := pg.AccountCredit(ctx, acct)
	if err != nil {
		t.Fatalf("AccountCredit: %v", err)
	}
	if got2.MaxInputPerMillion == nil || *got2.MaxInputPerMillion != 3000 {
		t.Fatalf("DB input cap=%v, want 3000", got2.MaxInputPerMillion)
	}
	if got2.MaxOutputPerMillion == nil || *got2.MaxOutputPerMillion != 9000 {
		t.Fatalf("DB output cap=%v, want 9000", got2.MaxOutputPerMillion)
	}
}

func TestDBLedgerCursorRoundTrip(t *testing.T) {
	pg := newTestStore(t)
	ctx := context.Background()
	acct := "acct-db-2"

	// Seed two credited deposits → two ledger legs to page over.
	for i, sig := range []string{"sig-a", "sig-b"} {
		if _, err := pg.InsertDepositEvent(ctx, billing.DepositEvent{
			TransactionSignature: sig, InstructionIndex: 0, Slot: int64(200 + i),
			FromWallet: "WalletDB", AmountRaw: int64(1_000_000 + i*1_000_000), SeenAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("InsertDepositEvent %s: %v", sig, err)
		}
		if _, err := pg.ApplyDeposit(ctx, sig, 0, acct, time.Now().UTC()); err != nil {
			t.Fatalf("ApplyDeposit %s: %v", sig, err)
		}
	}

	svc := billingapi.New(pg, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	// First page: limit=1 → exactly one entry + a next cursor.
	req := httptest.NewRequest(http.MethodGet, "/manage/billing/ledger?limit=1", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, acct).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ledger code=%d body=%s", rec.Code, rec.Body.String())
	}
	var page1 struct {
		Entries []struct {
			ID  int64  `json:"id"`
			Ref string `json:"ref"`
		} `json:"entries"`
		Next string `json:"next"`
	}
	json.Unmarshal(rec.Body.Bytes(), &page1)
	if len(page1.Entries) != 1 {
		t.Fatalf("page1 entries=%d, want 1", len(page1.Entries))
	}
	if page1.Next == "" {
		t.Fatal("page1 next cursor missing")
	}
	firstID := page1.Entries[0].ID

	// Second page: the next cursor must yield a different (older) entry.
	req2 := httptest.NewRequest(http.MethodGet, "/manage/billing/ledger?cursor="+page1.Next, nil)
	req2.Header.Set("Authorization", "Bearer token")
	rec2 := httptest.NewRecorder()
	authed(t, svc, acct).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET ledger page2 code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var page2 struct {
		Entries []struct {
			ID int64 `json:"id"`
		} `json:"entries"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &page2)
	if len(page2.Entries) == 0 {
		t.Fatal("page2 empty, expected the second ledger row")
	}
	if page2.Entries[0].ID == firstID {
		t.Fatal("page2 returned the same row as page1; cursor not advancing")
	}
}

func TestDBDepositsList(t *testing.T) {
	pg := newTestStore(t)
	ctx := context.Background()
	acct := "acct-db-3"

	if _, err := pg.InsertDepositEvent(ctx, billing.DepositEvent{
		TransactionSignature: "sig-d1", InstructionIndex: 0, Slot: 300,
		FromWallet: "WalletD", AmountRaw: 750_000, SeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertDepositEvent d1: %v", err)
	}
	if _, err := pg.ApplyDeposit(ctx, "sig-d1", 0, acct, time.Now().UTC()); err != nil {
		t.Fatalf("ApplyDeposit d1: %v", err)
	}

	svc := billingapi.New(pg, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)
	req := httptest.NewRequest(http.MethodGet, "/manage/billing/deposits", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, acct).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET deposits code=%d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Entries []struct {
			TransactionSignature string `json:"transaction_signature"`
			AmountRaw            string `json:"amount_raw"`
			AssignmentState      string `json:"assignment_state"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].TransactionSignature != "sig-d1" || page.Entries[0].AmountRaw != "750000" {
		t.Fatalf("deposits = %+v", page)
	}
	if page.Entries[0].AssignmentState != "assigned" {
		t.Fatalf("assignment state=%q, want assigned", page.Entries[0].AssignmentState)
	}
}
