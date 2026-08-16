package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/catalog"
)

// catalogStub satisfies the local serviceCatalog interface without a live
// upstream mesh, so catalogAllowlist can be tested in isolation.
type catalogStub struct {
	services []catalog.Service
	err      error
}

func (c *catalogStub) ServicesForPricing(context.Context) ([]catalog.Service, error) {
	return c.services, c.err
}

func TestCatalogAllowlistMapsServiceToAllowEntry(t *testing.T) {
	c := &catalogStub{services: []catalog.Service{
		{Name: "llm", Models: []string{"llama3.1-70b", "llama3.1-8b"}},
		{Name: "embed", Models: []string{"bge-m3"}},
	}}
	entries, err := catalogAllowlist(c).Services(context.Background())
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries)=%d, want 2", len(entries))
	}
	if entries[0].Name != "llm" || len(entries[0].Models) != 2 {
		t.Fatalf("entry0=%+v", entries[0])
	}
	if entries[0].Models[0] != "llama3.1-70b" || entries[0].Models[1] != "llama3.1-8b" {
		t.Fatalf("entry0 models=%v", entries[0].Models)
	}
	if entries[1].Name != "embed" || len(entries[1].Models) != 1 || entries[1].Models[0] != "bge-m3" {
		t.Fatalf("entry1=%+v", entries[1])
	}
}

func TestCatalogAllowlistEmptyYieldsEmpty(t *testing.T) {
	entries, err := catalogAllowlist(&catalogStub{}).Services(context.Background())
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries)=%d, want 0", len(entries))
	}
}

func TestCatalogAllowlistPropagatesError(t *testing.T) {
	want := errors.New("upstream down")
	_, err := catalogAllowlist(&catalogStub{err: want}).Services(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
}

// reconcileStub satisfies depositReconcilerStore without a live Postgres. It
// records the forwarded args (including the now the adapter supplies) so the
// test can assert both forwarding and that the timestamp is real.
type reconcileStub struct {
	gotWallet    string
	gotAccountID string
	gotNow       time.Time
	count        int
	err          error
}

func (s *reconcileStub) ReconcileDepositsForWallet(_ context.Context, wallet, accountID string, now time.Time) (int, error) {
	s.gotWallet = wallet
	s.gotAccountID = accountID
	s.gotNow = now
	return s.count, s.err
}

func TestDepositReconcilerForwardsArgsAndSuppliesNow(t *testing.T) {
	stub := &reconcileStub{count: 7}
	r := depositReconciler{s: stub}
	n, err := r.ReconcileDepositsForWallet(context.Background(), "walletABC", "user-1")
	if err != nil {
		t.Fatalf("ReconcileDepositsForWallet: %v", err)
	}
	if n != 7 {
		t.Fatalf("count=%d, want 7", n)
	}
	if stub.gotWallet != "walletABC" || stub.gotAccountID != "user-1" {
		t.Fatalf("forwarded wallet=%q account=%q, want walletABC/user-1", stub.gotWallet, stub.gotAccountID)
	}
	if stub.gotNow.IsZero() {
		t.Fatal("adapter supplied zero now, want a real timestamp")
	}
}

func TestDepositReconcilerPropagatesError(t *testing.T) {
	want := errors.New("db down")
	r := depositReconciler{s: &reconcileStub{err: want}}
	if _, err := r.ReconcileDepositsForWallet(context.Background(), "w", "u"); !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
}
