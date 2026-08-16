package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// newTestStore connects to TEST_DATABASE_URL and applies the schema. It skips the
// test when the variable is unset so unit runs stay hermetic.
func newTestStore(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	p, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(p.Close)
	if err := p.Migrate(ctx, `
		DROP TABLE IF EXISTS withdrawals;
		DROP TABLE IF EXISTS deposit_cursors;
		DROP TABLE IF EXISTS deposit_events;
		DROP TABLE IF EXISTS credit_ledger;
		DROP TABLE IF EXISTS billing_requests;
		DROP TABLE IF EXISTS peer_asks;
		DROP TABLE IF EXISTS account_credits;
		DROP TABLE IF EXISTS node_credential_challenges;
		DROP TABLE IF EXISTS instance_service_acl_rules;
		DROP TABLE IF EXISTS instance_services;
		DROP TABLE IF EXISTS trusted_region_membership_events;
		DROP TABLE IF EXISTS trusted_region_invitations;
		DROP TABLE IF EXISTS trusted_region_memberships;
		DROP TABLE IF EXISTS trusted_regions;
		DROP TABLE IF EXISTS instance_acl_rules;
		DROP TABLE IF EXISTS instances;
		DROP TABLE IF EXISTS wallet_challenges;
		DROP TABLE IF EXISTS user_wallets;
		DROP TABLE IF EXISTS account_identities;
		DROP TABLE IF EXISTS api_keys;
		DROP TABLE IF EXISTS faucet_claims;
	`); err != nil {
		t.Fatalf("drop: %v", err)
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

func TestPostgresValidateLifecycle(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	hash := HashKey("token-1")

	// Unknown key → not valid, no error.
	if id, ok, err := p.Validate(ctx, hash); err != nil || ok || id != "" {
		t.Fatalf("Validate(unknown) = (%q,%v,%v), want (\"\",false,nil)", id, ok, err)
	}

	// Insert → valid (legacy key, no owner).
	if err := p.Insert(ctx, hash, "alice"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id, ok, err := p.Validate(ctx, hash); err != nil || !ok || id != "" {
		t.Fatalf("Validate(active) = (%q,%v,%v), want (\"\",true,nil)", id, ok, err)
	}

	// Revoke → not valid.
	changed, err := p.Revoke(ctx, hash)
	if err != nil || !changed {
		t.Fatalf("Revoke = (%v,%v), want (true,nil)", changed, err)
	}
	if id, ok, err := p.Validate(ctx, hash); err != nil || ok || id != "" {
		t.Fatalf("Validate(revoked) = (%q,%v,%v), want (\"\",false,nil)", id, ok, err)
	}

	// List returns the row.
	rows, err := p.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "alice" || rows[0].Active {
		t.Fatalf("List = %+v, want one inactive row named alice", rows)
	}
}

func TestPostgresUserKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	const alice, bob = "user-alice", "user-bob"

	if n, err := p.CountActiveByUser(ctx, alice); err != nil || n != 0 {
		t.Fatalf("CountActiveByUser(empty) = (%d,%v), want (0,nil)", n, err)
	}

	info, err := p.InsertUserKey(ctx, alice, HashKey("tok-a"), "laptop", "sk-1a2b3c4d")
	if err != nil {
		t.Fatalf("InsertUserKey: %v", err)
	}
	if info.ID == 0 || info.Name != "laptop" || info.Prefix != "sk-1a2b3c4d" || !info.Active {
		t.Fatalf("InsertUserKey returned %+v", info)
	}
	if info.CreatedAt.IsZero() {
		t.Fatal("InsertUserKey CreatedAt is zero")
	}

	// The inserted key validates through the existing proxy path.
	if id, ok, err := p.Validate(ctx, HashKey("tok-a")); err != nil || !ok || id != "user-alice" {
		t.Fatalf("Validate(new user key) = (%q,%v,%v), want (\"user-alice\",true,nil)", id, ok, err)
	}

	// Listing is owner-scoped and never leaks another user's keys.
	if _, err := p.InsertUserKey(ctx, bob, HashKey("tok-b"), "bob-key", "sk-9999abcd"); err != nil {
		t.Fatalf("InsertUserKey(bob): %v", err)
	}
	rows, err := p.ListByUser(ctx, alice)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != info.ID || rows[0].Prefix != "sk-1a2b3c4d" {
		t.Fatalf("ListByUser(alice) = %+v, want only alice's key", rows)
	}

	// Bob cannot revoke Alice's key.
	if changed, err := p.RevokeByIDForUser(ctx, bob, info.ID); err != nil || changed {
		t.Fatalf("RevokeByIDForUser(bob, alice's id) = (%v,%v), want (false,nil)", changed, err)
	}
	// Alice can.
	if changed, err := p.RevokeByIDForUser(ctx, alice, info.ID); err != nil || !changed {
		t.Fatalf("RevokeByIDForUser(alice) = (%v,%v), want (true,nil)", changed, err)
	}
	// Revoked keys drop out of the active count.
	if n, err := p.CountActiveByUser(ctx, alice); err != nil || n != 0 {
		t.Fatalf("CountActiveByUser(after revoke) = (%d,%v), want (0,nil)", n, err)
	}
}

func TestPostgresInstanceACLAndWalletLifecycle(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	const alice, bob = "user-alice", "user-bob"

	alicePrimary, err := p.LinkWallet(ctx, alice, "wallet-alice-primary")
	if err != nil || !alicePrimary.Primary {
		t.Fatalf("LinkWallet(alice primary) = (%+v,%v), want primary", alicePrimary, err)
	}
	aliceBackup, err := p.LinkWallet(ctx, alice, "wallet-alice-backup")
	if err != nil || aliceBackup.Primary {
		t.Fatalf("LinkWallet(alice backup) = (%+v,%v), want non-primary", aliceBackup, err)
	}
	bobPrimary, err := p.LinkWallet(ctx, bob, "wallet-bob-primary")
	if err != nil || !bobPrimary.Primary {
		t.Fatalf("LinkWallet(bob primary) = (%+v,%v), want primary", bobPrimary, err)
	}
	if _, err := p.LinkWallet(ctx, alice, alicePrimary.Wallet); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate same-account wallet err=%v, want ErrConflict", err)
	}
	if _, err := p.LinkWallet(ctx, bob, alicePrimary.Wallet); !errors.Is(err, ErrWalletOtherAccount) {
		t.Fatalf("cross-account wallet err=%v, want ErrWalletOtherAccount", err)
	}

	now := time.Now().UTC()
	inst, err := p.CreateInstance(ctx, InstanceInfo{
		AccountID:           alice,
		PeerID:              "peer-lifecycle",
		Label:               "Alice worker",
		OwnerWallet:         alicePrimary.Wallet,
		AccessMode:          "restricted",
		OwnershipStatus:     "active",
		ObservedWallet:      &alicePrimary.Wallet,
		OwnershipObservedAt: &now,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if _, err := p.CreateInstance(ctx, InstanceInfo{
		AccountID: alice, PeerID: "peer-wrong-wallet", OwnerWallet: bobPrimary.Wallet,
		AccessMode: "restricted", OwnershipStatus: "active",
	}); err == nil {
		t.Fatal("CreateInstance accepted wallet owned by another account")
	}

	updated, err := p.ReplaceInstanceACL(ctx, alice, inst.ID, "restricted", "active", &alicePrimary.Wallet, &now, []ACLRule{
		{Kind: "email_domain", Value: "example.com"},
		{Kind: "wallet", Value: bobPrimary.Wallet},
	})
	if err != nil {
		t.Fatalf("ReplaceInstanceACL: %v", err)
	}
	if updated.PolicyRevision != inst.PolicyRevision+1 || len(updated.Rules) != 2 {
		t.Fatalf("updated instance=%+v, want revision increment and two rules", updated)
	}
	managed, err := p.ListManagedInstancesByPeerIDs(ctx, []string{inst.PeerID})
	if err != nil || len(managed) != 1 || len(managed[0].Rules) != 2 {
		t.Fatalf("ListManagedInstancesByPeerIDs = (%+v,%v), want instance with rules", managed, err)
	}
	if _, err := p.DeleteWalletByIDForUser(ctx, alice, alicePrimary.ID); !errors.Is(err, ErrWalletInUse) {
		t.Fatalf("DeleteWallet(active owner) err=%v, want ErrWalletInUse", err)
	}

	reclaimed, err := p.ReclaimInstance(ctx, inst.ID, bob, bobPrimary.Wallet, &bobPrimary.Wallet, &now)
	if err != nil {
		t.Fatalf("ReclaimInstance: %v", err)
	}
	if reclaimed.AccountID != bob || reclaimed.AccessMode != "restricted" || len(reclaimed.Rules) != 0 {
		t.Fatalf("reclaimed=%+v, want Bob restricted owner-only", reclaimed)
	}
	got, err := p.GetInstanceByPeerID(ctx, inst.PeerID)
	if err != nil || got.AccountID != bob || len(got.Rules) != 0 {
		t.Fatalf("GetInstanceByPeerID after reclaim = (%+v,%v)", got, err)
	}

	changed, err := p.DeleteWalletByIDForUser(ctx, alice, alicePrimary.ID)
	if err != nil || !changed {
		t.Fatalf("DeleteWallet(released owner) = (%v,%v), want true,nil", changed, err)
	}
	aliceWallets, err := p.ListWalletsByUser(ctx, alice)
	if err != nil || len(aliceWallets) != 1 || aliceWallets[0].ID != aliceBackup.ID || !aliceWallets[0].Primary {
		t.Fatalf("Alice wallets after primary delete = (%+v,%v), want promoted backup", aliceWallets, err)
	}
}

func TestPostgresWalletChallengeSingleUse(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	now := time.Now().UTC()
	challenge := WalletChallenge{
		ID: "challenge-race", AccountID: "user-alice", Wallet: "wallet-alice",
		Nonce: "nonce", Message: "message", IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := p.CreateWalletChallenge(ctx, challenge); err != nil {
		t.Fatalf("CreateWalletChallenge: %v", err)
	}

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- p.ConsumeWalletChallenge(ctx, challenge.AccountID, challenge.ID, now) }()
	}
	var successes, consumed int
	for i := 0; i < 2; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrChallengeConsumed):
			consumed++
		default:
			t.Fatalf("ConsumeWalletChallenge err=%v", err)
		}
	}
	if successes != 1 || consumed != 1 {
		t.Fatalf("challenge race successes=%d consumed=%d, want 1/1", successes, consumed)
	}
}

