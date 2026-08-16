package store

import (
	"context"
	"testing"
	"time"
)

func TestPostgresConsumeNodeCredentialChallengePreservesAudience(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	ch := NodeCredentialChallenge{
		ID:               "challenge-pricing",
		PeerID:           "peer-1",
		RegionSlug:       "eu-west",
		NodeRole:         "seller",
		Audience:         "api.opentela.ai/internal/pricing",
		NonceHash:        hashSecret("nonce-1"),
		ChallengeMessage: "challenge message",
		IssuedAt:         now,
		ExpiresAt:        now.Add(5 * time.Minute),
	}
	if err := p.CreateNodeCredentialChallenge(ctx, ch); err != nil {
		t.Fatalf("CreateNodeCredentialChallenge: %v", err)
	}

	got, err := p.ConsumeNodeCredentialChallenge(ctx, ch.ID, ch.PeerID, "nonce-1", ch.Audience, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ConsumeNodeCredentialChallenge: %v", err)
	}
	if got.Audience != ch.Audience {
		t.Fatalf("Audience = %q, want %q", got.Audience, ch.Audience)
	}
	if got.NonceHash != ch.NonceHash {
		t.Fatalf("NonceHash = %q, want %q", got.NonceHash, ch.NonceHash)
	}
	if got.ChallengeMessage != ch.ChallengeMessage {
		t.Fatalf("ChallengeMessage = %q, want %q", got.ChallengeMessage, ch.ChallengeMessage)
	}
}
