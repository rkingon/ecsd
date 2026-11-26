package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"

	"github.com/rkingon/ecsd/internal/config"
	"github.com/rkingon/ecsd/internal/ecsx"
	"github.com/rkingon/ecsd/internal/manifest"
	"github.com/rkingon/ecsd/internal/probe"
	"github.com/rkingon/ecsd/internal/release"
	"github.com/rkingon/ecsd/internal/testutil"
	"github.com/rkingon/ecsd/internal/ui"
)

// Commits in history order; a later one includes every earlier one.
const (
	c1 = "1111111111111111111111111111111111111111"
	c2 = "2222222222222222222222222222222222222222"
	c3 = "3333333333333333333333333333333333333333"
)

type fakeGit struct {
	order   []string
	changed map[string][]string // "from..to" → files
}

func (g fakeGit) Has(sha string) bool { return slices.Contains(g.order, sha) }
func (g fakeGit) IsAncestor(a, b string) (bool, error) {
	return slices.Index(g.order, a) <= slices.Index(g.order, b), nil
}
func (g fakeGit) Changed(from, to string) ([]string, error) {
	if files, ok := g.changed[from+".."+to]; ok {
		return files, nil
	}
	return []string{"src/main.go"}, nil
}

type world struct {
	t     *testing.T
	ecs   *testutil.ECS
	ssm   *testutil.SSM
	env   *Env
	cfg   *config.File
	hooks []string
	sick  string // a commit whose health check reports another
	pins  fakePinner
}

// fakePinner leaves images as they are, or pins them to a made-up digest when digests is set.
type fakePinner struct{ digests bool }

func (f *fakePinner) Pin(_ context.Context, image string, _ bool) (string, error) {
	if !f.digests {
		return image, nil
	}
	repo, _, _ := strings.Cut(image, ":")
	return repo + "@sha256:" + strings.Repeat("d", 64), nil
}

const configYAML = `
app: shop
images:
  "*": ${registry}/shop-${name}:${sha}
release: { task: api, command: [migrate] }
health:
  - { url: "${hosts.api}/healthz", revision: .revision }
smoke:
  - { url: "${hosts.api}/v1/keys", method: POST, body: "{}", expect: 401, contains: unknown, attempts: 2, interval: 1ms }
verify_image: verify "$ECSD_IMAGE"
`

func newWorld(t *testing.T, yml string) *world {
	t.Helper()
	cfg, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, ecs: testutil.NewECS(), ssm: testutil.NewSSM(), cfg: cfg}
	api := w.ecs.AddTemplate("shop-api", "placeholder")
	web := w.ecs.AddTemplate("shop-web", "placeholder")
	worker := w.ecs.AddTemplate("shop-worker", "placeholder")
	w.ecs.AddService("api", api, 2)
	w.ecs.AddService("web", web, 2)

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			sha := w.ecs.Tags(w.ecs.Revision("api"))["ecsd:sha"]
			if sha == w.sick {
				sha = "deadbeefdeadbeef"
			}
			fmt.Fprintf(rw, `{"revision": %q}`, sha)
		case "/v1/keys":
			rw.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(rw, "unknown key")
		}
	}))
	t.Cleanup(srv.Close)

	m := &manifest.Manifest{
		Cluster: "shop-prod", Registry: "reg.example",
		Services: map[string]manifest.Family{"api": {Template: api}, "web": {Template: web}},
		Tasks:    map[string]manifest.Family{"worker": {Template: worker}},
		Hosts:    map[string]string{"api": srv.URL},
	}
	w.env = &Env{
		App: "shop", Stage: "prod", Config: cfg, Manifest: m,
		ECS:            &ecsx.Client{API: w.ecs, Cluster: m.Cluster, UI: ui.Discard(), Interval: time.Millisecond},
		Store:          &release.Store{API: w.ssm, Parameter: "/shop/prod/release"},
		Git:            fakeGit{order: []string{c1, c2, c3}},
		Probe:          &probe.Prober{HTTP: srv.Client(), Log: t.Logf},
		Images:         &w.pins,
		UI:             ui.Discard(),
		HealthInterval: time.Millisecond,
		Shell: func(_ context.Context, command string, env []string) error {
			w.hooks = append(w.hooks, command+" "+strings.Join(env, " "))
			if strings.Contains(command, "fail") {
				return errors.New("exit status 1")
			}
			return nil
		},
	}
	return w
}

