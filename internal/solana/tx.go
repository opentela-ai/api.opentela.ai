// Package solana wire-format builders shared by the faucet and the billing
// withdrawal worker. Both compose SPL Token transfers (and, when the
// destination token account does not yet exist, an Associated Token Account
// Create) into the Solana message that gets signed. Keeping the construction
// in one place means the withdrawal worker (Step 7) reuses the exact bytes
// the faucet has been broadcasting since launch.
package solana

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
)

// AccountMeta is one entry in a Solana message's account list, carrying the
// 32-byte public key plus its signer/writable flags. The message header's
// num_required_signatures / num_readonly_* are derived from these flags by
// BuildMessage.
type AccountMeta struct {
	Key      []byte
	Signer   bool
	Writable bool
}

// Instruction is one instruction in a Solana message: the program's index
// into the account list, the (index-encoded) account list, and the raw data.
type Instruction struct {
	ProgramIndex byte
	Accounts     []byte
	Data         []byte
}

// AppendCompactU16 writes the variable-length unsigned integer used by the
// Solana wire format into buf.
func AppendCompactU16(buf *bytes.Buffer, v int) {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
		if v == 0 {
			break
		}
	}
}

// BuildMessage serializes a Solana message (the bytes that get signed) from
// account metadata, a recent blockhash, and instructions. The header records
// the count of signers, read-only signers, and read-only unsigned accounts.
func BuildMessage(accounts []AccountMeta, blockhash []byte, ixs []Instruction) []byte {
	var buf bytes.Buffer

	// --- Header ---
	signers := 0
	readonlySigned := 0
	readonlyUnsigned := 0
	for _, a := range accounts {
		if a.Signer {
			signers++
			if !a.Writable {
				readonlySigned++
			}
		} else if !a.Writable {
			readonlyUnsigned++
		}
	}
	buf.WriteByte(byte(signers))
	buf.WriteByte(byte(readonlySigned))
	buf.WriteByte(byte(readonlyUnsigned))

	// --- Account keys ---
	AppendCompactU16(&buf, len(accounts))
	for _, a := range accounts {
		buf.Write(a.Key)
	}

	// --- Recent blockhash ---
	buf.Write(blockhash)

	// --- Instructions ---
	AppendCompactU16(&buf, len(ixs))
	for _, ix := range ixs {
		buf.WriteByte(ix.ProgramIndex)
		AppendCompactU16(&buf, len(ix.Accounts))
		buf.Write(ix.Accounts)
		AppendCompactU16(&buf, len(ix.Data))
		buf.Write(ix.Data)
	}
	return buf.Bytes()
}

// SerializeTransaction wraps a signed message into the Solana wire format:
// compact-u16 signature count, the 64-byte signature, then the message.
func SerializeTransaction(signature, message []byte) []byte {
	var buf bytes.Buffer
	AppendCompactU16(&buf, 1)
	buf.Write(signature)
	buf.Write(message)
	return buf.Bytes()
}

// SignMessage signs message with priv and wraps it into the wire format. It is
// the one-line helper the faucet and withdrawal worker share so both produce
// the identical serialization for a given (key, message).
func SignMessage(priv ed25519.PrivateKey, message []byte) []byte {
	sig := ed25519.Sign(priv, message)
	return SerializeTransaction(sig, message)
}

// TransferInstructionData is the SPL Token "transfer" instruction (index 3)
// payload: u8 instruction, u64 little-endian amount.
func TransferInstructionData(amount uint64) []byte {
	data := make([]byte, 9)
	data[0] = 3 // Transfer
	binary.LittleEndian.PutUint64(data[1:], amount)
	return data
}

// CreateATAInstructionData is the Associated Token Account "create" (index 0)
// payload — an empty byte slice. Kept here so the withdrawal worker does not
// repeat the literal.
var CreateATAInstructionData = []byte{0}
