package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/rkingon/ecsd/internal/deploy"
	"github.com/rkingon/ecsd/internal/testutil"
	"github.com/rkingon/ecsd/internal/ui"
)

// stage is a repository with ecsd.yml and two commits, and fake AWS behind it.
type stage struct {
	t       *testing.T
	dir     string
	commits []string
	ecs     *testutil.ECS
	ssm     *testutil.SSM
	logs    *testutil.Logs
}

func newStage(t *testing.T) *stage {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	s := &stage{t: t, dir: t.TempDir(), ecs: testutil.NewECS(), ssm: testutil.NewSSM(), logs: &testutil.Logs{Lines: []string{"migrated 3"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"revision": %q}`, s.ecs.Tags(s.ecs.Revision("web"))["ecsd:sha"])
	}))
	t.Cleanup(srv.Close)

	s.write("ecsd.yml", `
app: shop
images:
  "*": ${registry}/shop-${name}:${sha}
release: { task: web, command: [migrate] }
health:
  - { url: "${hosts.web}/healthz", revision: .revision }
console: { task: console }
verify_image: none
`)
	s.git("init", "-q", "-b", "main")
	s.commit("one")
	s.write("src/main.go", "package main")
	s.commit("two")

	web := s.ecs.AddTemplate("shop-web", "placeholder")
	console := s.ecs.AddTemplate("shop-console", "placeholder")
	s.ecs.AddService("web", web, 2)
	s.ssm.Set("/shop/prod/deploy", fmt.Sprintf(`{
		"cluster": "shop-prod", "registry": "reg.example",
		"services": {"web": {"template": %q}},
		"tasks": {"console": {"template": %q}},
		"hosts": {"web": %q}
	}`, web, console, srv.URL))
	return s
}

func (s *stage) write(name, body string) {
	p := filepath.Join(s.dir, name)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *stage) git(args ...string) string {
	s.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (s *stage) commit(msg string) {
	s.git("add", ".")
	s.git("commit", "-qm", msg)
	s.commits = append(s.commits, s.git("rev-parse", "HEAD"))
}

// run runs ecsd with args and returns what it printed to stdout.
func (s *stage) run(args ...string) (string, error) {
	s.t.Helper()
	a := &app{
		ecs: s.ecs, ssm: s.ssm, logs: s.logs, aws: aws.Config{Region: "us-east-1"}, ui: ui.Discard(), interval: time.Millisecond,
		pinner: samePinner{},
	}
	cmd := rootWith(a)
	cmd.SetArgs(append([]string{"--stage", "prod", "--repo", s.dir, "--config", filepath.Join(s.dir, "ecsd.yml")}, args...))
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	err := cmd.ExecuteContext(context.Background())
	w.Close()
	os.Stdout = stdout
	return <-done, err
}

func (s *stage) must(args ...string) string {
	s.t.Helper()
	out, err := s.run(args...)
	if err != nil {
		s.t.Fatalf("ecsd %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func TestCLI(t *testing.T) {
	s := newStage(t)
	one, two := s.commits[0], s.commits[1]
	t.Setenv("GITHUB_SHA", "")

	s.must("check")
	s.must("deploy", "--sha", one)
	if out := s.must("plan"); !strings.Contains(out, "reg.example/shop-web:"+two) || !strings.Contains(out, "release task: web [migrate]") {
		t.Errorf("plan:\n%s", out)
	}
	s.must("deploy") // HEAD
	if out := s.must("superseded", "--sha", one); !strings.Contains(out, "superseded") {
		t.Errorf("superseded: %q", out)
	}
	if out := s.must("history"); strings.Count(out, "\nv") != 2 || !strings.Contains(out, two[:12]) {
		t.Errorf("history:\n%s", out)
	}
	s.must("rollback")
	if out := s.must("status"); !strings.Contains(out, "rollback") || !strings.Contains(out, one) {
		t.Errorf("status:\n%s", out)
	}
	if got := s.ecs.Image(s.ecs.Revision("web")); got != "reg.example/shop-web:"+one {
		t.Errorf("after rollback web runs %s", got)
	}

	if out := s.must("run", "web", "--", "rake", "db:seed"); !strings.Contains(out, "migrated 3") {
		t.Errorf("run printed %q", out)
	}
	s.ecs.ExitCodes["shop-web"] = 3
	var exit exitError
	if _, err := s.run("run", "web", "--", "false"); !errors.As(err, &exit) || exit.code != 3 {
		t.Errorf("run's exit = %v, want 3", err)
	}

	marker := deploy.ReleaseStartedBy("shop")
	newest := func() []string {
		tasks := s.ecs.TasksStartedBy(marker)
		slices.SortFunc(tasks, func(a, b types.Task) int { return a.CreatedAt.Compare(*b.CreatedAt) })
		return tasks[len(tasks)-1].Overrides.ContainerOverrides[0].Command
	}
	delete(s.ecs.ExitCodes, "shop-web")
	s.must("run", "--release")
	if got := newest(); !slices.Equal(got, []string{"migrate"}) {
		t.Errorf("run --release ran %v under %s", got, marker)
	}
	s.ecs.ExitCodes["shop-web"] = 4
	if _, err := s.run("run", "--release", "--", "migrate", "--down"); !errors.As(err, &exit) || exit.code != 4 || !slices.Equal(newest(), []string{"migrate", "--down"}) {
		t.Errorf("run --release -- migrate --down: exit %v, ran %v", err, newest())
	}
	if _, err := s.run("run", "--release", "web"); err == nil {
		t.Error("run --release took a name")
	}

	if out := s.must("logs", "web"); !strings.Contains(out, "app/01234567 migrated 3") {
		t.Errorf("logs:\n%s", out)
	}
	if out := s.must("manifest", "init", "--cluster", "shop-prod"); !strings.Contains(out, `"web"`) {
		t.Errorf("manifest init:\n%s", out)
	}
}

func TestCLIRejectsUnknownNames(t *testing.T) {
	s := newStage(t)
	s.write("ecsd.yml", "app: shop\nimages: {\"*\": x, api: y}\nverify_image: none\n")
	if _, err := s.run("plan"); err == nil || !strings.Contains(err.Error(), "images.api") {
		t.Errorf("err = %v", err)
	}
	if _, err := s.run("--stage", "Prod", "plan"); err == nil {
		t.Error("accepted stage Prod")
	}
}

func TestShellJoin(t *testing.T) {
	if got := shellJoin([]string{"rails", "runner", "puts 'hi'", ""}); got != `rails runner 'puts '\''hi'\''' ''` {
		t.Errorf("shellJoin = %s", got)
	}
}

type samePinner struct{}

func (samePinner) Pin(_ context.Context, image string, _ bool) (string, error) { return image, nil }