func TestPostgresConcurrentInstanceClaim(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	aliceWallet, err := p.LinkWallet(ctx, "user-alice", "wallet-alice")
	if err != nil {
		t.Fatalf("LinkWallet(alice): %v", err)
	}
	bobWallet, err := p.LinkWallet(ctx, "user-bob", "wallet-bob")
	if err != nil {
		t.Fatalf("LinkWallet(bob): %v", err)
	}
	now := time.Now().UTC()
	inputs := []InstanceInfo{
		{AccountID: "user-alice", PeerID: "peer-race", OwnerWallet: aliceWallet.Wallet, AccessMode: "restricted", OwnershipStatus: "active", ObservedWallet: &aliceWallet.Wallet, OwnershipObservedAt: &now},
		{AccountID: "user-bob", PeerID: "peer-race", OwnerWallet: bobWallet.Wallet, AccessMode: "restricted", OwnershipStatus: "active", ObservedWallet: &bobWallet.Wallet, OwnershipObservedAt: &now},
	}
	results := make(chan error, len(inputs))
	for _, input := range inputs {
		input := input
		go func() {
			_, err := p.CreateInstance(ctx, input)
			results <- err
		}()
	}
	var successes, conflicts int
	for range inputs {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("CreateInstance race err=%v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("claim race successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}
}

func TestPostgresServicePolicyReleaseInvariant(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	const owner = "user-owner"

	wallet, err := p.LinkWallet(ctx, owner, "wallet-owner")
	if err != nil {
		t.Fatalf("LinkWallet: %v", err)
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	inst, err := p.CreateInstance(ctx, InstanceInfo{
		AccountID:           owner,
		PeerID:              "peer-policy",
		OwnerWallet:         wallet.Wallet,
		AccessMode:          "restricted",
		OwnershipStatus:     "active",
		ObservedWallet:      &wallet.Wallet,
		OwnershipObservedAt: &now,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	region, err := p.CreateRegion(ctx, RegionInfo{Slug: "research-eu", Name: "Research EU", OwnerAccountID: owner, Status: "active"})
	if err != nil {
		t.Fatalf("CreateRegion: %v", err)
	}
	invite, err := p.CreateRegionInvitation(ctx, owner, region.ID, inst.ID, "worker", "accept-token", now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("CreateRegionInvitation: %v", err)
	}
	if _, err := p.AcceptRegionInvitation(ctx, owner, region.ID, inst.ID, "accept-token", now); err != nil {
		t.Fatalf("AcceptRegionInvitation(%d): %v", invite.ID, err)
	}

	updated, err := p.ReplaceInstanceServicePolicy(ctx, owner, inst.ID, ReplaceServicePolicyInput{
		PolicyScope: PolicyScopeService,
		Inventory: ServiceInventory{
			ObservedAt:       now,
			SupportsPolicyV2: true,
			Services:         []ObservedService{{Name: "llm-private", Count: 1}},
		},
		Services: []InstanceService{{
			ServiceName: "llm-private",
			Exposure:    ExposureTrustedRegion,
			RegionID:    &region.ID,
			AccessMode:  AccessModePublic,
		}},
	})
	if err != nil {
		t.Fatalf("ReplaceInstanceServicePolicy(service): %v", err)
	}
	if updated.PolicyScope != PolicyScopeService || len(updated.Services) != 1 {
		t.Fatalf("updated=%+v", updated)
	}
	if changed, err := p.ReleaseMembership(ctx, owner, inst.ID); !errors.Is(err, ErrConflict) || changed {
		t.Fatalf("ReleaseMembership with trusted binding = (%v,%v), want conflict", changed, err)
	}
	updated, err = p.ReplaceInstanceServicePolicy(ctx, owner, inst.ID, ReplaceServicePolicyInput{
		PolicyScope:           PolicyScopePeer,
		AcknowledgeScopeReset: true,
	})
	if err == nil {
		t.Fatalf("ReplaceInstanceServicePolicy(peer) succeeded unexpectedly with trusted binding present: %+v", updated)
	}
	updated, err = p.ReplaceInstanceServicePolicy(ctx, owner, inst.ID, ReplaceServicePolicyInput{
		PolicyScope: PolicyScopeService,
		Inventory: ServiceInventory{
			ObservedAt:       now,
			SupportsPolicyV2: true,
			Services:         []ObservedService{{Name: "llm-private", Count: 1}},
		},
		Services: []InstanceService{{
			ServiceName: "llm-private",
			Exposure:    ExposureDisabled,
			AccessMode:  AccessModePublic,
		}},
	})
	if err != nil {
		t.Fatalf("ReplaceInstanceServicePolicy(disable): %v", err)
	}
	if changed, err := p.ReleaseMembership(ctx, owner, inst.ID); err != nil || !changed {
		t.Fatalf("ReleaseMembership after disable = (%v,%v), want true,nil", changed, err)
	}
}

// TestPostgresRegionMigrationGuarded verifies that accepting an invitation to
// a new region is rejected with ErrRegionMigrationConflict while trusted
// service bindings tied to the old region still exist. Without the guard,
// the ON CONFLICT DO UPDATE would silently move the membership and the FK
// on instance_services(region_id, instance_id) would dangle.
func TestPostgresRegionMigrationGuarded(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	const owner = "user-owner"

	wallet, err := p.LinkWallet(ctx, owner, "wallet-owner")
	if err != nil {
		t.Fatalf("LinkWallet: %v", err)
	}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	inst, err := p.CreateInstance(ctx, InstanceInfo{
		AccountID:           owner,
		PeerID:              "peer-migrate",
		OwnerWallet:         wallet.Wallet,
		AccessMode:          "restricted",
		OwnershipStatus:     "active",
		ObservedWallet:      &wallet.Wallet,
		OwnershipObservedAt: &now,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	regionA, err := p.CreateRegion(ctx, RegionInfo{Slug: "region-a", Name: "A", OwnerAccountID: owner, Status: "active"})
	if err != nil {
		t.Fatalf("CreateRegion A: %v", err)
	}
	regionB, err := p.CreateRegion(ctx, RegionInfo{Slug: "region-b", Name: "B", OwnerAccountID: owner, Status: "active"})
	if err != nil {
		t.Fatalf("CreateRegion B: %v", err)
	}

	// Admit into region A.
	if _, err := p.CreateRegionInvitation(ctx, owner, regionA.ID, inst.ID, "worker", "token-a", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateRegionInvitation A: %v", err)
	}
	if _, err := p.AcceptRegionInvitation(ctx, owner, regionA.ID, inst.ID, "token-a", now); err != nil {
		t.Fatalf("AcceptRegionInvitation A: %v", err)
	}

	// Add a trusted service binding in region A — this is what makes a
	// silent migration unsafe.
	if _, err := p.ReplaceInstanceServicePolicy(ctx, owner, inst.ID, ReplaceServicePolicyInput{
		PolicyScope: PolicyScopeService,
		Inventory: ServiceInventory{
			ObservedAt:       now,
			SupportsPolicyV2: true,
			Services:         []ObservedService{{Name: "llm", Count: 1}},
		},
		Services: []InstanceService{{
			ServiceName: "llm",
			Exposure:    ExposureTrustedRegion,
			RegionID:    &regionA.ID,
			AccessMode:  AccessModePublic,
		}},
	}); err != nil {
		t.Fatalf("ReplaceInstanceServicePolicy: %v", err)
	}

	// Accepting an invitation into region B must be rejected, not silently
	// move the membership.
	if _, err := p.CreateRegionInvitation(ctx, owner, regionB.ID, inst.ID, "worker", "token-b", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateRegionInvitation B: %v", err)
	}
	if _, err := p.AcceptRegionInvitation(ctx, owner, regionB.ID, inst.ID, "token-b", now); !errors.Is(err, ErrRegionMigrationConflict) {
		t.Fatalf("AcceptRegionInvitation B err=%v, want ErrRegionMigrationConflict", err)
	}

	// After disabling the binding, migration is permitted.
	if _, err := p.ReplaceInstanceServicePolicy(ctx, owner, inst.ID, ReplaceServicePolicyInput{
		PolicyScope: PolicyScopeService,
		Inventory: ServiceInventory{
			ObservedAt:       now,
			SupportsPolicyV2: true,
			Services:         []ObservedService{{Name: "llm", Count: 1}},
		},
		Services: []InstanceService{{
			ServiceName: "llm",
			Exposure:    ExposureDisabled,
			AccessMode:  AccessModePublic,
		}},
	}); err != nil {
		t.Fatalf("ReplaceInstanceServicePolicy(disable): %v", err)
	}
	if _, err := p.AcceptRegionInvitation(ctx, owner, regionB.ID, inst.ID, "token-b", now); err != nil {
		t.Fatalf("AcceptRegionInvitation B after disable err=%v, want nil", err)
	}
}

// TestFaucetClaimLifecycle exercises the two-phase claim flow against a real
// Postgres: a pending reservation blocks a concurrent insert, completing the
// claim records the signature, and clearing a pending claim allows a retry.
func TestFaucetClaimLifecycle(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const accountID = "acct-faucet-lifecycle"

	// No claim yet.
	if _, err := p.GetFaucetClaim(ctx, accountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFaucetClaim before claim: err=%v, want ErrNotFound", err)
	}

	// Phase 1: reserve a pending claim (empty signature).
	reserved, err := p.ClaimFaucet(ctx, accountID, "WalletOne", "", 1_000_000_000)
	if err != nil || !reserved {
		t.Fatalf("ClaimFaucet reserve: reserved=%v err=%v", reserved, err)
	}

	// A concurrent reservation for the same account must be rejected (the
	// pending row is recent, not stale).
	again, err := p.ClaimFaucet(ctx, accountID, "WalletOne", "", 1_000_000_000)
	if err != nil || again {
		t.Fatalf("ClaimFaucet duplicate: again=%v err=%v, want false nil", again, err)
	}

	// The pending claim should not yet look "claimed" to the status endpoint.
	claim, err := p.GetFaucetClaim(ctx, accountID)
	if err != nil {
		t.Fatalf("GetFaucetClaim pending: %v", err)
	}
	if claim.TxSignature != "" {
		t.Fatalf("pending claim tx_signature = %q, want empty", claim.TxSignature)
	}

	// Phase 3: complete the claim with the on-chain signature.
	if err := p.CompleteFaucetClaim(ctx, accountID, "sig-final"); err != nil {
		t.Fatalf("CompleteFaucetClaim: %v", err)
	}

	// Completing again must fail (the row is no longer pending).
	if err := p.CompleteFaucetClaim(ctx, accountID, "sig-late"); err == nil {
		t.Fatal("CompleteFaucetClaim on completed claim: err=nil, want error")
	}

	claim, err = p.GetFaucetClaim(ctx, accountID)
	if err != nil {
		t.Fatalf("GetFaucetClaim after complete: %v", err)
	}
	if claim.TxSignature != "sig-final" {
		t.Fatalf("completed claim tx_signature = %q, want sig-final", claim.TxSignature)
	}

	// A completed claim blocks a new reservation.
	again, err = p.ClaimFaucet(ctx, accountID, "WalletOne", "", 1_000_000_000)
	if err != nil || again {
		t.Fatalf("ClaimFaucet after complete: again=%v err=%v, want false nil", again, err)
	}

	// Clearing a completed claim must be a no-op (it is never cleared).
	if err := p.ClearFaucetClaim(ctx, accountID); err != nil {
		t.Fatalf("ClearFaucetClaim on completed: %v", err)
	}
	if _, err := p.GetFaucetClaim(ctx, accountID); err != nil {
		t.Fatalf("completed claim should still exist after ClearFaucetClaim: %v", err)
	}
}

// TestFaucetClaimClearPending verifies that clearing a pending claim (on-chain
// send failure) lets the account retry.
func TestFaucetClaimClearPending(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const accountID = "acct-faucet-clear"

	if _, err := p.ClaimFaucet(ctx, accountID, "WalletTwo", "", 500); err != nil {
		t.Fatalf("ClaimFaucet reserve: %v", err)
	}
	if err := p.ClearFaucetClaim(ctx, accountID); err != nil {
		t.Fatalf("ClearFaucetClaim: %v", err)
	}
	if _, err := p.GetFaucetClaim(ctx, accountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFaucetClaim after clear: err=%v, want ErrNotFound", err)
	}
	// The account can now reserve again.
	reserved, err := p.ClaimFaucet(ctx, accountID, "WalletTwo", "", 500)
	if err != nil || !reserved {
		t.Fatalf("ClaimFaucet after clear: reserved=%v err=%v", reserved, err)
	}
}
