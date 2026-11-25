// Package expand substitutes ${name} references. ${ is used rather than { so JSON request bodies need no escaping.
package expand

import (
	"fmt"
	"strings"
)

// Lookup resolves one reference, such as "sha" or "hosts.api".
type Lookup func(key string) (string, bool)

// String replaces every ${key} in s. An unknown key or an unterminated reference is an error, never left as is.
func String(s string, lookup Lookup) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		b.WriteString(s[:i])
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return "", fmt.Errorf("unterminated ${ in %q", s)
		}
		key := s[i+2 : i+end]
		v, ok := lookup(key)
		if !ok {
			return "", fmt.Errorf("unknown variable ${%s}", key)
		}
		b.WriteString(v)
		s = s[i+end+1:]
	}
}

// Map is a Lookup over fixed values, falling back to next when set.
func Map(values map[string]string, next Lookup) Lookup {
	return func(key string) (string, bool) {
		if v, ok := values[key]; ok {
			return v, true
		}
		if next != nil {
			return next(key)
		}
		return "", false
	}
}
