// Package config reads ecsd.yml: what a deploy runs, versioned with the code it deploys. Where it runs (cluster,
// templates, network, hosts) is the stage manifest's business.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/invopop/jsonschema"
	"go.yaml.in/yaml/v3"
)

type File struct {
	App               string           `yaml:"app" json:"app" jsonschema:"pattern=^[a-z0-9][a-z0-9-]*$" jsonschema_description:"The app's name: the manifest and release parameters default to /<app>/<stage>/deploy and /<app>/<stage>/release."`
	Images            map[string]Image `yaml:"images" json:"images" jsonschema_description:"Image per service or task name, or * for all. Variables: ${registry} ${name} ${sha} ${app} ${stage}."`
	SkipUnlessChanged []string         `yaml:"skip_unless_changed,omitempty" json:"skip_unless_changed,omitempty" jsonschema_description:"Gitignore-style patterns over the files changed since the released commit; a deploy with no matching change does nothing. !pattern excludes."`
	BeforeRoll        []Gate           `yaml:"before_roll,omitempty" json:"before_roll,omitempty" jsonschema_description:"Gates run after registering and before the release command and rollout, in order."`
	Release           *Release         `yaml:"release,omitempty" json:"release,omitempty" jsonschema_description:"A one-off task run before the rollout, one at a time per app: migrations."`
	Health            []Health         `yaml:"health,omitempty" json:"health,omitempty" jsonschema_description:"Endpoints that must answer, and report the deployed commit, once every service has rolled."`
	Smoke             []Check          `yaml:"smoke,omitempty" json:"smoke,omitempty" jsonschema_description:"Requests that must get the expected answer before a deploy is recorded as a release."`
	OnFailure         string           `yaml:"on_failure,omitempty" json:"on_failure,omitempty" jsonschema:"enum=stop,enum=rollback,default=stop" jsonschema_description:"rollback: a failed rollout, health check or smoke test returns the services to the current release."`
	Console           *Console         `yaml:"console,omitempty" json:"console,omitempty" jsonschema_description:"The one-off task ecsd console starts."`
	VerifyImage       string           `yaml:"verify_image" json:"verify_image" jsonschema_description:"Required. A shell command run for every image before it's deployed or rolled back to, with ECSD_IMAGE set (pinned to its digest); non-zero stops the deploy. none deploys images unverified, and says so on every deploy."`
	ImagePolicy       ImagePolicy      `yaml:"image_policy,omitempty" json:"image_policy,omitempty" jsonschema_description:"Opt-outs from the image rules: every image is in the manifest's registry, and pinned to a digest (ECR tags are resolved)."`
	Timeouts          Timeouts         `yaml:"timeouts,omitempty" json:"timeouts,omitempty"`
}

// Image is a string (the reference) or an object naming the container that gets it.
type Image struct {
	Ref       string `yaml:"image" json:"image"`
	Container string `yaml:"container,omitempty" json:"container,omitempty"`
}

func (i *Image) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		i.Ref = n.Value
		return nil
	}
	type plain Image
	return decodeStrict(n, (*plain)(i))
}

func (Image) JSONSchema() *jsonschema.Schema {
	props := jsonschema.NewProperties()
	props.Set("image", &jsonschema.Schema{Type: "string"})
	props.Set("container", &jsonschema.Schema{Type: "string", Description: "Defaults to the task definition's first container."})
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		{Type: "string"},
		{Type: "object", Properties: props, Required: []string{"image"}, AdditionalProperties: jsonschema.FalseSchema},
	}}
}

// Gate is exactly one of drain or exec.
type Gate struct {
	Drain *Drain `yaml:"drain,omitempty" json:"drain,omitempty" jsonschema_description:"Wait until no task started by started_by is running: work a restart would lose."`
	Exec  string `yaml:"exec,omitempty" json:"exec,omitempty" jsonschema_description:"A shell command; non-zero stops the deploy."`
}

type Drain struct {
	StartedBy string   `yaml:"started_by" json:"started_by"`
	Timeout   Duration `yaml:"timeout,omitempty" json:"timeout,omitempty" jsonschema_description:"Default 30m."`
}

type Release struct {
	Task      string   `yaml:"task" json:"task" jsonschema_description:"A service or task name from the manifest; its new revision runs."`
	Command   []string `yaml:"command,omitempty" json:"command,omitempty" jsonschema_description:"Overrides the container's command."`
	Container string   `yaml:"container,omitempty" json:"container,omitempty" jsonschema_description:"Defaults to the image's container."`
	Timeout   Duration `yaml:"timeout,omitempty" json:"timeout,omitempty" jsonschema_description:"How long it may run before it's stopped. Default 30m."`
	Wait      Duration `yaml:"wait,omitempty" json:"wait,omitempty" jsonschema_description:"How long to wait for another deploy's release task to finish. Default 30m."`
}

