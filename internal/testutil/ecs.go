package testutil

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// Outcome is how a fake service's next rollout ends.
type Outcome int

const (
	Complete    Outcome = iota
	Fail                // the deployment fails and stays PRIMARY
	RollBack            // a circuit breaker: the deployment goes and the previous revision's comes back, completed
	NeverSettle         // in progress forever
)

type Service struct {
	Outcome Outcome
	Polls   int // describes before a rollout resolves
	svc     types.Service
	left    int
}

// ECS fakes task definitions, rolling services and one-off tasks. Every describe advances the world one step.
type ECS struct {
	mu        sync.Mutex
	defs      map[string]*types.TaskDefinition
	tags      map[string][]types.Tag
	revisions map[string]int32
	services  map[string]*Service
	tasks     map[string]*task
	seq       int

	// ExitCodes is a one-off task's exit code by task definition family; default 0.
	ExitCodes map[string]int32
	// TaskPolls is how many describes a one-off task takes to stop; default 1.
	TaskPolls int
	// HideNewDeployment leaves UpdateService's answer without the deployment it made, as ECS's can be.
	HideNewDeployment bool
	Calls             []string
}

type task struct {
	t       types.Task
	left    int
	forever bool
}

func NewECS() *ECS {
	return &ECS{
		defs: map[string]*types.TaskDefinition{}, tags: map[string][]types.Tag{}, revisions: map[string]int32{},
		services: map[string]*Service{}, tasks: map[string]*task{}, ExitCodes: map[string]int32{}, TaskPolls: 1,
	}
}

func (e *ECS) call(name string) { e.Calls = append(e.Calls, name) }

// Calls named op.
func (e *ECS) Count(op string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.Calls {
		if c == op {
			n++
		}
	}
	return n
}

// AddTemplate registers a task definition with one container per name in containers, and returns its ARN.
func (e *ECS) AddTemplate(family, image string, containers ...string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(containers) == 0 {
		containers = []string{family}
	}
	var cds []types.ContainerDefinition
	for _, c := range containers {
		cds = append(cds, types.ContainerDefinition{
			Name: aws.String(c), Image: aws.String(image),
			LogConfiguration: &types.LogConfiguration{LogDriver: types.LogDriverAwslogs, Options: map[string]string{
				"awslogs-group": "/app/" + family, "awslogs-stream-prefix": "ecs",
			}},
		})
	}
	return e.register(&types.TaskDefinition{
		Family: aws.String(family), ContainerDefinitions: cds, TaskRoleArn: aws.String("arn:aws:iam::1:role/" + family),
		ExecutionRoleArn: aws.String("arn:aws:iam::1:role/exec"), NetworkMode: types.NetworkModeAwsvpc,
		Cpu: aws.String("256"), Memory: aws.String("512"),
	}, []types.Tag{{Key: aws.String("team"), Value: aws.String("shop")}})
}

func (e *ECS) register(td *types.TaskDefinition, tags []types.Tag) string {
	family := aws.ToString(td.Family)
	e.revisions[family]++
	rev := e.revisions[family]
	arn := fmt.Sprintf("arn:aws:ecs:us-east-1:1:task-definition/%s:%d", family, rev)
	td.TaskDefinitionArn = aws.String(arn)
	td.Revision = rev
	td.Status = types.TaskDefinitionStatusActive
	e.defs[arn] = td
	e.tags[arn] = tags
	return arn
}

// AddService makes a service settled on revision.
func (e *ECS) AddService(name, revision string, desired int32) *Service {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	s := &Service{svc: types.Service{
		ServiceName: aws.String(name), Status: aws.String("ACTIVE"), TaskDefinition: aws.String(revision),
		DesiredCount: desired, RunningCount: desired,
		DeploymentController: &types.DeploymentController{Type: types.DeploymentControllerTypeEcs},
		NetworkConfiguration: &types.NetworkConfiguration{AwsvpcConfiguration: &types.AwsVpcConfiguration{
			Subnets: []string{"subnet-svc"}, SecurityGroups: []string{"sg-svc"},
		}},
		Deployments: []types.Deployment{{
			Id: aws.String(fmt.Sprintf("ecs-svc/%d", e.seq)), Status: aws.String("PRIMARY"), TaskDefinition: aws.String(revision),
			RolloutState: types.DeploymentRolloutStateCompleted, DesiredCount: desired, RunningCount: desired,
		}},
	}}
	e.services[name] = s
	return s
}

