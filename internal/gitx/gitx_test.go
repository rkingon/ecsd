package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMatches(t *testing.T) {
	cases := []struct {
		path     string
		patterns []string
		want     bool
	}{
		{"src/api/main.go", []string{"!infra/**", "!**/*.md"}, true},
		{"infra/stack/main.tf", []string{"!infra/**", "!**/*.md"}, false},
		{"docs/README.md", []string{"!infra/**", "!**/*.md"}, false},
		{"src/api/main.go", []string{"src/**", "!src/**/*_test.go"}, true},
		{"src/api/main_test.go", []string{"src/**", "!src/**/*_test.go"}, false},
		{"Dockerfile", []string{"src/**"}, false},
		{"anything", nil, true},
	}
	for _, c := range cases {
		if got := Matches(c.path, c.patterns); got != c.want {
			t.Errorf("Matches(%q, %v) = %v", c.path, c.patterns, got)
		}
	}
}

func TestRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name string) {
		t.Helper()
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("src/a.go")
	run("add", ".")
	run("commit", "-qm", "one")
	r := Repo{Dir: dir}
	one, err := r.Resolve("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	write("infra/main.tf")
	run("add", ".")
	run("commit", "-qm", "two")
	two, _ := r.Resolve("HEAD")

	if ok, err := r.IsAncestor(one, two); err != nil || !ok {
		t.Errorf("IsAncestor(one, two) = %v, %v", ok, err)
	}
	if ok, err := r.IsAncestor(two, one); err != nil || ok {
		t.Errorf("IsAncestor(two, one) = %v, %v", ok, err)
	}
	changed, err := r.Changed(one, two)
	if err != nil || len(changed) != 1 || changed[0] != "infra/main.tf" {
		t.Errorf("Changed = %v, %v", changed, err)
	}
	if !r.Has(one) || r.Has("0000000000000000000000000000000000000000") {
		t.Error("Has")
	}
}
