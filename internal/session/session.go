// Package session opens interactive sessions into tasks (ECS Exec) and port forwards (SSM), handing the stream to
// AWS's session-manager-plugin as the AWS CLI does. The plugin isn't linked in: its Go source depends on a module that
// no longer exists and on the retired AWS SDK v1.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/rkingon/ecsd/internal/ecsx"
)

type SSMAPI interface {
	StartSession(context.Context, *ssm.StartSessionInput, ...func(*ssm.Options)) (*ssm.StartSessionOutput, error)
}

type Session struct {
	SessionID  string `json:"SessionId"`
	StreamURL  string `json:"StreamUrl"`
	TokenValue string `json:"TokenValue"`
}

type Opener struct {
	ECS     *ecsx.Client
	SSM     SSMAPI
	Region  string
	Profile string
	// Plugin runs the session; tests replace it.
	Plugin func(ctx context.Context, s Session, request map[string]any, endpoint string) error
}

// Exec runs command interactively in a task's container.
func (o *Opener) Exec(ctx context.Context, task *types.Task, container, command string) error {
	ct, err := containerOf(task, container)
	if err != nil {
		return err
	}
	out, err := o.ECS.API.ExecuteCommand(ctx, &ecs.ExecuteCommandInput{
		Cluster: aws.String(o.ECS.Cluster), Task: task.TaskArn, Container: ct.Name, Command: aws.String(command), Interactive: true,
	})
	if err != nil {
		return fmt.Errorf("execute command: %w", err)
	}
	s := Session{aws.ToString(out.Session.SessionId), aws.ToString(out.Session.StreamUrl), aws.ToString(out.Session.TokenValue)}
	return o.plugin(ctx, s, map[string]any{"Target": target(o.ECS.Cluster, task, ct)})
}

// Forward listens on localPort and forwards to port in the task, or to host:port through it when host is set: a
// database only the task's network reaches.
func (o *Opener) Forward(ctx context.Context, task *types.Task, container, host string, port, localPort int) error {
	ct, err := containerOf(task, container)
	if err != nil {
		return err
	}
	doc := "AWS-StartPortForwardingSession"
	params := map[string][]string{"portNumber": {fmt.Sprint(port)}, "localPortNumber": {fmt.Sprint(localPort)}}
	if host != "" {
		doc = "AWS-StartPortForwardingSessionToRemoteHost"
		params["host"] = []string{host}
	}
	tgt := target(o.ECS.Cluster, task, ct)
	out, err := o.SSM.StartSession(ctx, &ssm.StartSessionInput{Target: aws.String(tgt), DocumentName: aws.String(doc), Parameters: params})
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	s := Session{aws.ToString(out.SessionId), aws.ToString(out.StreamUrl), aws.ToString(out.TokenValue)}
	return o.plugin(ctx, s, map[string]any{"Target": tgt, "DocumentName": doc, "Parameters": params})
}

func (o *Opener) plugin(ctx context.Context, s Session, request map[string]any) error {
	endpoint := fmt.Sprintf("https://ssm.%s.amazonaws.com", o.Region)
	if o.Plugin != nil {
		return o.Plugin(ctx, s, request, endpoint)
	}
	return o.runPlugin(ctx, s, request, endpoint)
}

const responseEnv = "AWS_SSM_START_SESSION_RESPONSE_ECSD"

func (o *Opener) runPlugin(ctx context.Context, s Session, request map[string]any, endpoint string) error {
	path, err := exec.LookPath("session-manager-plugin")
	if err != nil {
		return errors.New("session-manager-plugin isn't installed: " +
			"https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html")
	}
	resp, err := json.Marshal(s)
	if err != nil {
		return err
	}
	req, err := json.Marshal(request)
	if err != nil {
		return err
	}
	// The plugin reads the session from the variable this names, so its token isn't in the process list.
	cmd := exec.CommandContext(ctx, path, responseEnv, o.Region, "StartSession", o.Profile, string(req), endpoint)
	cmd.Env = append(os.Environ(), responseEnv+"="+string(resp))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl-C belongs to the remote shell; the plugin forwards it.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	return cmd.Run()
}

func containerOf(task *types.Task, name string) (*types.Container, error) {
	for i := range task.Containers {
		if name == "" || aws.ToString(task.Containers[i].Name) == name {
			return &task.Containers[i], nil
		}
	}
	return nil, fmt.Errorf("task %s has no container %q", ecsx.TaskID(aws.ToString(task.TaskArn)), name)
}

// target is SSM's name for a container: ecs:<cluster name>_<task id>_<runtime id>.
func target(cluster string, task *types.Task, ct *types.Container) string {
	if i := strings.LastIndex(cluster, "/"); i >= 0 {
		cluster = cluster[i+1:]
	}
	return fmt.Sprintf("ecs:%s_%s_%s", cluster, ecsx.TaskID(aws.ToString(task.TaskArn)), aws.ToString(ct.RuntimeId))
}
