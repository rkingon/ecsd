// Package ecsx is the ECS work a deploy does: registering from templates, following rollouts by deployment, running
// one-off tasks.
package ecsx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/rkingon/ecsd/internal/ui"
)

// API is the subset of the ECS client ecsd calls; tests fake it.
type API interface {
	DescribeTaskDefinition(context.Context, *ecs.DescribeTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error)
	RegisterTaskDefinition(context.Context, *ecs.RegisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error)
	DescribeServices(context.Context, *ecs.DescribeServicesInput, ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
	UpdateService(context.Context, *ecs.UpdateServiceInput, ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error)
	RunTask(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
	DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	StopTask(context.Context, *ecs.StopTaskInput, ...func(*ecs.Options)) (*ecs.StopTaskOutput, error)
	ExecuteCommand(context.Context, *ecs.ExecuteCommandInput, ...func(*ecs.Options)) (*ecs.ExecuteCommandOutput, error)
}

type Client struct {
	API      API
	Cluster  string
	UI       *ui.UI
	Interval time.Duration // between polls; tests shorten it
}

func (c *Client) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return 10 * time.Second
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Definition is a registered task definition with its tags, as DescribeTaskDefinition returns it.
type Definition struct {
	*types.TaskDefinition
	Tags []types.Tag
}

func (c *Client) Describe(ctx context.Context, arn string) (*Definition, error) {
	out, err := c.API.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(arn),
		Include:        []types.TaskDefinitionField{types.TaskDefinitionFieldTags},
	})
	if err != nil {
		return nil, fmt.Errorf("task definition %s: %w", arn, err)
	}
	return &Definition{out.TaskDefinition, out.Tags}, nil
}

// Container returns the named container, or the first when name is empty.
func Container(td *types.TaskDefinition, name string) (*types.ContainerDefinition, error) {
	if len(td.ContainerDefinitions) == 0 {
		return nil, fmt.Errorf("%s has no containers", aws.ToString(td.TaskDefinitionArn))
	}
	if name == "" {
		return &td.ContainerDefinitions[0], nil
	}
	for i := range td.ContainerDefinitions {
		if aws.ToString(td.ContainerDefinitions[i].Name) == name {
			return &td.ContainerDefinitions[i], nil
		}
	}
	return nil, fmt.Errorf("%s has no container %q", aws.ToString(td.TaskDefinitionArn), name)
}

type Registered struct {
	ARN       string
	Container string // the container that got the image
	Reused    bool
}

// Register copies template as a new revision of its family, changing only container's image (none when image is
// empty) and adding tags. Never the family's latest revision: that may be an older deploy's. When the latest already
// is this copy (same template, image and tags), as on a rerun, it's reused rather than registered again, so services
// already on it aren't restarted.
func (c *Client) Register(ctx context.Context, template, container, image string, tags map[string]string) (Registered, error) {
	def, err := c.Describe(ctx, template)
	if err != nil {
		return Registered{}, err
	}
	td := def.TaskDefinition
	cd, err := Container(td, container)
	if err != nil {
		return Registered{}, err
	}
	if image != "" {
		cd.Image = aws.String(image)
	}
	reg := Registered{Container: aws.ToString(cd.Name)}

	if latest, err := c.Describe(ctx, aws.ToString(td.Family)); err == nil && sameCopy(latest, reg.Container, aws.ToString(cd.Image), tags) {
		reg.ARN, reg.Reused = aws.ToString(latest.TaskDefinitionArn), true
		return reg, nil
	}

	in := &ecs.RegisterTaskDefinitionInput{
		Family:                  td.Family,
		ContainerDefinitions:    td.ContainerDefinitions,
		TaskRoleArn:             td.TaskRoleArn,
		ExecutionRoleArn:        td.ExecutionRoleArn,
		NetworkMode:             td.NetworkMode,
		Volumes:                 td.Volumes,
		PlacementConstraints:    td.PlacementConstraints,
		RequiresCompatibilities: td.RequiresCompatibilities,
		Cpu:                     td.Cpu,
		Memory:                  td.Memory,
		PidMode:                 td.PidMode,
		IpcMode:                 td.IpcMode,
		ProxyConfiguration:      td.ProxyConfiguration,
		EphemeralStorage:        td.EphemeralStorage,
		RuntimePlatform:         td.RuntimePlatform,
		EnableFaultInjection:    td.EnableFaultInjection,
		Tags:                    mergeTags(def.Tags, tags),
	}
	out, err := c.API.RegisterTaskDefinition(ctx, in)
	if err != nil {
		return Registered{}, fmt.Errorf("register %s: %w", aws.ToString(td.Family), err)
	}
	reg.ARN = aws.ToString(out.TaskDefinition.TaskDefinitionArn)
	return reg, nil
}