type Health struct {
	URL      string `yaml:"url" json:"url"`
	Revision string `yaml:"revision,omitempty" json:"revision,omitempty" jsonschema_description:"Dot path into the JSON body (.revision, .build.sha) that must equal the deployed commit; a prefix of 7 or more characters matches."`
	Expect   int    `yaml:"expect,omitempty" json:"expect,omitempty" jsonschema_description:"Status. Default 200."`
}

type Check struct {
	Name     string            `yaml:"name,omitempty" json:"name,omitempty"`
	URL      string            `yaml:"url" json:"url"`
	Method   string            `yaml:"method,omitempty" json:"method,omitempty" jsonschema_description:"Default GET."`
	Headers  map[string]string `yaml:"headers,omitempty" json:"headers,omitempty" jsonschema_description:"Values may use ${env.NAME}."`
	Body     string            `yaml:"body,omitempty" json:"body,omitempty"`
	Expect   int               `yaml:"expect,omitempty" json:"expect,omitempty" jsonschema_description:"Status. Default 200."`
	Contains string            `yaml:"contains,omitempty" json:"contains,omitempty" jsonschema_description:"Text the body must contain."`
	Attempts int               `yaml:"attempts,omitempty" json:"attempts,omitempty" jsonschema_description:"Default 10."`
	Interval Duration          `yaml:"interval,omitempty" json:"interval,omitempty" jsonschema_description:"Between attempts. Default 6s."`
}

type Console struct {
	Task      string   `yaml:"task" json:"task"`
	Command   []string `yaml:"command,omitempty" json:"command,omitempty" jsonschema_description:"Default [sh]."`
	Container string   `yaml:"container,omitempty" json:"container,omitempty"`
}

type ImagePolicy struct {
	AnyRegistry bool `yaml:"any_registry,omitempty" json:"any_registry,omitempty" jsonschema_description:"Images may come from outside the manifest's registry."`
	Unpinned    bool `yaml:"unpinned,omitempty" json:"unpinned,omitempty" jsonschema_description:"Images outside ECR may be deployed by tag. ECR tags are always resolved to digests."`
}

// VerifyNone is verify_image's explicit opt-out.
const VerifyNone = "none"

type Timeouts struct {
	Roll Duration `yaml:"roll,omitempty" json:"roll,omitempty" jsonschema_description:"Per service, until its deployment completes and its old tasks are gone. Default 20m."`
}

// Duration is written as Go durations are: 90s, 30m, 1h30m.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

func (Duration) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`}
}

// Or returns d, or def when d is unset.
func (d Duration) Or(def time.Duration) time.Duration {
	if d.Duration > 0 {
		return d.Duration
	}
	return def
}

func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func Parse(b []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	return &f, f.Validate()
}

var appName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (f *File) Validate() error {
	var errs []error
	if !appName.MatchString(f.App) {
		errs = append(errs, fmt.Errorf("app: %q isn't lowercase letters, digits and dashes", f.App))
	}
	if len(f.Images) == 0 {
		errs = append(errs, errors.New("images: none"))
	}
	for name, img := range f.Images {
		if img.Ref == "" {
			errs = append(errs, fmt.Errorf("images.%s: empty", name))
		}
	}
	if f.VerifyImage == "" {
		errs = append(errs, errors.New("verify_image: required: a command that checks $ECSD_IMAGE (gh attestation verify, cosign verify), or none"))
	}
	switch f.OnFailure {
	case "", "stop", "rollback":
	default:
		errs = append(errs, fmt.Errorf("on_failure: %q isn't stop or rollback", f.OnFailure))
	}
	for i, g := range f.BeforeRoll {
		if (g.Drain == nil) == (g.Exec == "") {
			errs = append(errs, fmt.Errorf("before_roll[%d]: needs exactly one of drain or exec", i))
		}
		if g.Drain != nil && g.Drain.StartedBy == "" {
			errs = append(errs, fmt.Errorf("before_roll[%d].drain: started_by is empty", i))
		}
	}
	if f.Release != nil && f.Release.Task == "" {
		errs = append(errs, errors.New("release.task: empty"))
	}
	if f.Console != nil && f.Console.Task == "" {
		errs = append(errs, errors.New("console.task: empty"))
	}
	for i, h := range f.Health {
		if h.URL == "" {
			errs = append(errs, fmt.Errorf("health[%d].url: empty", i))
		}
	}
	for i, c := range f.Smoke {
		if c.URL == "" {
			errs = append(errs, fmt.Errorf("smoke[%d].url: empty", i))
		}
	}
	return errors.Join(errs...)
}

// ImageFor is the image entry for a service or task name, falling back to *.
func (f *File) ImageFor(name string) (Image, bool) {
	if img, ok := f.Images[name]; ok {
		return img, true
	}
	img, ok := f.Images["*"]
	return img, ok
}

func decodeStrict(n *yaml.Node, v any) error {
	// Node.Decode ignores the decoder's KnownFields, so unknown keys are checked by re-encoding through a strict decoder.
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(v)
}
