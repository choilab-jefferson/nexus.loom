package policy

import "strings"

// IsSecretEnvName reports whether an environment variable name looks like it
// carries a secret (HF_TOKEN, *_API_KEY, *TOKEN*, *SECRET*, *PASSWORD*, ...).
// It is the single matcher shared by the subprocess env allowlist and any
// argv/log redaction, so both agree on what a secret is.
func IsSecretEnvName(name string) bool {
	n := strings.ToUpper(strings.TrimSpace(name))
	for _, frag := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "PRIVATE_KEY"} {
		if strings.Contains(n, frag) {
			return true
		}
	}
	for _, suf := range []string{"_API_KEY", "APIKEY", "_ACCESS_KEY", "_AUTH_KEY"} {
		if strings.HasSuffix(n, suf) {
			return true
		}
	}
	return n == "API_KEY"
}
