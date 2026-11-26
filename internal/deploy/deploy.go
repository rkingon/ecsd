// Package deploy is the pipeline: decide whether there's anything to deploy, register, gate, run the release task,
// roll every service, check, record. And the way back.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"golang.org/x/sync/errgroup"

	"github.com/rkingon/ecsd/internal/config"
	"github.com/rkingon/ecsd/internal/ecsx"
	"github.com/rkingon/ecsd/internal/expand"
	"github.com/rkingon/ecsd/internal/gitx"
	"github.com/rkingon/ecsd/internal/images"
	"github.com/rkingon/ecsd/internal/logs"
	"github.com/rkingon/ecsd/internal/manifest"
	"github.com/rkingon/ecsd/internal/probe"
	"github.com/rkingon/ecsd/internal/release"
	"github.com/rkingon/ecsd/internal/ui"
)

// ReleaseStartedBy marks an app's release tasks, so a deploy finds another deploy's of the same app, and only those:
// apps sharing a cluster don't queue behind each other's migrations.
func ReleaseStartedBy(app string) string {
	s := "ecsd-release-" + app
	return s[:min(len(s), 128)]
}

type Git interface {
	Has(sha string) bool
	IsAncestor(a, b string) (bool, error)
	Changed(from, to string) ([]string, error)
}

type Env struct {
	App, Stage string
	Config     *config.File
	Manifest   *manifest.Manifest
	ECS        *ecsx.Client
	Store      *release.Store
	Logs       logs.API // optional: a release task's output
	Git        Git      // nil outside a repository
	Probe      *probe.Prober
	Images     Pinner
	UI         *ui.UI
	Actor      string

	// Shell runs hooks (verify_image, exec gates); defaults to sh -c.
	Shell func(ctx context.Context, command string, env []string) error
	// HealthInterval is between health check attempts; tests shorten it.
	HealthInterval time.Duration
}

// Pinner turns an image reference into one pinned to its digest.
type Pinner interface {
	Pin(ctx context.Context, image string, allowTags bool) (string, error)
}

type Options struct {
	SHA        string            // full commit hash
	Images     map[string]string // name → image, over ecsd.yml's
	Force      bool              // deploy even if superseded, unchanged, released or rolled back from
	NoRollback bool              // a failure leaves the services where they are
	SkipGates  bool
}

type Plan struct {
	SHA     string
	Current *release.Release
	Skip    string              // why there's nothing to do; empty to deploy
	Images  map[string]string   // name → image
	Holds   map[string][]string // name → the containers that get it, none for the first
}