func (w *world) deploy(sha string) (*Result, error) {
	return w.env.Deploy(context.Background(), Options{SHA: sha})
}

func (w *world) mustDeploy(sha string) *release.Release {
	w.t.Helper()
	res, err := w.deploy(sha)
	if err != nil {
		w.t.Fatalf("deploy %s: %v", sha[:7], err)
	}
	if res.Release == nil {
		w.t.Fatalf("deploy %s skipped: %s", sha[:7], res.Skipped)
	}
	return res.Release
}

func TestDeploy(t *testing.T) {
	w := newWorld(t, configYAML)
	rel := w.mustDeploy(c1)

	for _, svc := range []string{"api", "web"} {
		rev := w.ecs.Revision(svc)
		if rel.Revisions[svc] != rev {
			t.Errorf("%s runs %s, release says %s", svc, rev, rel.Revisions[svc])
		}
		if img := w.ecs.Image(rev); img != "reg.example/shop-"+svc+":"+c1 {
			t.Errorf("%s image = %s", svc, img)
		}
		tags := w.ecs.Tags(rev)
		if tags["ecsd:sha"] != c1 || tags["team"] != "shop" {
			t.Errorf("%s tags = %v", svc, tags)
		}
	}
	if img := w.ecs.Image(rel.Revisions["worker"]); img != "reg.example/shop-worker:"+c1 {
		t.Errorf("worker image = %s", img)
	}
	releases := w.ecs.TasksStartedBy(ReleaseStartedBy("shop"))
	if len(releases) != 1 || !slices.Equal(releases[0].Overrides.ContainerOverrides[0].Command, []string{"migrate"}) {
		t.Errorf("release tasks: %+v", releases)
	}
	if len(w.hooks) != 3 || !strings.Contains(w.hooks[0], "ECSD_IMAGE=reg.example/shop-api:"+c1) {
		t.Errorf("verify_image hooks: %v", w.hooks)
	}
	cur, _ := w.env.Store.Current(context.Background())
	if cur == nil || cur.SHA != c1 || cur.Kind != release.Deploy {
		t.Errorf("current release = %+v", cur)
	}
}

func TestDeploySkips(t *testing.T) {
	w := newWorld(t, configYAML+"skip_unless_changed: ['!docs/**']\n")
	w.mustDeploy(c2)
	w.env.Git = fakeGit{order: []string{c1, c2, c3}, changed: map[string][]string{c2 + ".." + c3: {"docs/a.md"}}}

	for sha, want := range map[string]string{c2: "already released", c1: "superseded", c3: "nothing skip_unless_changed covers"} {
		res, err := w.deploy(sha)
		if err != nil || !strings.Contains(res.Skipped, want) {
			t.Errorf("deploy %s = %+v, %v; want skipped %q", sha[:7], res, err, want)
		}
	}
	if n := w.ecs.Count("UpdateService"); n != 2 {
		t.Errorf("UpdateService calls = %d, want 2 (the first deploy's)", n)
	}
	res, err := w.env.Deploy(context.Background(), Options{SHA: c1, Force: true})
	if err != nil || res.Release == nil {
		t.Errorf("forced deploy = %+v, %v", res, err)
	}
}

func TestDeployRefusesUnknownReleasedCommit(t *testing.T) {
	w := newWorld(t, configYAML)
	w.mustDeploy(c2)
	w.env.Git = fakeGit{order: []string{c3}} // a shallow clone
	if _, err := w.deploy(c3); err == nil || !strings.Contains(err.Error(), "fetch-depth") {
		t.Errorf("err = %v", err)
	}
}

func TestRerunReusesRevisions(t *testing.T) {
	w := newWorld(t, configYAML)
	w.mustDeploy(c1)
	registered := w.ecs.Count("RegisterTaskDefinition")
	// As if the first run died after rolling, before recording.
	w.ssm = testutil.NewSSM()
	w.env.Store = &release.Store{API: w.ssm, Parameter: "/shop/prod/release"}
	w.mustDeploy(c1)
	if n := w.ecs.Count("RegisterTaskDefinition"); n != registered {
		t.Errorf("rerun registered %d more revisions", n-registered)
	}
	if n := w.ecs.Count("UpdateService"); n != 2 {
		t.Errorf("UpdateService calls = %d, want 2: a settled service isn't touched", n)
	}
}

