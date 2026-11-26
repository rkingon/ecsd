package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/rkingon/ecsd/internal/config"
	"github.com/rkingon/ecsd/internal/deploy"
	"github.com/rkingon/ecsd/internal/ecsx"
	"github.com/rkingon/ecsd/internal/gitx"
	"github.com/rkingon/ecsd/internal/images"
	"github.com/rkingon/ecsd/internal/logs"
	"github.com/rkingon/ecsd/internal/manifest"
	"github.com/rkingon/ecsd/internal/probe"
	"github.com/rkingon/ecsd/internal/release"
	"github.com/rkingon/ecsd/internal/session"
	"github.com/rkingon/ecsd/internal/ui"
)

// app loads what a command needs, once: ecsd.yml, AWS clients, the manifest.
type app struct {
	stage, configPath, manifestRef, region, repo string

	cfg      *config.File
	aws      aws.Config
	ecs      ecsAPI
	ssm      ssmAPI
	logs     logs.API
	manifest *manifest.Manifest
	ui       *ui.UI
	interval time.Duration // between polls; tests shorten it
	pinner   deploy.Pinner // tests replace ECR
}

type ecsAPI interface {
	ecsx.API
	ListServices(context.Context, *ecs.ListServicesInput, ...func(*ecs.Options)) (*ecs.ListServicesOutput, error)
}

type ssmAPI interface {
	release.API
	session.SSMAPI
}

func (a *app) needStage() error {
	if a.stage == "" {
		return errors.New("--stage (or ECSD_STAGE) is required")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(a.stage) {
		return fmt.Errorf("stage %q: lowercase letters, digits and dashes", a.stage)
	}
	return nil
}

func (a *app) loadConfig() error {
	if a.cfg != nil {
		return nil
	}
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	a.cfg = cfg
	return nil
}

func (a *app) loadAWS(ctx context.Context) error {
	if a.ecs != nil {
		return nil
	}
	var opts []func(*awsconfig.LoadOptions) error
	if a.region != "" {
		opts = append(opts, awsconfig.WithRegion(a.region))
	}
	c, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return err
	}
	if c.Region == "" {
		return errors.New("no AWS region: --region, AWS_REGION, or a profile's region")
	}
	a.aws = c
	a.ecs = ecs.NewFromConfig(c)
	a.ssm = ssm.NewFromConfig(c)
	a.logs = cloudwatchlogs.NewFromConfig(c)
	return nil
}

func (a *app) manifestParameter() string {
	ref := a.manifestRef
	if ref == "" {
		ref = fmt.Sprintf("ssm:/%s/%s/deploy", a.cfg.App, a.stage)
	}
	return ref
}

// load is everything a stage command needs.
func (a *app) load(ctx context.Context) error {
	if err := a.needStage(); err != nil {
		return err
	}
	if err := a.loadConfig(); err != nil {
		return err
	}
	if err := a.loadAWS(ctx); err != nil {
		return err
	}
	if a.ui == nil {
		a.ui = ui.New(os.Stderr)
	}
	if a.manifest == nil {
		m, err := manifest.Fetch(ctx, a.ssm, a.manifestParameter())
		if err != nil {
			return err
		}
		if m.Region != "" && m.Region != a.aws.Region {
			return fmt.Errorf("the manifest is for %s, the credentials' region is %s", m.Region, a.aws.Region)
		}
		if err := crossCheck(a.cfg, m); err != nil {
			return err
		}
		a.manifest = m
	}
	return nil
}

// crossCheck catches names in ecsd.yml the manifest doesn't have.
func crossCheck(cfg *config.File, m *manifest.Manifest) error {
	var errs []error
	known := func(name string) bool { _, _, ok := m.Family(name); return ok }
	for name := range cfg.Images {
		if name != "*" && !known(name) {
			errs = append(errs, fmt.Errorf("images.%s: not a service or task in the manifest", name))
		}
	}
	if cfg.Release != nil && !known(cfg.Release.Task) {
		errs = append(errs, fmt.Errorf("release.task: %s isn't a service or task in the manifest", cfg.Release.Task))
	}
	if cfg.Console != nil && !known(cfg.Console.Task) {
		errs = append(errs, fmt.Errorf("console.task: %s isn't a service or task in the manifest", cfg.Console.Task))
	}
	return errors.Join(errs...)
}

func (a *app) releaseParameter() string {
	if a.manifest != nil && a.manifest.ReleaseParameter != "" {
		return a.manifest.ReleaseParameter
	}
	return fmt.Sprintf("/%s/%s/release", a.cfg.App, a.stage)
}

func (a *app) ecsClient() *ecsx.Client {
	return &ecsx.Client{API: a.ecs, Cluster: a.manifest.Cluster, UI: a.ui, Interval: a.interval}
}

func (a *app) store() *release.Store {
	return &release.Store{API: a.ssm, Parameter: a.releaseParameter()}
}

func (a *app) env() *deploy.Env {
	e := &deploy.Env{
		App: a.cfg.App, Stage: a.stage, Config: a.cfg, Manifest: a.manifest,
		ECS: a.ecsClient(), Store: a.store(), Logs: a.logs, UI: a.ui, Actor: actor(),
		Probe: &probe.Prober{HTTP: deploy.DefaultHTTP, Log: a.ui.Printf}, HealthInterval: a.interval,
		Images: a.pinner,
	}
	if e.Images == nil {
		cfg := a.aws
		e.Images = &images.Resolver{Client: func(region string) images.DescribeImagesAPI {
			return ecr.NewFromConfig(cfg, func(o *ecr.Options) { o.Region = region })
		}}
	}
	if repo := (gitx.Repo{Dir: a.repo}); repo.Has("HEAD") {
		e.Git = repo
	}
	return e
}

// actor is who ran ecsd, and from which workflow run when it's Actions.
func actor() string {
	if u := os.Getenv("GITHUB_ACTOR"); u != "" {
		if run := os.Getenv("GITHUB_RUN_ID"); run != "" {
			return fmt.Sprintf("%s (%s/%s/actions/runs/%s)", u, os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"), run)
		}
		return u
	}
	return os.Getenv("USER")
}

// resolveSHA takes a commit given on the command line, GITHUB_SHA, or HEAD, as a full hash.
func (a *app) resolveSHA(given string) (string, error) {
	if given == "" {
		given = os.Getenv("GITHUB_SHA")
	}
	repo := gitx.Repo{Dir: a.repo}
	if given == "" {
		given = "HEAD"
	}
	if sha, err := repo.Resolve(given); err == nil {
		return sha, nil
	}
	if regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(given) {
		return given, nil
	}
	return "", fmt.Errorf("commit %q: not in %s, and not a full hash", given, a.repo)
}

func parseImages(flags []string) (map[string]string, error) {
	images := map[string]string{}
	for _, f := range flags {
		name, ref, ok := strings.Cut(f, "=")
		if !ok || name == "" || ref == "" {
			return nil, fmt.Errorf("--image %q: want name=image", f)
		}
		images[name] = ref
	}
	return images, nil
}
