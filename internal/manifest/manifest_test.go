package manifest

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	m, err := Parse([]byte(`{
		"cluster": "shop-prod",
		"registry": "123.dkr.ecr.us-east-1.amazonaws.com",
		"services": {"web": {"template": "arn:t/web:3"}, "api": {"template": "arn:t/api:7", "service": "shop-api"}},
		"tasks": {"console": {"template": "arn:t/console:1", "network": {"subnets": ["s-1"], "security_groups": ["sg-1"]}}},
		"hosts": {"api": "https://api.example.com/"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Names(); !reflect.DeepEqual(got, []string{"api", "web", "console"}) {
		t.Errorf("Names() = %v", got)
	}
	if m.ServiceName("api") != "shop-api" || m.ServiceName("web") != "web" {
		t.Error("ServiceName")
	}
	if h, ok := m.Host("hosts.api"); !ok || h != "https://api.example.com" {
		t.Errorf("Host = %q", h)
	}
	if _, isService, ok := m.Family("console"); !ok || isService {
		t.Error("Family(console)")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":     `{"cluster": "c", "services": {"a": {"template": "t"}}, "sevices": {}}`,
		"no cluster":        `{"services": {"a": {"template": "t"}}}`,
		"nothing to deploy": `{"cluster": "c"}`,
		"no template":       `{"cluster": "c", "services": {"a": {}}}`,
		"duplicate name":    `{"cluster": "c", "services": {"a": {"template": "t"}}, "tasks": {"a": {"template": "t"}}}`,
		"service network":   `{"cluster": "c", "services": {"a": {"template": "t", "network": {"subnets": ["s"]}}}}`,
		"no subnets":        `{"cluster": "c", "services": {"a": {"template": "t"}}, "network": {"subnets": []}}`,
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