func TestCircuitBreakerRollbackIsAFailure(t *testing.T) {
	w := newWorld(t, configYAML)
	w.mustDeploy(c1)
	w.ecs.AddService("web", w.ecs.Revision("web"), 2).Outcome = testutil.RollBack
	_, err := w.deploy(c2)
	if !errors.Is(err, ecsx.ErrRolledBack) {
		t.Fatalf("err = %v, want ErrRolledBack", err)
	}
	cur, _ := w.env.Store.Current(context.Background())
	if cur.SHA != c1 {
		t.Errorf("a failed deploy was recorded: current is %s", cur.SHA[:7])
	}
}

func TestFailedDeployRollsBack(t *testing.T) {
	w := newWorld(t, configYAML+"on_failure: rollback\n")
	first := w.mustDeploy(c1)
	w.sick = c2
	_, err := w.deploy(c2)
	if err == nil || !strings.Contains(err.Error(), "rolled back to 111111111111") {
		t.Fatalf("err = %v", err)
	}
	for _, svc := range []string{"api", "web"} {
		if got := w.ecs.Revision(svc); got != first.Revisions[svc] {
			t.Errorf("%s on %s, want %s", svc, got, first.Revisions[svc])
		}
	}
}

func TestFailedReleaseTaskStopsBeforeRolling(t *testing.T) {
	w := newWorld(t, configYAML)
	w.ecs.ExitCodes["shop-api"] = 1
	if _, err := w.deploy(c1); err == nil || !strings.Contains(err.Error(), "exited 1") {
		t.Fatalf("err = %v", err)
	}
	if n := w.ecs.Count("UpdateService"); n != 0 {
		t.Errorf("rolled %d services after a failed release task", n)
	}
}

func TestReleaseWaitsForAnotherDeploys(t *testing.T) {
	w := newWorld(t, configYAML)
	other := w.ecs.StartTask("shop-api", ReleaseStartedBy("shop"), 3)
	w.mustDeploy(c1)
	if s := aws.ToString(w.ecs.Task(other).StoppedReason); strings.Contains(s, "ecsd") {
		t.Errorf("stopped the other deploy's release task: %s", s)
	}
}

func TestReleaseLosesRaceToOlderTask(t *testing.T) {
	w := newWorld(t, configYAML)
	w.env.Config.Release.Wait = config.Duration{Duration: time.Second}
	// Started before ours but invisible to the first wait, as a concurrent deploy's would be.
	other := w.ecs.StartTask("shop-api", "pending", 2)
	orig := w.env.ECS.API
	w.env.ECS.API = &hookECS{API: orig, onRun: func() {
		w.ecs.Retag(other, ReleaseStartedBy("shop"))
	}}
	w.mustDeploy(c1)
	tasks := w.ecs.TasksStartedBy(ReleaseStartedBy("shop"))
	stopped := 0
	for _, task := range tasks {
		if strings.Contains(aws.ToString(task.StoppedReason), "started first") {
			stopped++
		}
	}
	if len(tasks) != 3 || stopped != 1 {
		t.Errorf("release tasks = %d, stopped as the race's loser = %d; want 3 and 1", len(tasks), stopped)
	}
}

func TestInterruptedReleaseStopsItsTask(t *testing.T) {
	w := newWorld(t, configYAML)
	w.ecs.TaskPolls = 1000
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := w.env.Deploy(ctx, Options{SHA: c1}); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v", err)
	}
	tasks := w.ecs.TasksStartedBy(ReleaseStartedBy("shop"))
	if len(tasks) != 1 || aws.ToString(tasks[0].LastStatus) != "STOPPED" {
		t.Errorf("release task left running: %+v", tasks)
	}
}

func TestVerifyImageFailureStopsEverything(t *testing.T) {
	w := newWorld(t, strings.Replace(configYAML, `verify "$ECSD_IMAGE"`, `fail`, 1))
	if _, err := w.deploy(c1); err == nil || !strings.Contains(err.Error(), "verify_image") {
		t.Fatalf("err = %v", err)
	}
	if n := w.ecs.Count("RegisterTaskDefinition"); n != 0 {
		t.Errorf("registered %d revisions of an unverified image", n)
	}
}

