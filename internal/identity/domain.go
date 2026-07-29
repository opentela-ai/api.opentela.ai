package identity

import (
	"fmt"
	"strings"
)

const maxDomainLen = 253

// NormalizeRuleDomain validates an ASCII DNS domain for ACL rules and returns
// its lower-cased canonical form.
func NormalizeRuleDomain(v string) (string, error) {
	domain := strings.ToLower(strings.TrimSpace(v))
	if domain == "" {
		return "", fmt.Errorf("domain is required")
	}
	if len(domain) > maxDomainLen {
		return "", fmt.Errorf("domain too long")
	}
	if strings.ContainsAny(domain, " \t\r\n") {
		return "", fmt.Errorf("domain contains whitespace")
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", fmt.Errorf("domain cannot start or end with dot")
	}
	if strings.Contains(domain, "..") {
		return "", fmt.Errorf("domain cannot contain consecutive dots")
	}
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if label == "" {
			return "", fmt.Errorf("domain contains empty label")
		}
		if len(label) > 63 {
			return "", fmt.Errorf("domain label too long")
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' {
				continue
			}
			return "", fmt.Errorf("domain must be ASCII only")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("domain labels cannot start or end with hyphen")
		}
	}
	return domain, nil
}

// DomainFromVerifiedEmail extracts the exact lower-cased ASCII domain after the
// final @. It returns false when the address cannot contribute to ACL matches.
func DomainFromVerifiedEmail(email string) (string, bool) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", false
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return "", false
	}
	domain, err := NormalizeRuleDomain(email[at+1:])
	if err != nil {
		return "", false
	}
	return domain, true
}
