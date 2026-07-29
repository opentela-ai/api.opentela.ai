package identity

import "testing"

func TestNormalizeRuleDomain(t *testing.T) {
	got, err := NormalizeRuleDomain("Example.COM")
	if err != nil {
		t.Fatalf("NormalizeRuleDomain: %v", err)
	}
	if got != "example.com" {
		t.Fatalf("NormalizeRuleDomain = %q, want example.com", got)
	}
}

func TestNormalizeRuleDomainRejectsInvalid(t *testing.T) {
	cases := []string{
		"",
		".example.com",
		"example.com.",
		"exa..mple.com",
		"exa mple.com",
		"münich.example",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			if _, err := NormalizeRuleDomain(tc); err == nil {
				t.Fatalf("NormalizeRuleDomain(%q) unexpectedly succeeded", tc)
			}
		})
	}
}

func TestDomainFromVerifiedEmail(t *testing.T) {
	got, ok := DomainFromVerifiedEmail("User@Sub.Example.COM")
	if !ok {
		t.Fatal("DomainFromVerifiedEmail returned ok=false")
	}
	if got != "sub.example.com" {
		t.Fatalf("DomainFromVerifiedEmail = %q, want sub.example.com", got)
	}
}
