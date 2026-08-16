package faucet

import (
	"crypto/ed25519"
	"fmt"

	"github.com/opentela-ai/api/internal/solana"
)

// Solana program and sysvar addresses used by the faucet transaction. The
// canonical constants now live in internal/solana; these keep the faucet's
// historical call sites byte-identical.
var (
	systemProgramID = solana.SystemProgramID
	rentSysvarID    = solana.RentSysvarID
	ataProgramID    = solana.ATAProgramID
)

func mustDecodeB58(s string) []byte {
	b, err := solana.DecodeBase58(s, 64)
	if err != nil {
		panic(fmt.Sprintf("faucet: invalid built-in address %q: %v", s, err))
	}
	return b
}

// The wire-format builders (solana.AccountMeta, solana.Instruction,
// solana.BuildMessage, solana.SerializeTransaction, solana.TransferInstructionData)
// live in internal/solana/tx.go and are shared with the billing withdrawal
// worker (Step 7). These unexported aliases keep the faucet's historical call
// sites readable while removing the duplication.
type accountMeta = solana.AccountMeta
type instruction = solana.Instruction

// buildMessage serializes a Solana message (the bytes that get signed) from
// account metadata, a blockhash, and instructions. The shared builder derives
// the header (num_required_signatures / num_readonly_*) from the account
// flags, which for the faucet's account list is byte-identical to the previous
// hardcoded header (one writable signer, no read-only signers).
func buildMessage(accounts []accountMeta, blockhash []byte, ixs []instruction) []byte {
	return solana.BuildMessage(accounts, blockhash, ixs)
}

func serializeTransaction(signature, message []byte) []byte {
	return solana.SerializeTransaction(signature, message)
}

// associatedTokenAddress delegates to the shared solana.AssociatedTokenAddress
// (seeds [owner, tokenProgram, mint], ATA program). Kept as a wrapper so the
// faucet's golden vectors exercise the same code path the billing deposit
// watcher relies on.
func associatedTokenAddress(owner, mint, tokenProgram []byte) ([]byte, error) {
	return solana.AssociatedTokenAddress(owner, mint, tokenProgram)
}

// buildTransferMessage composes the signed-message bytes for a faucet payout:
// it transfers amountRaw OTELA from the faucet's associated token account to
// the recipient's associated token account, creating the recipient's account
// first when it does not exist yet (createATA). tokenProgram is the classic
// SPL Token program or Token-2022; for Token-2022 the rent sysvar is omitted
// from the create-ATA instruction.
func buildTransferMessage(
	faucet, sourceATA, destATA, recipient, mint []byte,
	tokenProgram []byte, createATA bool, token2022 bool, blockhash []byte, amountRaw uint64,
) []byte {
	// Account order:
	//   0 faucet      (signer, writable)
	//   1 source ATA  (writable)
	//   2 dest ATA    (writable)
	//   3 recipient   (readonly)  — only when creating the ATA
	//   4 mint        (readonly)  — only when creating the ATA
	//   5 system prog (readonly)  — only when creating the ATA
	//   6 ATA program (readonly)  — only when creating the ATA
	//   7 token prog  (readonly)
	//   8 rent        (readonly)  — only when creating the ATA (classic SPL)
	accounts := []accountMeta{
		{Key: faucet, Signer: true, Writable: true},
		{Key: sourceATA, Writable: true},
		{Key: destATA, Writable: true},
	}
	var ixs []instruction
	if createATA {
		accounts = append(accounts,
			accountMeta{Key: recipient},
			accountMeta{Key: mint},
			accountMeta{Key: systemProgramID},
			accountMeta{Key: ataProgramID},
			accountMeta{Key: tokenProgram},
		)
		createAccounts := []byte{0, 2, 3, 4, 5, 7} // payer, ata, owner, mint, system, token-prog
		if !token2022 {
			accounts = append(accounts, accountMeta{Key: rentSysvarID})
			createAccounts = append(createAccounts, 8) // rent
		}
		ixs = append(ixs, instruction{
			ProgramIndex: 6, // ATA program
			Accounts:     createAccounts,
			Data:         solana.CreateATAInstructionData,
		})
	} else {
		accounts = append(accounts, accountMeta{Key: tokenProgram})
	}

	// Token program index: index 7 when the recipient ATA is created (the
	// account list is [faucet, source, dest, recipient, mint, system,
	// ata-prog, token-prog(, rent)]), index 3 otherwise.
	tokenProgramIndex := byte(3)
	if createATA {
		tokenProgramIndex = 7
	}
	ixs = append(ixs, instruction{
		ProgramIndex: tokenProgramIndex,
		Accounts:     []byte{1, 2, 0}, // source, destination, authority
		Data:         solana.TransferInstructionData(amountRaw),
	})

	return buildMessage(accounts, blockhash, ixs)
}

// signAndSerialize signs the message with the faucet private key and wraps it
// into the wire format.
func signAndSerialize(priv ed25519.PrivateKey, message []byte) []byte {
	return solana.SignMessage(priv, message)
}