func (e *ECS) SetController(service string, t types.DeploymentControllerType) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.services[service].svc.DeploymentController = &types.DeploymentController{Type: t}
}

// Revision is what a service's PRIMARY deployment runs.
func (e *ECS) Revision(service string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return aws.ToString(e.services[service].svc.TaskDefinition)
}

// Image is the image of a registered revision's first container.
func (e *ECS) Image(revision string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return aws.ToString(e.defs[revision].ContainerDefinitions[0].Image)
}

// LatestImage is the first container's image in the family's newest revision: what an app starting the family by
// name runs.
func (e *ECS) LatestImage(family string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return aws.ToString(e.defs[e.resolve(family)].ContainerDefinitions[0].Image)
}

// Tags of a registered revision.
func (e *ECS) Tags(revision string) map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := map[string]string{}
	for _, t := range e.tags[revision] {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// StartTask adds a running task started by startedBy that stops after polls describes, or never when polls < 0.
func (e *ECS) StartTask(family, startedBy string, polls int) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startTask(family, startedBy, polls, nil)
}

func (e *ECS) startTask(family, startedBy string, polls int, td *types.TaskDefinition) string {
	e.seq++
	arn := fmt.Sprintf("arn:aws:ecs:us-east-1:1:task/c/%s%d", family, e.seq)
	var containers []types.Container
	if td != nil {
		for _, cd := range td.ContainerDefinitions {
			containers = append(containers, types.Container{Name: cd.Name, RuntimeId: aws.String("rt-" + aws.ToString(cd.Name))})
		}
	} else {
		containers = []types.Container{{Name: aws.String(family)}}
	}
	t := &task{t: types.Task{
		TaskArn: aws.String(arn), LastStatus: aws.String("RUNNING"), DesiredStatus: aws.String("RUNNING"),
		StartedBy: aws.String(startedBy), Containers: containers, Group: aws.String("family:" + family),
		CreatedAt: aws.Time(time.Unix(1_700_000_000+int64(e.seq), 0)),
	}, left: polls, forever: polls < 0}
	e.tasks[arn] = t
	return arn
}

// Retag changes who a task was started by.
func (e *ECS) Retag(arn, startedBy string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tasks[arn].t.StartedBy = aws.String(startedBy)
}

// Task is a task's current state.
func (e *ECS) Task(arn string) types.Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tasks[arn].t
}

// Tasks started by startedBy, in any state.
func (e *ECS) TasksStartedBy(startedBy string) []types.Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []types.Task
	for _, t := range e.tasks {
		if aws.ToString(t.t.StartedBy) == startedBy {
			out = append(out, t.t)
		}
	}
	return out
}

func (e *ECS) DescribeTaskDefinition(_ context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.call("DescribeTaskDefinition")
	arn := e.resolve(aws.ToString(in.TaskDefinition))
	td, ok := e.defs[arn]
	if !ok {
		return nil, fmt.Errorf("ClientException: unable to describe task definition %s", aws.ToString(in.TaskDefinition))
	}
	cp := *td
	cp.ContainerDefinitions = append([]types.ContainerDefinition(nil), td.ContainerDefinitions...)
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: &cp, Tags: e.tags[arn]}, nil
}

// resolve takes an ARN, family:revision, or a family (its latest revision).
func (e *ECS) resolve(ref string) string {
	if strings.HasPrefix(ref, "arn:") {
		return ref
	}
	if !strings.Contains(ref, ":") {
		ref = fmt.Sprintf("%s:%d", ref, e.revisions[ref])
	}
	return "arn:aws:ecs:us-east-1:1:task-definition/" + ref
}

