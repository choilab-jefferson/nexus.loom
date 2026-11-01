package trace

import (
	"strings"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

// Caps for the argv logged on run_command tool_call events.
const (
	maxArgvElems     = 64
	maxArgvElemBytes = 256
	maxArgvTotal     = 4096
	argvMarker       = "...[truncated]"
)

// RedactArgv extracts the argv of a run_command argument map and returns a
// redacted, size-capped copy for tracing. ok is false when args has no
// string-array argv. It never mutates args, so HashArgs is unaffected.
func RedactArgv(args map[string]any) ([]string, bool) {
	var in []string
	switch v := args["argv"].(type) {
	case []string:
		in = v
	case []any:
		for _, e := range v {
			s, isStr := e.(string)
			if !isStr {
				return nil, false
			}
			in = append(in, s)
		}
	default:
		return nil, false
	}

	out := make([]string, 0, len(in))
	total := 0
	maskNext := false
	for _, tok := range in {
		if len(out) >= maxArgvElems || total >= maxArgvTotal {
			return append(out, argvMarker), true
		}
		var s string
		switch {
		case maskNext:
			s, maskNext = "***", false
		default:
			s, maskNext = redactToken(tok)
		}
		if len(s) > maxArgvElemBytes {
			s = strings.ToValidUTF8(s[:maxArgvElemBytes], "") + argvMarker
		}
		out = append(out, s)
		total += len(s)
	}
	return out, true
}

func isSecretFlag(name string) bool {
	return policy.IsSecretEnvName(strings.ReplaceAll(strings.TrimLeft(name, "-"), "-", "_"))
}

// redactToken redacts a single token; maskNext reports that the following
// token is the value of a secret flag (`--token x`).
func redactToken(tok string) (string, bool) {
	if strings.HasPrefix(tok, "-") {
		name, _, hasVal := strings.Cut(tok, "=")
		if isSecretFlag(name) {
			if hasVal {
				return name + "=***", false
			}
			return tok, true
		}
		return tok, false
	}
	if k, _, ok := strings.Cut(tok, "="); ok && k != "" && policy.IsSecretEnvName(k) {
		return k + "=***", false
	}
	if end, ok := bearerEnd(tok); ok {
		return tok[:end] + "***", false
	}
	if strings.EqualFold(tok, "bearer") {
		return tok, true
	}
	return tok, false
}

// bearerEnd returns the index just past the first case-insensitive "Bearer "
// in tok when a non-blank credential follows it.
func bearerEnd(tok string) (int, bool) {
	const p = "bearer "
	for i := 0; i+len(p) <= len(tok); i++ {
		if strings.EqualFold(tok[i:i+len(p)], p) {
			if strings.TrimSpace(tok[i+len(p):]) == "" {
				return 0, false
			}
			return i + len(p), true
		}
	}
	return 0, false
}
