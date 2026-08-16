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