func (e *ECS) RegisterTaskDefinition(_ context.Context, in *ecs.RegisterTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.call("RegisterTaskDefinition")
	for _, t := range in.Tags {
		if strings.HasPrefix(aws.ToString(t.Key), "aws:") {
			return nil, fmt.Errorf("InvalidParameterException: aws: tags are reserved")
		}
	}
	td := &types.TaskDefinition{
		Family: in.Family, ContainerDefinitions: in.ContainerDefinitions, TaskRoleArn: in.TaskRoleArn,
		ExecutionRoleArn: in.ExecutionRoleArn, NetworkMode: in.NetworkMode, Cpu: in.Cpu, Memory: in.Memory,
	}
	e.register(td, in.Tags)
	return &ecs.RegisterTaskDefinitionOutput{TaskDefinition: td, Tags: in.Tags}, nil
}

func (e *ECS) DescribeServices(_ context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.call("DescribeServices")
	out := &ecs.DescribeServicesOutput{}
	for _, name := range in.Services {
		name = name[strings.LastIndex(name, "/")+1:]
		s, ok := e.services[name]
		if !ok {
			out.Failures = append(out.Failures, types.Failure{Arn: aws.String(name), Reason: aws.String("MISSING")})
			continue
		}
		e.advance(s)
		out.Services = append(out.Services, copyService(s.svc))
	}
	return out, nil
}

func copyService(s types.Service) types.Service {
	s.Deployments = append([]types.Deployment(nil), s.Deployments...)
	return s
}

func (e *ECS) advance(s *Service) {
	p := &s.svc.Deployments[0]
	if p.RolloutState != types.DeploymentRolloutStateInProgress {
		return
	}
	if s.left > 0 {
		s.left--
		return
	}
	switch s.Outcome {
	case Complete:
		p.RolloutState = types.DeploymentRolloutStateCompleted
		p.RunningCount = s.svc.DesiredCount
		s.svc.Deployments = s.svc.Deployments[:1]
		s.svc.RunningCount = s.svc.DesiredCount
	case Fail:
		p.RolloutState = types.DeploymentRolloutStateFailed
		p.RolloutStateReason = aws.String("ECS deployment circuit breaker: tasks failed to start.")
	case RollBack:
		prev := s.svc.Deployments[1]
		e.seq++
		s.svc.Deployments = []types.Deployment{{
			Id: aws.String(fmt.Sprintf("ecs-svc/%d", e.seq)), Status: aws.String("PRIMARY"), TaskDefinition: prev.TaskDefinition,
			RolloutState: types.DeploymentRolloutStateCompleted, DesiredCount: s.svc.DesiredCount, RunningCount: s.svc.DesiredCount,
		}}
		s.svc.TaskDefinition = prev.TaskDefinition
		s.svc.RunningCount = s.svc.DesiredCount
	case NeverSettle:
	}
}

func (e *ECS) UpdateService(_ context.Context, in *ecs.UpdateServiceInput, _ ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.call("UpdateService")
	s, ok := e.services[aws.ToString(in.Service)]
	if !ok {
		return nil, fmt.Errorf("ServiceNotFoundException")
	}
	rev := aws.ToString(in.TaskDefinition)
	if _, ok := e.defs[rev]; !ok {
		return nil, fmt.Errorf("ClientException: no task definition %s", rev)
	}
	var old []types.Deployment
	for _, d := range s.svc.Deployments {
		d.Status = aws.String("ACTIVE")
		old = append(old, d)
	}
	e.seq++
	s.svc.Deployments = append([]types.Deployment{{
		Id: aws.String(fmt.Sprintf("ecs-svc/%d", e.seq)), Status: aws.String("PRIMARY"), TaskDefinition: aws.String(rev),
		RolloutState: types.DeploymentRolloutStateInProgress, DesiredCount: s.svc.DesiredCount,
		CreatedAt: aws.Time(time.Now()),
	}}, old...)
	s.svc.TaskDefinition = aws.String(rev)
	s.left = s.Polls
	answer := copyService(s.svc)
	if e.HideNewDeployment {
		answer.Deployments = old
	}
	return &ecs.UpdateServiceOutput{Service: &answer}, nil
}