func sameCopy(latest *Definition, container, image string, tags map[string]string) bool {
	have := map[string]string{}
	for _, t := range latest.Tags {
		have[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	for k, v := range tags {
		if have[k] != v {
			return false
		}
	}
	cd, err := Container(latest.TaskDefinition, container)
	return err == nil && aws.ToString(cd.Image) == image
}

func mergeTags(have []types.Tag, add map[string]string) []types.Tag {
	var out []types.Tag
	for _, t := range have {
		// aws: tags are AWS's own and can't be set.
		if _, replaced := add[aws.ToString(t.Key)]; !replaced && !strings.HasPrefix(aws.ToString(t.Key), "aws:") {
			out = append(out, t)
		}
	}
	for k, v := range add {
		out = append(out, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (c *Client) Service(ctx context.Context, name string) (*types.Service, error) {
	out, err := c.API.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(c.Cluster), Services: []string{name}})
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", name, err)
	}
	if len(out.Failures) > 0 {
		return nil, fmt.Errorf("service %s: %s", name, aws.ToString(out.Failures[0].Reason))
	}
	if len(out.Services) == 0 || aws.ToString(out.Services[0].Status) != "ACTIVE" {
		return nil, fmt.Errorf("service %s: not active in %s", name, c.Cluster)
	}
	return &out.Services[0], nil
}

func primary(s *types.Service) *types.Deployment {
	for i := range s.Deployments {
		if aws.ToString(s.Deployments[i].Status) == "PRIMARY" {
			return &s.Deployments[i]
		}
	}
	return nil
}

// Settled is a service running only revision, as many tasks as it wants.
func Settled(s *types.Service, revision string) bool {
	p := primary(s)
	return p != nil && len(s.Deployments) == 1 && aws.ToString(p.TaskDefinition) == revision &&
		p.RolloutState == types.DeploymentRolloutStateCompleted && s.RunningCount == s.DesiredCount
}

// ErrRolledBack is a rollout that ended with the service on another revision: a circuit breaker's rollback, or a
// newer deployment. Waiting for "stable" alone reports these as success.
var ErrRolledBack = errors.New("rolled back")

// Roll moves service onto revision and follows the deployment it starts, by id, until it completes and the old tasks
// are gone. A service already settled on revision is left alone, so a rerun is a no-op.
func (c *Client) Roll(ctx context.Context, service, revision string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s, err := c.Service(ctx, service)
	if err != nil {
		return err
	}
	if dc := s.DeploymentController; dc != nil && dc.Type != types.DeploymentControllerTypeEcs {
		return fmt.Errorf("service %s: deployment controller %s; ecsd rolls ECS-controlled services only", service, dc.Type)
	}
	if Settled(s, revision) {
		c.UI.Printf("%s: already on %s", service, short(revision))
		return nil
	}

	var id string
	if p := primary(s); p != nil && aws.ToString(p.TaskDefinition) == revision && p.RolloutState == types.DeploymentRolloutStateInProgress {
		id = aws.ToString(p.Id) // a rerun of an interrupted deploy: follow the rollout already under way
	} else {
		out, err := c.API.UpdateService(ctx, &ecs.UpdateServiceInput{
			Cluster: aws.String(c.Cluster), Service: aws.String(service), TaskDefinition: aws.String(revision),
		})
		if err != nil {
			return fmt.Errorf("update %s: %w", service, err)
		}
		if p := primary(out.Service); p != nil && aws.ToString(p.TaskDefinition) == revision {
			id = aws.ToString(p.Id)
		}
	}
	c.UI.Printf("%s: rolling to %s", service, short(revision))

	seen := time.Now()
	for {
		if s, err = c.Service(ctx, service); err != nil {
			return err
		}
		if id == "" {
			// UpdateService's answer can predate the deployment it made; it shows on a later read.
			if p := primary(s); p != nil && aws.ToString(p.TaskDefinition) == revision {
				id = aws.ToString(p.Id)
			}
		}
		if id != "" {
			d := deployment(s, id)
			switch {
			case d == nil || aws.ToString(d.Status) != "PRIMARY":
				return fmt.Errorf("%s: %w to %s", service, ErrRolledBack, onRevision(s))
			case d.RolloutState == types.DeploymentRolloutStateFailed:
				return fmt.Errorf("%s: rollout of %s failed: %s", service, short(revision), aws.ToString(d.RolloutStateReason))
			case Settled(s, revision):
				c.UI.Printf("%s: %d/%d on %s", service, s.RunningCount, s.DesiredCount, short(revision))
				return nil
			}
		}
		seen = c.printEvents(service, s, seen)
		if err := sleep(ctx, c.interval()); err != nil {
			if errors.Is(err, context.Canceled) {
				return fmt.Errorf("%s: interrupted while rolling to %s: %s", service, short(revision), describeDeployments(s))
			}
			return fmt.Errorf("%s: not settled on %s within %s: %s", service, short(revision), timeout, describeDeployments(s))
		}
	}
}

func deployment(s *types.Service, id string) *types.Deployment {
	for i := range s.Deployments {
		if aws.ToString(s.Deployments[i].Id) == id {
			return &s.Deployments[i]
		}
	}
	return nil
}

func onRevision(s *types.Service) string {
	if p := primary(s); p != nil {
		return short(aws.ToString(p.TaskDefinition))
	}
	return "an unknown revision"
}

func describeDeployments(s *types.Service) string {
	var parts []string
	for _, d := range s.Deployments {
		parts = append(parts, fmt.Sprintf("%s %s %s %d/%d", aws.ToString(d.Status), short(aws.ToString(d.TaskDefinition)),
			d.RolloutState, d.RunningCount, d.DesiredCount))
	}
	return strings.Join(parts, "; ")
}

// printEvents prints the service's events newer than since; they say why tasks don't start.
func (c *Client) printEvents(service string, s *types.Service, since time.Time) time.Time {
	latest := since
	for i := len(s.Events) - 1; i >= 0; i-- {
		e := s.Events[i]
		if at := aws.ToTime(e.CreatedAt); at.After(since) {
			c.UI.Printf("%s: %s", service, aws.ToString(e.Message))
			if at.After(latest) {
				latest = at
			}
		}
	}
	return latest
}

// short is a task definition ARN's family:revision.
func short(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// Short is exported for callers printing revisions.
func Short(arn string) string { return short(arn) }
