package config

import (
	"strings"
	"testing"
	"time"
)

const full = `
app: shop
images:
  "*": ${registry}/shop-${name}:${sha}
  worker: { image: "${registry}/shop-worker:${sha}", container: worker }
skip_unless_changed: ["**", "!infra/**", "!**/*.md"]
before_roll:
  - drain: { started_by: scheduler, timeout: 100m }
  - exec: ./scripts/check.sh
release: { task: api, command: [migrate], timeout: 15m }
health:
  - { url: "${hosts.api}/healthz", revision: .revision }
smoke:
  - { url: "${hosts.api}/v1/ping", method: POST, headers: { Authorization: "Bearer ${env.TOKEN}" }, body: "{}", expect: 401 }
on_failure: rollback
console: { task: console, command: [bash] }
verify_image: ./verify.sh "$ECSD_IMAGE"
timeouts: { roll: 25m }
`

func TestParseFull(t *testing.T) {
	f, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if f.Images["worker"].Container != "worker" || f.Images["*"].Ref != "${registry}/shop-${name}:${sha}" {
		t.Errorf("images: %+v", f.Images)
	}
	if f.BeforeRoll[0].Drain.Timeout.Duration != 100*time.Minute {
		t.Errorf("drain timeout: %v", f.BeforeRoll[0].Drain.Timeout)
	}
	if f.Timeouts.Roll.Or(time.Minute) != 25*time.Minute {
		t.Errorf("roll timeout: %v", f.Timeouts.Roll)
	}
	if img, ok := f.ImageFor("api"); !ok || img.Ref != f.Images["*"].Ref {
		t.Errorf("ImageFor(api) = %+v", img)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"no verify_image":   "app: a\nimages: {x: y}\n",
		"unknown key":       "app: a\nimages: {x: y}\nrelase: {task: api}\nverify_image: none\n",
		"unknown image key": "app: a\nimages: {x: {image: y, contaner: z}}\nverify_image: none\n",
		"bad app":           "app: Shop\nimages: {x: y}\nverify_image: none\n",
		"no images":         "app: a\n",
		"both gate kinds":   "app: a\nimages: {x: y}\nbefore_roll: [{drain: {started_by: s}, exec: e}]\nverify_image: none\n",
		"bad on_failure":    "app: a\nimages: {x: y}\non_failure: retry\nverify_image: none\n",
		"bad duration":      "app: a\nimages: {x: y}\nrelease: {task: api, timeout: soon}\nverify_image: none\n",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: parsed", name)
		} else if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("%s: empty error", name)
		}
	}
}