func (e *ECS) RunTask(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.call("RunTask")
	td, ok := e.defs[e.resolve(aws.ToString(in.TaskDefinition))]
	if !ok {
		return nil, fmt.Errorf("ClientException: no task definition %s", aws.ToString(in.TaskDefinition))
	}
	if in.NetworkConfiguration == nil || len(in.NetworkConfiguration.AwsvpcConfiguration.Subnets) == 0 {
		return nil, fmt.Errorf("InvalidParameterException: Network Configuration must be provided when networkMode 'awsvpc' is specified")
	}
	polls := e.TaskPolls
	if in.EnableExecuteCommand {
		polls = -1
	}
	arn := e.startTask(aws.ToString(td.Family), aws.ToString(in.StartedBy), polls, td)
	t := e.tasks[arn]
	t.t.TaskDefinitionArn = td.TaskDefinitionArn
	t.t.Overrides = in.Overrides
	if in.EnableExecuteCommand {
		for i := range t.t.Containers {
			t.t.Containers[i].ManagedAgents = []types.ManagedAgent{{Name: types.ManagedAgentNameExecuteCommandAgent, LastStatus: aws.String("RUNNING")}}
		}
	}
	return &ecs.RunTaskOutput{Tasks: []types.Task{t.t}}, nil
}

func (e *ECS) DescribeTasks(_ context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.call("DescribeTasks")
	out := &ecs.DescribeTasksOutput{}
	for _, arn := range in.Tasks {
		t, ok := e.tasks[arn]
		if !ok {
			out.Failures = append(out.Failures, types.Failure{Arn: aws.String(arn), Reason: aws.String("MISSING")})
			continue
		}
		e.advanceTask(t)
		out.Tasks = append(out.Tasks, t.t)
	}
	return out, nil
}

func (e *ECS) advanceTask(t *task) {
	if aws.ToString(t.t.LastStatus) == "STOPPED" || t.forever {
		return
	}
	if t.left > 0 {
		t.left--
		return
	}
	e.stopTask(t, "Essential container in task exited")
}

func (e *ECS) stopTask(t *task, reason string) {
	family := strings.TrimPrefix(aws.ToString(t.t.Group), "family:")
	code := e.ExitCodes[family]
	t.t.LastStatus = aws.String("STOPPED")
	t.t.DesiredStatus = aws.String("STOPPED")
	t.t.StoppedReason = aws.String(reason)
	for i := range t.t.Containers {
		t.t.Containers[i].ExitCode = aws.Int32(code)
	}
}

func (e *ECS) ListTasks(_ context.Context, in *ecs.ListTasksInput, _ ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.call("ListTasks")
	out := &ecs.ListTasksOutput{}
	for arn, t := range e.tasks {
		if in.StartedBy != nil && aws.ToString(t.t.StartedBy) != aws.ToString(in.StartedBy) {
			continue
		}
		if in.DesiredStatus != "" && aws.ToString(t.t.DesiredStatus) != string(in.DesiredStatus) {
			continue
		}
		out.TaskArns = append(out.TaskArns, arn)
	}
	return out, nil
}

func (e *ECS) StopTask(_ context.Context, in *ecs.StopTaskInput, _ ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.call("StopTask")
	t, ok := e.tasks[aws.ToString(in.Task)]
	if !ok {
		return nil, fmt.Errorf("InvalidParameterException: no task")
	}
	t.forever = false
	e.stopTask(t, aws.ToString(in.Reason))
	for i := range t.t.Containers {
		t.t.Containers[i].ExitCode = aws.Int32(143)
	}
	return &ecs.StopTaskOutput{Task: &t.t}, nil
}

func (e *ECS) ExecuteCommand(_ context.Context, in *ecs.ExecuteCommandInput, _ ...func(*ecs.Options)) (*ecs.ExecuteCommandOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.call("ExecuteCommand")
	return &ecs.ExecuteCommandOutput{Session: &types.Session{
		SessionId: aws.String("s-1"), StreamUrl: aws.String("wss://example"), TokenValue: aws.String("token"),
	}, ContainerName: in.Container}, nil
}

func (e *ECS) ListServices(_ context.Context, in *ecs.ListServicesInput, _ ...func(*ecs.Options)) (*ecs.ListServicesOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := &ecs.ListServicesOutput{}
	for name := range e.services {
		out.ServiceArns = append(out.ServiceArns, "arn:aws:ecs:us-east-1:1:service/"+aws.ToString(in.Cluster)+"/"+name)
	}
	return out, nil
}
