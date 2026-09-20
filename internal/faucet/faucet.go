// Package faucet pays out OTELA (an SPL token) from a funded faucet wallet to
// verified accounts' associated token accounts. It builds, signs, and
// broadcasts the Solana transaction using only the standard library plus the
// project's base58 helpers.
package faucet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/opentela-ai/api/internal/solana"
)

var token2022ProgramID = mustDecodeB58("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")

// Service pays OTELA from a single faucet wallet.
type Service struct {
	rpc          *rpcClient
	mint         []byte
	tokenProgram []byte
	token2022    bool
	faucetKey    ed25519.PrivateKey
	faucetPub    []byte
	amountRaw    uint64
}

// New builds a faucet Service. rpcURL is the Solana JSON-RPC endpoint, mint is
// the OTELA mint address, tokenProgram is the SPL Token or Token-2022 program
// id, faucetKey is the faucet wallet's private key (which must hold OTELA in
// its associated token account and enough SOL for fees), and amountRaw is the
// per-claim payout in token base units.
func New(rpcURL, mint, tokenProgram string, faucetKey ed25519.PrivateKey, amountRaw uint64) (*Service, error) {
	if len(faucetKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("faucet: private key must be %d bytes", ed25519.PrivateKeySize)
	}
	mintBytes, err := solana.DecodeBase58(mint, solana.PublicKeyBytes)
	if err != nil || len(mintBytes) != solana.PublicKeyBytes {
		return nil, fmt.Errorf("faucet: invalid mint address: %v", err)
	}
	tpBytes, err := solana.DecodeBase58(tokenProgram, solana.PublicKeyBytes)
	if err != nil || len(tpBytes) != solana.PublicKeyBytes {
		return nil, fmt.Errorf("faucet: invalid token program address: %v", err)
	}
	if rpcURL == "" {
		return nil, fmt.Errorf("faucet: rpc url is required")
	}
	return &Service{
		rpc:          newRPCClient(rpcURL),
		mint:         mintBytes,
		tokenProgram: tpBytes,
		token2022:    bytes.Equal(tpBytes, token2022ProgramID),
		faucetKey:    faucetKey,
		faucetPub:    faucetKey.Public().(ed25519.PublicKey),
		amountRaw:    amountRaw,
	}, nil
}

// FaucetWallet returns the faucet wallet's public key (base58).
func (s *Service) FaucetWallet() string {
	return solana.EncodeBase58(s.faucetPub)
}

// Mint returns the OTELA mint address (base58).
func (s *Service) Mint() string {
	return solana.EncodeBase58(s.mint)
}

// Send transfers one faucet payout to recipientOwner's associated token
// account, creating that account first when needed. It returns the broadcast
// transaction signature.
func (s *Service) Send(ctx context.Context, recipientOwner string) (string, error) {
	owner, err := solana.DecodeBase58(recipientOwner, solana.PublicKeyBytes)
	if err != nil || len(owner) != solana.PublicKeyBytes {
		return "", fmt.Errorf("faucet: invalid recipient wallet: %w", err)
	}
	sourceATA, err := associatedTokenAddress(s.faucetPub, s.mint, s.tokenProgram)
	if err != nil {
		return "", fmt.Errorf("faucet: derive faucet ata: %w", err)
	}
	destATA, err := associatedTokenAddress(owner, s.mint, s.tokenProgram)
	if err != nil {
		return "", fmt.Errorf("faucet: derive recipient ata: %w", err)
	}
	exists, err := s.rpc.accountExists(ctx, destATA)
	if err != nil {
		return "", err
	}
	blockhash, err := s.rpc.latestBlockhash(ctx)
	if err != nil {
		return "", err
	}
	message := buildTransferMessage(
		s.faucetPub, sourceATA, destATA, owner, s.mint,
		s.tokenProgram, !exists, s.token2022, blockhash, s.amountRaw,
	)
	sig, err := s.rpc.sendTransaction(ctx, signAndSerialize(s.faucetKey, message))
	if err != nil {
		return "", err
	}
	return sig, nil
}

// Solana cost constants for the affordability pre-check.
const (
	// Base transaction fee for a signed transaction (per-signature).
	txBaseFeeLamports = 5_000
	// SPL token account data length (165 bytes) — the worst case is the
	// recipient ATA not existing yet and the faucet paying its rent.
	tokenAccountDataLen = 165
)

// FundCheck reports whether the faucet wallet can afford one payout right
// now. NeededLamports assumes the worst case (the recipient's associated
// token account does not exist and its rent must be paid from the faucet's
// SOL) on top of the base transaction fee.
type FundCheck struct {
	TokenRaw       uint64 // OTELA (raw) held by the faucet wallet
	Lamports       uint64 // SOL (lamports) held by the faucet wallet
	NeededTokenRaw uint64 // one payout in raw units
	NeededLamports uint64 // base fee + worst-case recipient ATA rent
	Funded         bool
}

// CheckFunded queries the faucet wallet's current OTELA and SOL balances and
// compares them against the cost of a single payout. RPC failures bubble up;
// callers decide whether to fail closed or skip the check.
func (s *Service) CheckFunded(ctx context.Context) (FundCheck, error) {
	sourceATA, err := associatedTokenAddress(s.faucetPub, s.mint, s.tokenProgram)
	if err != nil {
		return FundCheck{}, fmt.Errorf("faucet: derive faucet ata: %w", err)
	}

	tokenRaw, err := s.rpc.tokenBalance(ctx, sourceATA)
	if err != nil {
		return FundCheck{}, err
	}
	lamports, err := s.rpc.solBalance(ctx, s.faucetPub)
	if err != nil {
		return FundCheck{}, err
	}
	rent, err := s.rpc.rentExemptMinimum(ctx, tokenAccountDataLen)
	if err != nil {
		return FundCheck{}, err
	}

	needed := FundCheck{
		TokenRaw:       tokenRaw,
		Lamports:       lamports,
		NeededTokenRaw: s.amountRaw,
		NeededLamports: txBaseFeeLamports + rent,
	}
	needed.Funded = tokenRaw >= s.amountRaw && lamports >= needed.NeededLamports
	return needed, nil
}