// Plan decides what a deploy of opts.SHA would do, without changing anything.
func (e *Env) Plan(ctx context.Context, opts Options) (*Plan, error) {
	cur, err := e.Store.Current(ctx)
	if err != nil {
		return nil, err
	}
	p := &Plan{SHA: opts.SHA, Current: cur, Images: map[string]string{}, Holds: e.holds()}
	for _, name := range e.Manifest.Names() {
		img, err := e.image(name, opts)
		if err != nil {
			return nil, err
		}
		if err := e.inRegistry(name, img); err != nil {
			return nil, err
		}
		p.Images[name] = img
	}
	if !opts.Force {
		if p.Skip, err = e.skip(ctx, cur, opts.SHA); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (e *Env) holds() map[string][]string {
	h := map[string][]string{}
	for _, name := range e.Manifest.Names() {
		if i, ok := e.Config.ImageFor(name); ok {
			h[name] = i.Containers
		}
	}
	return h
}

// inRegistry enforces that images come from the manifest's registry, unless image_policy.any_registry.
func (e *Env) inRegistry(name, img string) error {
	if e.Config.ImagePolicy.AnyRegistry {
		return nil
	}
	if e.Manifest.Registry == "" {
		return errors.New("the manifest names no registry: set its registry, or image_policy.any_registry in ecsd.yml")
	}
	if !images.InRegistry(img, e.Manifest.Registry) {
		return fmt.Errorf("%s: image %s isn't in the manifest's registry %s (image_policy.any_registry allows it)", name, img, e.Manifest.Registry)
	}
	return nil
}

// Image is ecsd.yml's image for name at sha, before pinning.
func (e *Env) Image(name, sha string) (string, error) { return e.image(name, Options{SHA: sha}) }

func (e *Env) image(name string, opts Options) (string, error) {
	if img, ok := opts.Images[name]; ok {
		return img, nil
	}
	img, ok := e.Config.ImageFor(name)
	if !ok {
		return "", fmt.Errorf("images: nothing for %s, and no *", name)
	}
	vars := map[string]string{"name": name, "sha": opts.SHA, "app": e.App, "stage": e.Stage}
	if e.Manifest.Registry != "" {
		vars["registry"] = e.Manifest.Registry
	}
	ref, err := expand.String(img.Ref, expand.Map(vars, nil))
	if err != nil {
		return "", fmt.Errorf("images.%s: %w", name, err)
	}
	return ref, nil
}

func (e *Env) skip(ctx context.Context, cur *release.Release, sha string) (string, error) {
	if cur == nil {
		return "", nil
	}
	if cur.SHA == sha {
		// A rollback chose its release's config too: a rerun of its commit mustn't put the current templates back.
		if cur.Kind != release.Rollback {
			if changed, err := e.changedTemplates(ctx, cur); err != nil || len(changed) > 0 {
				return "", err
			}
		}
		return fmt.Sprintf("%s is already released (version %d)", short(sha), cur.Version), nil
	}
	if e.Git == nil {
		return "", errors.New("not in a git repository, so whether this commit is behind the released one can't be checked (--force deploys anyway)")
	}
	if !e.Git.Has(cur.SHA) {
		return "", fmt.Errorf("the released commit %s isn't in this clone: fetch its history (actions/checkout's fetch-depth: 0), or --force", short(cur.SHA))
	}
	behind, err := e.Git.IsAncestor(sha, cur.SHA)
	if err != nil {
		return "", err
	}
	if behind {
		return fmt.Sprintf("superseded: the released %s already includes %s", short(cur.SHA), short(sha)), nil
	}
	// After the superseded check: a stale rerun of a rolled-back commit, once a newer one is released, skips quietly.
	history, err := e.Store.History(ctx, 0)
	if err != nil {
		return "", err
	}
	if rb, bad := release.RolledBackFrom(history, sha); bad {
		return "", fmt.Errorf("%s was rolled back from in release version %d (%s): --force deploys it again", short(sha), rb.Version, rb.By)
	}
	if len(e.Config.SkipUnlessChanged) > 0 {
		changed, err := e.Git.Changed(cur.SHA, sha)
		if err != nil {
			return "", err
		}
		if _, ok := gitx.AnyMatches(changed, e.Config.SkipUnlessChanged); !ok {
			if changed, err := e.changedTemplates(ctx, cur); err != nil || len(changed) > 0 {
				return "", err
			}
			return fmt.Sprintf("nothing skip_unless_changed covers changed since %s", short(cur.SHA)), nil
		}
	}
	return "", nil
}

// changedTemplates names the families whose manifest template isn't the one their released revision was copied from:
// an IaC change, which the next deploy ships even when the commit is released or unchanged.
func (e *Env) changedTemplates(ctx context.Context, cur *release.Release) ([]string, error) {
	var changed []string
	for _, name := range e.Manifest.Names() {
		fam, _, _ := e.Manifest.Family(name)
		if rev := cur.Revisions[name]; rev != "" {
			def, err := e.ECS.Describe(ctx, rev)
			if err != nil {
				return nil, err
			}
			if tagValue(def.Tags, "ecsd:template") == fam.Template {
				continue
			}
		}
		changed = append(changed, name)
	}
	if len(changed) > 0 {
		e.UI.Printf("templates changed since %s was released: %s", short(cur.SHA), strings.Join(changed, ", "))
	}
	return changed, nil
}

func tagValue(tags []types.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

type Result struct {
	Skipped string
	Release *release.Release
}

// Deploy runs the pipeline. A failure after the rollout starts returns the services to the current release when
// on_failure is rollback; the release task's changes (migrations) stay, as they must be backward compatible.
//
// The newest revision of every family is the current release's, outside a deploy: apps that start tasks by family name
// run released code. So task families are registered only once every check has passed, and a failed deploy puts a copy
// of the released revision back on top of every family.
func (e *Env) Deploy(ctx context.Context, opts Options) (*Result, error) {
	p, err := e.Plan(ctx, opts)
	if err != nil {
		return nil, err
	}
	e.UI.Output("sha", opts.SHA)
	if p.Skip != "" {
		e.UI.Step("nothing to deploy: %s", p.Skip)
		e.UI.Summary("Nothing to deploy to %s: %s", e.Stage, p.Skip)
		e.UI.Output("skipped", "true")
		return &Result{Skipped: p.Skip}, nil
	}
	e.UI.Output("skipped", "false")
	e.UI.Step("deploying %s to %s", short(opts.SHA), e.Stage)

	if err := e.pin(ctx, p.Images); err != nil {
		return nil, err
	}
	if err := e.verifyImages(ctx, p.Images, opts.SHA); err != nil {
		return nil, err
	}
	early := e.Manifest.ServiceNames()
	if r := e.Config.Release; r != nil && !slices.Contains(early, r.Task) {
		early = append(early, r.Task)
	}
	regs, err := e.register(ctx, early, p, opts.SHA, nil)
	if err != nil {
		return nil, e.failed(ctx, err, p.Current, regs)
	}
	if !opts.SkipGates {
		if err := e.gates(ctx, opts.SHA); err != nil {
			return nil, e.failed(ctx, err, p.Current, regs)
		}
	}
	if e.Config.Release != nil {
		if err := e.runRelease(ctx, regs, opts.SHA); err != nil {
			return nil, e.failed(ctx, err, p.Current, regs)
		}
	}

	if now, err := e.overtaken(ctx, opts, p.Current); err != nil {
		return nil, e.failed(ctx, err, p.Current, regs)
	} else if now != nil {
		return nil, e.failed(ctx, fmt.Errorf("%s was released while this deploy waited: not rolling %s over it", short(now.SHA), short(opts.SHA)), now, regs)
	}

	err = e.rollAndCheck(ctx, arns(regs), opts.SHA)
	if err == nil {
		var late map[string]ecsx.Registered
		late, err = e.register(ctx, without(e.Manifest.Names(), early), p, opts.SHA, nil)
		maps.Copy(regs, late)
	}
	if err != nil {
		// A cancelled deploy is left as it is: CI kills the job soon after cancelling it, mid-rollback.
		if e.Config.OnFailure == "rollback" && !opts.NoRollback && p.Current != nil && ctx.Err() == nil {
			e.UI.Errorf("deploy of %s failed: %v", short(opts.SHA), err)
			if rerr := e.restore(ctx, p.Current); rerr != nil {
				return nil, e.failed(ctx, fmt.Errorf("%w; and the rollback to %s failed too: %v", err, short(p.Current.SHA), rerr), p.Current, regs)
			}
			e.UI.Summary("Deploy of `%s` to %s failed and was rolled back to `%s`: %v", short(opts.SHA), e.Stage, short(p.Current.SHA), err)
			err = fmt.Errorf("%w (rolled back to %s)", err, short(p.Current.SHA))
		}
		return nil, e.failed(ctx, err, p.Current, regs)
	}

	if now, err := e.overtaken(ctx, opts, p.Current); err != nil {
		return nil, e.failed(ctx, err, p.Current, regs)
	} else if now != nil {
		err := fmt.Errorf("%s was released while this deploy rolled out", short(now.SHA))
		if rerr := e.restore(ctx, now); rerr != nil {
			err = fmt.Errorf("%w; and returning to it failed: %v", err, rerr)
		}
		return nil, e.failed(ctx, err, now, regs)
	}

	rel := &release.Release{
		SHA: opts.SHA, Kind: release.Deploy, Revisions: arns(regs), Images: p.Images, At: time.Now().UTC(), By: e.Actor,
	}
	if err := e.Store.Record(ctx, rel); err != nil {
		return nil, err
	}
	e.UI.Output("version", fmt.Sprint(rel.Version))
	e.summarize("Deployed", rel)
	return &Result{Release: rel}, nil
}

// overtaken returns the current release if one was recorded since was that this deploy mustn't go out over: a rollback,
// or a deploy of a commit opts.SHA doesn't include. Plan's supersede check alone can't stop either.
func (e *Env) overtaken(ctx context.Context, opts Options, was *release.Release) (*release.Release, error) {
	if opts.Force {
		return nil, nil
	}
	now, err := e.Store.Current(ctx)
	if err != nil || now == nil || (was != nil && now.Version == was.Version) || now.SHA == opts.SHA {
		return nil, err
	}
	// A rollback is someone saying "not that": it stops a deploy whatever the deploy's commit includes.
	if now.Kind == release.Rollback {
		return now, nil
	}
	if e.Git != nil && e.Git.Has(now.SHA) {
		if older, err := e.Git.IsAncestor(now.SHA, opts.SHA); err != nil || older {
			return nil, err
		}
	}
	return now, nil
}

func without(names, drop []string) []string {
	var out []string
	for _, n := range names {
		if !slices.Contains(drop, n) {
			out = append(out, n)
		}
	}
	return out
}

// failed puts a copy of the current release's revision back on top of every family, and returns err: those this deploy
// registered, and any a template was registered into since. A family already topped by the release's is left as it is.
// It outlives a cancelled deploy's context: it's a few quick calls, and skipping it leaves unreleased code where apps
// find it by family name.
func (e *Env) failed(ctx context.Context, err error, cur *release.Release, regs map[string]ecsx.Registered) error {
	if cur == nil {
		if len(regs) > 0 {
			names := slices.Sorted(maps.Keys(regs))
			e.UI.Warnf("nothing released yet, so the newest revisions of %s stay this failed deploy's", strings.Join(names, ", "))
		}
		return err
	}
	var restorable []string
	for _, name := range e.Manifest.Names() {
		if cur.Revisions[name] != "" {
			restorable = append(restorable, name)
		} else if _, ok := regs[name]; ok {
			e.UI.Warnf("%s: not in the current release, so its newest revision stays this failed deploy's", name)
		}
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if _, rerr := e.register(rctx, restorable, &Plan{}, cur.SHA, cur.Revisions); rerr != nil {
		return fmt.Errorf("%w; and putting the released revisions back on top failed: %v", err, rerr)
	}
	return err
}

// pin replaces every image with its digest-pinned reference.
func (e *Env) pin(ctx context.Context, imgs map[string]string) error {
	if e.Images == nil {
		return errors.New("no image resolver")
	}
	for _, name := range slices.Sorted(maps.Keys(imgs)) {
		pinned, err := e.Images.Pin(ctx, imgs[name], e.Config.ImagePolicy.Unpinned)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		imgs[name] = pinned
	}
	return nil
}

func arns(regs map[string]ecsx.Registered) map[string]string {
	m := map[string]string{}
	for name, r := range regs {
		m[name] = r.ARN
	}
	return m
}

// register copies each named family's template with its new image, or, with from set, the revision from names as it
// is.
func (e *Env) register(ctx context.Context, names []string, p *Plan, sha string, from map[string]string) (map[string]ecsx.Registered, error) {
	regs := map[string]ecsx.Registered{}
	if len(names) == 0 {
		return regs, nil
	}
	err := e.UI.Group("register", func() error {
		for _, name := range names {
			fam, _, _ := e.Manifest.Family(name)
			template, image := fam.Template, p.Images[name]
			tags := map[string]string{"ecsd:sha": sha, "ecsd:template": template}
			if from != nil {
				// A copy keeps its source's ecsd:template: the template it was made from.
				template, image = from[name], ""
				delete(tags, "ecsd:template")
			}
			r, err := e.ECS.Register(ctx, template, fam.Family, p.Holds[name], image, tags)
			if err != nil {
				return err
			}
			verb := "registered"
			if r.Reused {
				verb = "reusing"
			}
			e.UI.Printf("%s: %s %s", name, verb, ecsx.Short(r.ARN))
			regs[name] = r
		}
		return nil
	})
	return regs, err
}

func (e *Env) verifyImages(ctx context.Context, imgs map[string]string, sha string) error {
	if e.Config.VerifyImage == config.VerifyNone {
		e.UI.Warnf("verify_image is none: %s's images are deployed unverified", e.App)
		return nil
	}
	return e.UI.Group("verify images", func() error {
		for _, img := range distinct(imgs) {
			if err := e.shell(ctx, e.Config.VerifyImage, append(e.hookEnv(sha), "ECSD_IMAGE="+img)); err != nil {
				return fmt.Errorf("verify_image %s: %w", img, err)
			}
			e.UI.Printf("%s: verified", img)
		}
		return nil
	})
}

func distinct(imgs map[string]string) []string {
	var out []string
	for _, img := range imgs {
		if img != "" && !slices.Contains(out, img) {
			out = append(out, img)
		}
	}
	slices.Sort(out)
	return out
}

func (e *Env) hookEnv(sha string) []string {
	return []string{"ECSD_APP=" + e.App, "ECSD_STAGE=" + e.Stage, "ECSD_SHA=" + sha, "ECSD_CLUSTER=" + e.Manifest.Cluster}
}

func (e *Env) shell(ctx context.Context, command string, env []string) error {
	if e.Shell != nil {
		return e.Shell(ctx, command, env)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Env = append(os.Environ(), env...)
	return cmd.Run()
}

func (e *Env) gates(ctx context.Context, sha string) error {
	for _, g := range e.Config.BeforeRoll {
		switch {
		case g.Drain != nil:
			err := e.UI.Group("drain "+g.Drain.StartedBy, func() error {
				return e.ECS.WaitNone(ctx, g.Drain.StartedBy, g.Drain.Timeout.Or(30*time.Minute))
			})
			if err != nil {
				return fmt.Errorf("before_roll drain: %w", err)
			}
		case g.Exec != "":
			err := e.UI.Group(g.Exec, func() error { return e.shell(ctx, g.Exec, e.hookEnv(sha)) })
			if err != nil {
				return fmt.Errorf("before_roll exec %q: %w", g.Exec, err)
			}
		}
	}
	return nil
}

// network is a one-off task's: its own, the manifest's, or else the first service's.
func (e *Env) network(ctx context.Context, name string) (*manifest.Network, error) {
	if fam, isService, ok := e.Manifest.Family(name); ok && !isService && fam.Network != nil {
		return fam.Network, nil
	}
	if e.Manifest.Network != nil {
		return e.Manifest.Network, nil
	}
	services := e.Manifest.ServiceNames()
	if _, isService, _ := e.Manifest.Family(name); isService {
		services = append([]string{name}, services...)
	}
	for _, svc := range services {
		s, err := e.ECS.Service(ctx, e.Manifest.ServiceName(svc))
		if err != nil {
			return nil, err
		}
		if nc := s.NetworkConfiguration; nc != nil && nc.AwsvpcConfiguration != nil {
			c := nc.AwsvpcConfiguration
			return &manifest.Network{Subnets: c.Subnets, SecurityGroups: c.SecurityGroups, AssignPublicIP: c.AssignPublicIp == "ENABLED"}, nil
		}
	}
	return nil, errors.New("no network for one-off tasks: set the manifest's network")
}

func (e *Env) runRelease(ctx context.Context, regs map[string]ecsx.Registered, sha string) error {
	r := e.Config.Release
	reg, ok := regs[r.Task]
	if !ok {
		return fmt.Errorf("release.task: %s isn't in the manifest", r.Task)
	}
	container := r.Container
	if container == "" {
		container = reg.Container
	}
	return e.RunRelease(ctx, reg.ARN, container, r.Command, map[string]string{"ecsd:sha": sha})
}

// RunRelease runs command (nil: the container's own) from revision as the app's release task, one at a time per app:
// it waits for any other release task, and if one started alongside its own, the older one runs and this one stops
// and waits again. An interrupted run stops its task. A non-zero exit is a *TaskFailed.
func (e *Env) RunRelease(ctx context.Context, revision, container string, command []string, tags map[string]string) error {
	r := e.Config.Release
	net, err := e.network(ctx, r.Task)
	if err != nil {
		return err
	}
	return e.UI.Group("release: "+r.Task+" "+strings.Join(command, " "), func() error {
		for attempt := 0; ; attempt++ {
			if err := e.ECS.WaitNone(ctx, ReleaseStartedBy(e.App), r.Wait.Or(30*time.Minute)); err != nil {
				return fmt.Errorf("release: %w", err)
			}
			arn, err := e.ECS.Run(ctx, ecsx.RunInput{
				Revision: revision, Container: container, Command: command, StartedBy: ReleaseStartedBy(e.App), Network: net,
				LaunchType: e.Manifest.LaunchType, Strategy: e.Manifest.CapacityProviderStrategy, Tags: tags,
			})
			if err != nil {
				return err
			}
			if lost, err := e.lostRace(ctx, arn); err != nil || lost {
				e.stop(arn, "another release task started first")
				if err != nil {
					return err
				}
				if attempt >= 3 {
					return errors.New("release: lost the race for the release task 3 times")
				}
				continue
			}
			e.UI.Printf("task %s", ecsx.TaskID(arn))
			return e.awaitRelease(ctx, arn, revision, container, r.Timeout.Or(30*time.Minute))
		}
	})
}

// TaskFailed is a release task's non-zero exit.
type TaskFailed struct {
	Task string
	Code int32
}

func (t *TaskFailed) Error() string { return fmt.Sprintf("release task %s exited %d", t.Task, t.Code) }

// lostRace reports whether another release task started alongside arn, earlier.
func (e *Env) lostRace(ctx context.Context, arn string) (bool, error) {
	active, err := e.ECS.Active(ctx, ReleaseStartedBy(e.App))
	if err != nil {
		return false, err
	}
	var mine *time.Time
	for _, t := range active {
		if aws.ToString(t.TaskArn) == arn {
			mine = t.CreatedAt
		}
	}
	if mine == nil {
		return false, nil // already stopped: its exit code says how
	}
	for _, t := range active {
		other := aws.ToString(t.TaskArn)
		if other == arn {
			continue
		}
		if t.CreatedAt == nil || t.CreatedAt.Before(*mine) || (t.CreatedAt.Equal(*mine) && other < arn) {
			return true, nil
		}
	}
	return false, nil
}

func (e *Env) awaitRelease(ctx context.Context, arn, revision, container string, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t, err := e.ECS.WaitStopped(wctx, arn)
	if err != nil {
		reason := "the deploy was interrupted"
		if ctx.Err() == nil {
			reason = fmt.Sprintf("it ran past %s", timeout)
		}
		e.stop(arn, "ecsd: "+reason)
		return fmt.Errorf("release task %s stopped: %s", ecsx.TaskID(arn), reason)
	}
	e.printTaskLogs(ctx, revision, container, arn)
	code, err := ecsx.ExitCode(t, container)
	if err != nil {
		return err
	}
	if code != 0 {
		return &TaskFailed{ecsx.TaskID(arn), code}
	}
	e.UI.Printf("release task %s exited 0", ecsx.TaskID(arn))
	return nil
}

// stop is best effort, and outlives a cancelled deploy's context.
func (e *Env) stop(arn, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.ECS.Stop(ctx, arn, reason); err != nil {
		e.UI.Warnf("stopping %s: %v", ecsx.TaskID(arn), err)
	}
}

func (e *Env) printTaskLogs(ctx context.Context, revision, container, arn string) {
	if e.Logs == nil {
		return
	}
	def, err := e.ECS.Describe(ctx, revision)
	if err != nil {
		return
	}
	cd, err := ecsx.Container(def.TaskDefinition, container)
	if err != nil {
		return
	}
	if src, ok := logs.SourceOf(cd); ok {
		if err := logs.Task(ctx, e.Logs, src, ecsx.TaskID(arn), uiWriter{e.UI}); err != nil {
			e.UI.Warnf("logs of %s: %v", ecsx.TaskID(arn), err)
		}
	}
}

type uiWriter struct{ ui *ui.UI }

func (w uiWriter) Write(b []byte) (int, error) {
	w.ui.Printf("%s", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

func (e *Env) rollAndCheck(ctx context.Context, revisions map[string]string, sha string) error {
	if err := e.roll(ctx, revisions); err != nil {
		return err
	}
	return e.check(ctx, sha)
}

func (e *Env) roll(ctx context.Context, revisions map[string]string) error {
	return e.UI.Group("roll", func() error {
		g, gctx := errgroup.WithContext(ctx)
		for _, name := range e.Manifest.ServiceNames() {
			rev, ok := revisions[name]
			if !ok {
				e.UI.Warnf("%s: no revision to roll to; left as it is", name)
				continue
			}
			g.Go(func() error {
				return e.ECS.Roll(gctx, e.Manifest.ServiceName(name), rev, e.Config.Timeouts.Roll.Or(20*time.Minute))
			})
		}
		return g.Wait()
	})
}

func (e *Env) vars(sha string) expand.Lookup {
	return expand.Map(map[string]string{"sha": sha, "app": e.App, "stage": e.Stage}, func(key string) (string, bool) {
		if name, ok := strings.CutPrefix(key, "env."); ok {
			return os.LookupEnv(name)
		}
		return e.Manifest.Host(key)
	})
}

// Checks expands ecsd.yml's health and smoke checks for sha.
func (e *Env) Checks(sha string) (health, smoke []probe.Check, err error) {
	lookup := e.vars(sha)
	x := func(s string) string {
		if err != nil {
			return ""
		}
		var v string
		v, err = expand.String(s, lookup)
		return v
	}
	interval := e.HealthInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	for _, h := range e.Config.Health {
		health = append(health, probe.Check{URL: x(h.URL), Expect: h.Expect, Revision: h.Revision, Attempts: 36, Interval: interval, Streak: 3})
	}
	for _, c := range e.Config.Smoke {
		headers := map[string]string{}
		for k, v := range c.Headers {
			headers[k] = x(v)
		}
		attempts := c.Attempts
		if attempts == 0 {
			attempts = 10
		}
		smoke = append(smoke, probe.Check{
			Name: c.Name, URL: x(c.URL), Method: c.Method, Headers: headers, Body: x(c.Body), Expect: c.Expect,
			Contains: c.Contains, Attempts: attempts, Interval: c.Interval.Or(6 * time.Second),
		})
	}
	return health, smoke, err
}

func (e *Env) check(ctx context.Context, sha string) error {
	health, smoke, err := e.Checks(sha)
	if err != nil {
		return err
	}
	if len(health) > 0 {
		if err := e.UI.Group("health", func() error { return e.probeAll(ctx, health, sha) }); err != nil {
			return err
		}
	}
	if len(smoke) > 0 {
		return e.UI.Group("smoke", func() error {
			for _, c := range smoke {
				if err := e.Probe.Run(ctx, c, sha); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return nil
}

func (e *Env) probeAll(ctx context.Context, checks []probe.Check, sha string) error {
	g, gctx := errgroup.WithContext(ctx)
	for _, c := range checks {
		g.Go(func() error { return e.Probe.Run(gctx, c, sha) })
	}
	return g.Wait()
}

// restore returns the services to rel's revisions after a failed deploy, and checks them; nothing is recorded, as rel
// is still the current release.
func (e *Env) restore(ctx context.Context, rel *release.Release) error {
	ctx = context.WithoutCancel(ctx)
	e.UI.Step("rolling back to %s", short(rel.SHA))
	if err := e.roll(ctx, rel.Revisions); err != nil {
		return err
	}
	return e.check(ctx, rel.SHA)
}

func (e *Env) summarize(verb string, rel *release.Release) {
	e.UI.Step("%s %s to %s (release version %d)", strings.ToLower(verb), short(rel.SHA), e.Stage, rel.Version)
	e.UI.Summary("### %s `%s` to %s\n\n| | revision | image |\n|---|---|---|", verb, short(rel.SHA), e.Stage)
	for _, name := range slices.Sorted(maps.Keys(rel.Revisions)) {
		e.UI.Summary("| %s | `%s` | `%s` |", name, ecsx.Short(rel.Revisions[name]), rel.Images[name])
	}
}

func short(sha string) string { return sha[:min(12, len(sha))] }

// DefaultHTTP is the client probes use.
var DefaultHTTP = &http.Client{Timeout: 10 * time.Second}
