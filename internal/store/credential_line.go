package store

import "strings"

// CredentialLine is one imported text row.
type CredentialLine struct {
	Email    string
	Password string
	ApiKey   string
	Raw      string
}

// ParseCredentialLine accepts:
//
//	email----password----apikey
//	email----password            (apikey empty → Status pending_key)
//
// Lines starting with '#' and blank lines are skipped by the caller.
func ParseCredentialLine(raw string) (CredentialLine, bool) {
	raw = strings.TrimSpace(raw)
	out := CredentialLine{Raw: raw}
	if raw == "" || strings.HasPrefix(raw, "#") {
		return out, false
	}
	parts := strings.Split(raw, "----")
	switch {
	case len(parts) >= 3:
		out.Email = strings.TrimSpace(parts[0])
		out.Password = strings.TrimSpace(parts[1])
		out.ApiKey = strings.TrimSpace(parts[2])
		if out.Email == "" || !strings.Contains(out.Email, "@") {
			return out, false
		}
		return out, true
	case len(parts) == 2:
		out.Email = strings.TrimSpace(parts[0])
		out.Password = strings.TrimSpace(parts[1])
		if out.Email == "" || !strings.Contains(out.Email, "@") {
			return out, false
		}
		return out, true
	default:
		return out, false
	}
}
