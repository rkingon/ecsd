package ecsx

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/rkingon/ecsd/internal/manifest"
)

type RunInput struct {
	Revision   string
	Container  string // whose command is overridden
	Command    []string
	StartedBy  string
	Network    *manifest.Network
	Exec       bool // ECS Exec, for consoles
	LaunchType string
	Strategy   []manifest.CapacityProvider
	Tags       map[string]string
}

func (c *Client) Run(ctx context.Context, in RunInput) (string, error) {
	req := &ecs.RunTaskInput{
		Cluster:              aws.String(c.Cluster),
		TaskDefinition:       aws.String(in.Revision),
		StartedBy:            aws.String(in.StartedBy),
		EnableExecuteCommand: in.Exec,
		PropagateTags:        types.PropagateTagsTaskDefinition,
	}
	if len(in.Strategy) > 0 {
		for _, p := range in.Strategy {
			req.CapacityProviderStrategy = append(req.CapacityProviderStrategy, types.CapacityProviderStrategyItem{
				CapacityProvider: aws.String(p.CapacityProvider), Weight: p.Weight, Base: p.Base,
			})
		}
	} else {
		lt := in.LaunchType
		if lt == "" {
			lt = string(types.LaunchTypeFargate)
		}
		req.LaunchType = types.LaunchType(lt)
	}
	if in.Network != nil {
		assign := types.AssignPublicIpDisabled
		if in.Network.AssignPublicIP {
			assign = types.AssignPublicIpEnabled
		}
		req.NetworkConfiguration = &types.NetworkConfiguration{AwsvpcConfiguration: &types.AwsVpcConfiguration{
			Subnets: in.Network.Subnets, SecurityGroups: in.Network.SecurityGroups, AssignPublicIp: assign,
		}}
	}
	if len(in.Command) > 0 {
		req.Overrides = &types.TaskOverride{ContainerOverrides: []types.ContainerOverride{{
			Name: aws.String(in.Container), Command: in.Command,
		}}}
	}
	for k, v := range in.Tags {
		req.Tags = append(req.Tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	out, err := c.API.RunTask(ctx, req)
	if err != nil {
		return "", fmt.Errorf("run %s: %w", short(in.Revision), err)
	}
	if len(out.Failures) > 0 {
		f := out.Failures[0]
		return "", fmt.Errorf("run %s: %s %s", short(in.Revision), aws.ToString(f.Reason), aws.ToString(f.Detail))
	}
	if len(out.Tasks) == 0 {
		return "", fmt.Errorf("run %s: no task started", short(in.Revision))
	}
	return aws.ToString(out.Tasks[0].TaskArn), nil
}

func (c *Client) Task(ctx context.Context, arn string) (*types.Task, error) {
	out, err := c.API.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: aws.String(c.Cluster), Tasks: []string{arn}})
	if err != nil {
		return nil, fmt.Errorf("task %s: %w", TaskID(arn), err)
	}
	if len(out.Failures) > 0 || len(out.Tasks) == 0 {
		return nil, fmt.Errorf("task %s: not found", TaskID(arn))
	}
	return &out.Tasks[0], nil
}

// WaitStopped polls until the task stops, or ctx ends.
func (c *Client) WaitStopped(ctx context.Context, arn string) (*types.Task, error) {
	for {
		t, err := c.Task(ctx, arn)
		if err != nil {
			return nil, err
		}
		if aws.ToString(t.LastStatus) == "STOPPED" {
			return t, nil
		}
		if err := sleep(ctx, c.interval()); err != nil {
			return nil, err
		}
	}
}

// WaitRunning polls until the task runs, and its exec agent too when exec is set.
func (c *Client) WaitRunning(ctx context.Context, arn string, exec bool) (*types.Task, error) {
	for {
		t, err := c.Task(ctx, arn)
		if err != nil {
			return nil, err
		}
		switch aws.ToString(t.LastStatus) {
		case "STOPPED", "DEPROVISIONING":
			return nil, fmt.Errorf("task %s stopped: %s", TaskID(arn), aws.ToString(t.StoppedReason))
		case "RUNNING":
			if !exec || execAgentRunning(t) {
				return t, nil
			}
		}
		if err := sleep(ctx, min(c.interval(), 2*time.Second)); err != nil {
			return nil, err
		}
	}
}

func execAgentRunning(t *types.Task) bool {
	for _, ct := range t.Containers {
		for _, a := range ct.ManagedAgents {
			if a.Name == types.ManagedAgentNameExecuteCommandAgent && aws.ToString(a.LastStatus) == "RUNNING" {
				return true
			}
		}
	}
	return false
}

// ExitCode is container's exit code in a stopped task. A container that never ran has none, and that's an error.
func ExitCode(t *types.Task, container string) (int32, error) {
	for _, ct := range t.Containers {
		if container != "" && aws.ToString(ct.Name) != container {
			continue
		}
		if ct.ExitCode == nil {
			return 0, fmt.Errorf("task %s: %s has no exit code: %s %s", TaskID(aws.ToString(t.TaskArn)), aws.ToString(ct.Name),
				aws.ToString(t.StoppedReason), aws.ToString(ct.Reason))
		}
		return *ct.ExitCode, nil
	}
	return 0, fmt.Errorf("task %s: no container %q", TaskID(aws.ToString(t.TaskArn)), container)
}

// Active lists the tasks startedBy started that haven't stopped, whatever their desired status: a task being stopped
// still runs. Fails closed: any listing or description that fails or comes back short is an error.
func (c *Client) Active(ctx context.Context, startedBy string) ([]types.Task, error) {
	var arns []string
	for _, desired := range []types.DesiredStatus{types.DesiredStatusRunning, types.DesiredStatusStopped} {
		p := ecs.NewListTasksPaginator(c.API, &ecs.ListTasksInput{
			Cluster: aws.String(c.Cluster), StartedBy: aws.String(startedBy), DesiredStatus: desired,
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("list tasks started by %s: %w", startedBy, err)
			}
			arns = append(arns, page.TaskArns...)
		}
	}
	var active []types.Task
	for len(arns) > 0 {
		n := min(len(arns), 100)
		out, err := c.API.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: aws.String(c.Cluster), Tasks: arns[:n]})
		if err != nil {
			return nil, fmt.Errorf("describe tasks started by %s: %w", startedBy, err)
		}
		if len(out.Failures) > 0 || len(out.Tasks) != n {
			return nil, fmt.Errorf("describe tasks started by %s: %d of %d described", startedBy, len(out.Tasks), n)
		}
		for _, t := range out.Tasks {
			if aws.ToString(t.LastStatus) != "STOPPED" {
				active = append(active, t)
			}
		}
		arns = arns[n:]
	}
	return active, nil
}

// WaitNone waits until no task started by startedBy is active, up to timeout.
func (c *Client) WaitNone(ctx context.Context, startedBy string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		active, err := c.Active(ctx, startedBy)
		if err != nil {
			return err
		}
		if len(active) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d task(s) started by %s still running after %s", len(active), startedBy, timeout)
		}
		c.UI.Printf("waiting for %d task(s) started by %s", len(active), startedBy)
		if err := sleep(ctx, c.interval()); err != nil {
			return err
		}
	}
}

func (c *Client) Stop(ctx context.Context, arn, reason string) error {
	_, err := c.API.StopTask(ctx, &ecs.StopTaskInput{Cluster: aws.String(c.Cluster), Task: aws.String(arn), Reason: aws.String(reason)})
	return err
}

// TaskID is the last segment of a task ARN.
func TaskID(arn string) string { return short(arn) }
