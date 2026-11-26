package schema

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var update = flag.Bool("update", false, "rewrite schema/v1 from the types")

// The published schemas are generated: go test ./internal/schema -update rewrites them.
func TestPublishedSchemasAreCurrent(t *testing.T) {
	for _, name := range Names {
		want, err := Generate(name)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("..", "..", "schema", "v1", name+".json")
		if *update {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is stale: go test ./internal/schema -update", path)
		}
	}
}

// The schemas must accept what the parsers accept, and reject a typo the parsers reject.
func TestSchemasAcceptValidFiles(t *testing.T) {
	cases := []struct {
		schema, doc string
		valid       bool
	}{
		{"config", `{"app": "shop", "images": {"*": "${registry}/shop-${name}:${sha}", "w": {"image": "x", "container": "w"}},
			"before_roll": [{"drain": {"started_by": "s", "timeout": "100m"}}, {"exec": "./x"}],
			"release": {"task": "api", "command": ["migrate"], "timeout": "15m"},
			"health": [{"url": "${hosts.api}/healthz", "revision": ".revision"}],
			"smoke": [{"url": "u", "method": "POST", "headers": {"A": "b"}, "body": "{}", "expect": 401, "interval": "1s"}],
			"on_failure": "rollback", "console": {"task": "c"}, "timeouts": {"roll": "25m"}, "verify_image": "none",
			"image_policy": {"any_registry": true}}`, true},
		{"config", `{"app": "shop", "images": {"*": "x"}, "relase": {"task": "api"}}`, false},
		{"config", `{"app": "shop", "images": {"*": "x"}, "release": {"task": "api", "timeout": "soon"}}`, false},
		{"manifest", `{"cluster": "c", "services": {"api": {"template": "arn"}}, "tasks": {"console": {"template": "arn",
			"network": {"subnets": ["s"], "security_groups": ["g"]}}}, "hosts": {"api": "https://x"}}`, true},
		{"manifest", `{"cluster": "c", "services": {"api": {"template": "arn", "templat": "x"}}}`, false},
	}
	for i, c := range cases {
		b, err := Generate(c.schema)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		comp := jsonschema.NewCompiler()
		if err := comp.AddResource(c.schema+".json", doc); err != nil {
			t.Fatal(err)
		}
		sch, err := comp.Compile(c.schema + ".json")
		if err != nil {
			t.Fatalf("%s: %v", c.schema, err)
		}
		inst, err := jsonschema.UnmarshalJSON(strings.NewReader(c.doc))
		if err != nil {
			t.Fatal(err)
		}
		if err := sch.Validate(inst); (err == nil) != c.valid {
			t.Errorf("case %d: valid = %v, want %v: %v", i, err == nil, c.valid, err)
		}
	}
}