func TestDrainGate(t *testing.T) {
	w := newWorld(t, configYAML+"before_roll: [{drain: {started_by: scheduler, timeout: 5s}}]\n")
	job := w.ecs.StartTask("shop-worker", "scheduler", 3)
	w.mustDeploy(c1)
	if aws.ToString(w.ecs.Task(job).LastStatus) != "STOPPED" {
		t.Error("rolled before the scheduler's job finished")
	}

	w = newWorld(t, configYAML+"before_roll: [{drain: {started_by: scheduler, timeout: 1ms}}]\n")
	w.ecs.StartTask("shop-worker", "scheduler", -1)
	if _, err := w.deploy(c1); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("err = %v", err)
	}
}

func TestDeploymentMissingFromUpdateAnswer(t *testing.T) {
	w := newWorld(t, configYAML)
	w.ecs.HideNewDeployment = true
	w.mustDeploy(c1)
}

func TestRollback(t *testing.T) {
	w := newWorld(t, configYAML)
	first := w.mustDeploy(c1)
	w.mustDeploy(c2)
	ctx := context.Background()

	rel, err := w.env.Rollback(ctx, RollbackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rel.SHA != c1 || rel.Kind != release.Rollback || rel.From != c2 {
		t.Errorf("rollback release = %+v", rel)
	}
	for _, svc := range []string{"api", "web"} {
		if got, want := w.ecs.Image(w.ecs.Revision(svc)), w.ecs.Image(first.Revisions[svc]); got != want {
			t.Errorf("%s runs %s, want %s", svc, got, want)
		}
	}
	if img := w.ecs.Image(rel.Revisions["worker"]); img != "reg.example/shop-worker:"+c1 {
		t.Errorf("worker's newest revision runs %s", img)
	}
	if n := len(w.ecs.TasksStartedBy(ReleaseStartedBy("shop"))); n != 2 {
		t.Errorf("rollback ran a release task: %d in all", n)
	}
	// c2 was rolled back from, and c1 is current: nowhere left to go.
	if _, err := w.env.Rollback(ctx, RollbackOptions{}); err == nil {
		t.Error("second rollback found a target")
	}
	if rel, err := w.env.Rollback(ctx, RollbackOptions{To: c2[:7]}); err != nil || rel.SHA != c2 {
		t.Errorf("rollback --to %s = %+v, %v", c2[:7], rel, err)
	}
}

type hookECS struct {
	ecsx.API
	onRun func()
}

func (h *hookECS) RunTask(ctx context.Context, in *ecs.RunTaskInput, opts ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	h.onRun()
	return h.API.RunTask(ctx, in, opts...)
}

func TestFailedReleaseTaskRestoresFamilies(t *testing.T) {
	w := newWorld(t, configYAML)
	w.mustDeploy(c1)
	w.ecs.ExitCodes["shop-api"] = 1
	if _, err := w.deploy(c2); err == nil {
		t.Fatal("deploy with a failing release task succeeded")
	}
	for _, family := range []string{"shop-api", "shop-web", "shop-worker"} {
		if got := w.ecs.LatestImage(family); !strings.HasSuffix(got, ":"+c1) {
			t.Errorf("%s's newest revision runs %s, not the released %s", family, got, c1[:7])
		}
	}
}

func TestTaskFamiliesWaitForChecks(t *testing.T) {
	w := newWorld(t, configYAML+"on_failure: rollback\n")
	w.mustDeploy(c1)
	w.sick = c2
	registered := w.ecs.Count("RegisterTaskDefinition")
	if _, err := w.deploy(c2); err == nil {
		t.Fatal("deploy with failing health checks succeeded")
	}
	for _, family := range []string{"shop-api", "shop-web", "shop-worker"} {
		if got := w.ecs.LatestImage(family); !strings.HasSuffix(got, ":"+c1) {
			t.Errorf("%s's newest revision runs %s after the rollback", family, got)
		}
	}
	// api and web for c2, then copies of c1's on top; worker never.
	if n := w.ecs.Count("RegisterTaskDefinition") - registered; n != 4 {
		t.Errorf("registered %d revisions, want 4", n)
	}
}

func TestRolledBackCommitIsRefused(t *testing.T) {
	w := newWorld(t, configYAML)
	w.mustDeploy(c1)
	w.mustDeploy(c2)
	if _, err := w.env.Rollback(context.Background(), RollbackOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.deploy(c2); err == nil || !strings.Contains(err.Error(), "rolled back from") {
		t.Errorf("redeploy of a rolled-back commit: %v", err)
	}
	if res, err := w.env.Deploy(context.Background(), Options{SHA: c2, Force: true}); err != nil || res.Release == nil {
		t.Errorf("forced redeploy = %+v, %v", res, err)
	}
	// Released again, so no longer refused: a rerun is just "already released".
	if res, err := w.deploy(c2); err != nil || !strings.Contains(res.Skipped, "already released") {
		t.Errorf("rerun after the forced redeploy = %+v, %v", res, err)
	}
}

func TestRollbackPutsReleasedRevisionsOnTop(t *testing.T) {
	w := newWorld(t, configYAML)
	w.mustDeploy(c1)
	w.mustDeploy(c2)
	if _, err := w.env.Rollback(context.Background(), RollbackOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"shop-api", "shop-web", "shop-worker"} {
		if got := w.ecs.LatestImage(family); !strings.HasSuffix(got, ":"+c1) {
			t.Errorf("%s's newest revision runs %s after rolling back to %s", family, got, c1[:7])
		}
	}
}

func TestImagePolicy(t *testing.T) {
	w := newWorld(t, strings.Replace(configYAML, "${registry}/shop-${name}", "elsewhere.example/shop-${name}", 1))
	if _, err := w.deploy(c1); err == nil || !strings.Contains(err.Error(), "isn't in the manifest's registry") {
		t.Errorf("foreign image: %v", err)
	}
	w = newWorld(t, strings.Replace(configYAML, "${registry}/shop-${name}", "elsewhere.example/shop-${name}", 1)+"image_policy: {any_registry: true}\n")
	w.mustDeploy(c1)

	w = newWorld(t, configYAML)
	w.env.Manifest.Registry = ""
	if _, err := w.env.Plan(context.Background(), Options{SHA: c1, Images: map[string]string{"api": "x/y:1", "web": "x/y:1", "worker": "x/y:1"}}); err == nil {
		t.Error("a manifest without a registry passed")
	}
}

func TestImagesArePinned(t *testing.T) {
	w := newWorld(t, configYAML)
	w.pins.digests = true
	rel := w.mustDeploy(c1)
	want := "reg.example/shop-api@sha256:" + strings.Repeat("d", 64)
	if rel.Images["api"] != want || w.ecs.Image(rel.Revisions["api"]) != want {
		t.Errorf("api: recorded %s, registered %s; want %s", rel.Images["api"], w.ecs.Image(rel.Revisions["api"]), want)
	}
	if !strings.Contains(w.hooks[0], "ECSD_IMAGE="+want) {
		t.Errorf("verify_image saw %s", w.hooks[0])
	}
}

func TestOtherAppsReleaseTasksDontBlock(t *testing.T) {
	w := newWorld(t, configYAML)
	w.env.Config.Release.Wait = config.Duration{Duration: 10 * time.Millisecond}
	w.ecs.StartTask("other-api", ReleaseStartedBy("other"), -1)
	w.mustDeploy(c1)
}

func TestStaleRerunOfRolledBackCommitSkips(t *testing.T) {
	w := newWorld(t, configYAML)
	w.mustDeploy(c1)
	w.mustDeploy(c2)
	if _, err := w.env.Rollback(context.Background(), RollbackOptions{}); err != nil {
		t.Fatal(err)
	}
	w.mustDeploy(c3)
	res, err := w.deploy(c2)
	if err != nil || !strings.Contains(res.Skipped, "superseded") {
		t.Errorf("stale rerun of the rolled-back %s = %+v, %v; want a quiet superseded skip", c2[:7], res, err)
	}
}

func TestTemplateChangeShipsWithoutNewCode(t *testing.T) {
	w := newWorld(t, configYAML)
	w.mustDeploy(c1)
	tmpl := w.ecs.AddTemplate("shop-api", "placeholder")
	w.env.Manifest.Services["api"] = manifest.Family{Template: tmpl}

	rel := w.mustDeploy(c1)
	if got := w.ecs.Tags(rel.Revisions["api"])["ecsd:template"]; got != tmpl {
		t.Errorf("api's revision was copied from %s, want the new template %s", got, tmpl)
	}
	if res, err := w.deploy(c1); err != nil || !strings.Contains(res.Skipped, "already released") {
		t.Errorf("rerun after shipping the template = %+v, %v", res, err)
	}

	// Copies keep the template they came from, so a rollback isn't mistaken for a template change.
	w.mustDeploy(c2)
	if _, err := w.env.Rollback(context.Background(), RollbackOptions{}); err != nil {
		t.Fatal(err)
	}
	if res, err := w.deploy(c1); err != nil || !strings.Contains(res.Skipped, "already released") {
		t.Errorf("deploy of the rolled-back-to commit = %+v, %v", res, err)
	}

	w = newWorld(t, configYAML+"skip_unless_changed: [\"src/**\"]\n")
	w.env.Git = fakeGit{order: []string{c1, c2, c3}, changed: map[string][]string{c1 + ".." + c2: {"README.md"}}}
	w.mustDeploy(c1)
	if res, err := w.deploy(c2); err != nil || res.Skipped == "" {
		t.Fatalf("unchanged deploy = %+v, %v", res, err)
	}
	w.env.Manifest.Services["api"] = manifest.Family{Template: w.ecs.AddTemplate("shop-api", "placeholder")}
	w.mustDeploy(c2)
}

type updateHook struct {
	ecsx.API
	once   sync.Once
	before func()
}

func (h *updateHook) UpdateService(ctx context.Context, in *ecs.UpdateServiceInput, opts ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error) {
	h.once.Do(h.before)
	return h.API.UpdateService(ctx, in, opts...)
}

func TestNewerReleaseWhileWaitingStopsTheRoll(t *testing.T) {
	w := newWorld(t, configYAML)
	first := w.mustDeploy(c1)
	w.env.ECS.API = &hookECS{API: w.ecs, onRun: func() {
		_ = w.env.Store.Record(context.Background(), &release.Release{SHA: c3, Kind: release.Deploy, Revisions: first.Revisions})
	}}
	if _, err := w.deploy(c2); err == nil || !strings.Contains(err.Error(), "while this deploy waited") {
		t.Fatalf("deploy overtaken before its roll: %v", err)
	}
	if got := w.ecs.Image(w.ecs.Revision("api")); strings.HasSuffix(got, c2) {
		t.Errorf("api rolled to %s over a newer release", got)
	}
}

func TestNewerReleaseDuringRollIsRestored(t *testing.T) {
	w := newWorld(t, configYAML)
	first := w.mustDeploy(c1)
	w.env.ECS.API = &updateHook{API: w.ecs, before: func() {
		_ = w.env.Store.Record(context.Background(), &release.Release{SHA: c3, Kind: release.Deploy, Revisions: first.Revisions})
	}}
	if _, err := w.deploy(c2); err == nil || !strings.Contains(err.Error(), "while this deploy rolled out") {
		t.Fatalf("deploy overtaken during its roll: %v", err)
	}
	if cur, _ := w.env.Store.Current(context.Background()); cur.SHA != c3 {
		t.Errorf("current release = %s, want the newer %s", cur.SHA[:7], c3[:7])
	}
	for _, svc := range []string{"api", "web"} {
		if got := w.ecs.Revision(svc); got != first.Revisions[svc] {
			t.Errorf("%s runs %s, want the newer release's %s", svc, got, first.Revisions[svc])
		}
	}
}

func TestOlderReleaseWhileWaitingIsFine(t *testing.T) {
	w := newWorld(t, configYAML)
	w.env.ECS.API = &hookECS{API: w.ecs, onRun: func() {
		_ = w.env.Store.Record(context.Background(), &release.Release{SHA: c1, Kind: release.Deploy})
	}}
	w.mustDeploy(c2)
}

func TestRollbackImagesOnly(t *testing.T) {
	w := newWorld(t, configYAML)
	w.mustDeploy(c1)
	tmpl := w.ecs.AddTemplate("shop-api", "placeholder")
	w.env.Manifest.Services["api"] = manifest.Family{Template: tmpl}
	w.mustDeploy(c2)
	rel, err := w.env.Rollback(context.Background(), RollbackOptions{ImagesOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	rev := w.ecs.Revision("api")
	if img, from := w.ecs.Image(rev), w.ecs.Tags(rev)["ecsd:template"]; img != "reg.example/shop-api:"+c1 || from != tmpl || rel.Revisions["api"] != rev {
		t.Errorf("api runs %s from %s, want c1's image in the current template %s", img, from, tmpl)
	}
}
