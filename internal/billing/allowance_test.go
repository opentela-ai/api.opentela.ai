package billing

import (
	"testing"
	"time"
)

func allow(accountID, delegate string, raw int64, revoked *time.Time) Allowance {
	return Allowance{AccountID: accountID, Delegate: delegate, AllowanceRaw: raw, ApprovedAt: time.Now(), RevokedAt: revoked}
}

func TestDelegationAvailable(t *testing.T) {
	tests := []struct {
		name       string
		credit     AccountCredit
		allowances []Allowance
		want       int64
	}{
		{
			name:   "no allowances: deposit rail only",
			credit: AccountCredit{CreditRaw: 100, ReservedRaw: 30},
			want:   70,
		},
		{
			name:       "delegation below credit: allowance binds",
			credit:     AccountCredit{CreditRaw: 500},
			allowances: []Allowance{allow("a", "d1", 120, nil)},
			want:       120,
		},
		{
			name:       "credit below delegation: credit binds",
			credit:     AccountCredit{CreditRaw: 80, ReservedRaw: 10},
			allowances: []Allowance{allow("a", "d1", 120, nil)},
			want:       70,
		},
		{
			name:       "multiple delegates sum",
			credit:     AccountCredit{CreditRaw: 500},
			allowances: []Allowance{allow("a", "d1", 120, nil), allow("a", "d2", 80, nil)},
			want:       200,
		},
		{
			name:       "revoked allowances excluded",
			credit:     AccountCredit{CreditRaw: 500},
			allowances: []Allowance{allow("a", "d1", 120, nil), allow("a", "d2", 80, ptrTime(time.Now()))},
			want:       120,
		},
		{
			name:       "all revoked: deposit rail again (credit was clamped at revoke)",
			credit:     AccountCredit{CreditRaw: 30, ReservedRaw: 30},
			allowances: []Allowance{allow("a", "d1", 0, ptrTime(time.Now()))},
			want:       0,
		},
		{
			name:       "zero credit binds even with allowance (credit is the projection)",
			credit:     AccountCredit{},
			allowances: []Allowance{allow("a", "d1", 100, nil)},
			want:       0,
		},
		{
			name:       "negative available: zero",
			credit:     AccountCredit{CreditRaw: 10, ReservedRaw: 20},
			allowances: []Allowance{allow("a", "d1", 100, nil)},
			want:       0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DelegationAvailable(tt.credit, tt.allowances); got != tt.want {
				t.Fatalf("DelegationAvailable = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAllowanceActive(t *testing.T) {
	if allow("a", "d", 100, nil).Active() != true {
		t.Fatal("positive allowance must be active")
	}
	if allow("a", "d", 0, nil).Active() != false {
		t.Fatal("zero allowance must not be active")
	}
	if allow("a", "d", 100, ptrTime(time.Now())).Active() != false {
		t.Fatal("revoked allowance must not be active")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
