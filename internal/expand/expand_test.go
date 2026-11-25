package expand

import "testing"

func TestString(t *testing.T) {
	lookup := Map(map[string]string{"sha": "abc", "hosts.api": "https://api.example.com"}, nil)
	cases := []struct {
		in, want string
		err      bool
	}{
		{"${hosts.api}/healthz", "https://api.example.com/healthz", false},
		{"img:${sha}", "img:abc", false},
		{`{"a": {}}`, `{"a": {}}`, false},
		{"${nope}", "", true},
		{"${sha", "", true},
	}
	for _, c := range cases {
		got, err := String(c.in, lookup)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("String(%q) = %q, %v", c.in, got, err)
		}
	}
}
