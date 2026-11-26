package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/spf13/cobra"

	"github.com/rkingon/ecsd/internal/deploy"
	"github.com/rkingon/ecsd/internal/ecsx"
	"github.com/rkingon/ecsd/internal/logs"
	"github.com/rkingon/ecsd/internal/manifest"
	"github.com/rkingon/ecsd/internal/session"
)

// released is the revision the current release runs for name.
func (a *app) released(ctx context.Context, name string) (string, error) {
	if _, _, ok := a.manifest.Family(name); !ok {
		return "", fmt.Errorf("%s isn't a service or task in the manifest", name)
	}
	cur, err := a.store().Current(ctx)
	if err != nil {
		return "", err
	}
	if cur == nil || cur.Revisions[name] == "" {
		return "", fmt.Errorf("%s has no released revision on %s yet (--from-template runs the manifest's template)", name, a.stage)
	}
	return cur.Revisions[name], nil
}

// revision is what a one-off task of name runs: its released revision, or with fromTemplate the manifest's template,
// for tasks before a stage's first deploy. Explicit, so a mistyped stage can't quietly run template code.
func (a *app) revision(ctx context.Context, name string, fromTemplate bool) (string, error) {
	if !fromTemplate {
		return a.released(ctx, name)
	}
	fam, _, ok := a.manifest.Family(name)
	if !ok {
		return "", fmt.Errorf("%s isn't a service or task in the manifest", name)
	}
	return fam.Template, nil
}

// oneOffNetwork mirrors deploy's: the task's own, the manifest's, or the first service's.
func (a *app) oneOffNetwork(ctx context.Context, name string) (*manifest.Network, error) {
	if fam, isService, _ := a.manifest.Family(name); !isService && fam.Network != nil {
		return fam.Network, nil
	}
	if a.manifest.Network != nil {
		return a.manifest.Network, nil
	}
	for _, svc := range a.manifest.ServiceNames() {
		s, err := a.ecsClient().Service(ctx, a.manifest.ServiceName(svc))
		if err != nil {
			return nil, err
		}
		if nc := s.NetworkConfiguration; nc != nil && nc.AwsvpcConfiguration != nil {
			c := nc.AwsvpcConfiguration
			return &manifest.Network{Subnets: c.Subnets, SecurityGroups: c.SecurityGroups, AssignPublicIP: c.AssignPublicIp == types.AssignPublicIpEnabled}, nil
		}
	}
	return nil, errors.New("no network for one-off tasks: set the manifest's network")
}

func (a *app) container(ctx context.Context, revision, name string) (*types.ContainerDefinition, error) {
	def, err := a.ecsClient().Describe(ctx, revision)
	if err != nil {
		return nil, err
	}
	return ecsx.Container(def.TaskDefinition, name)
}

func runCmd(a *app) *cobra.Command {
	var container string
	var detach, asRelease, fromTemplate bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "run <name> [-- command...] | run --release [-- command...]",
		Short: "Run a one-off task from the released revision of a service or task; exits with its exit code",
		Args: func(cmd *cobra.Command, args []string) error {
			if asRelease {
				if len(args) > 0 && cmd.ArgsLenAtDash() != 0 {
					return errors.New("--release takes no name: the task is release.task; give a command after --")
				}
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if asRelease {
				if detach || cmd.Flags().Changed("timeout") {
					return errors.New("--release waits for its task and uses release.timeout: drop --detach and --timeout")
				}
				return a.runRelease(ctx, container, args, fromTemplate)
			}
			if err := a.load(ctx); err != nil {
				return err
			}
			name, command := args[0], args[1:]
			rev, err := a.revision(ctx, name, fromTemplate)
			if err != nil {
				return err
			}
			cd, err := a.container(ctx, rev, container)
			if err != nil {
				return err
			}
			net, err := a.oneOffNetwork(ctx, name)
			if err != nil {
				return err
			}
			c := a.ecsClient()
			arn, err := c.Run(ctx, ecsx.RunInput{
				Revision: rev, Container: aws.ToString(cd.Name), Command: command, StartedBy: "ecsd-run-" + user(), Network: net,
				LaunchType: a.manifest.LaunchType, Strategy: a.manifest.CapacityProviderStrategy,
			})
			if err != nil {
				return err
			}
			a.ui.Printf("task %s (%s)", ecsx.TaskID(arn), ecsx.Short(rev))
			if detach {
				fmt.Println(arn)
				return nil
			}
			wctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			t, err := c.WaitStopped(wctx, arn)
			if err != nil {
				sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer scancel()
				_ = c.Stop(sctx, arn, "ecsd run: interrupted")
				return fmt.Errorf("task %s stopped: %w", ecsx.TaskID(arn), err)
			}
			if src, ok := logs.SourceOf(cd); ok {
				if err := logs.Task(ctx, a.logs, src, ecsx.TaskID(arn), os.Stdout); err != nil {
					a.ui.Warnf("logs: %v", err)
				}
			}
			code, err := ecsx.ExitCode(t, aws.ToString(cd.Name))
			if err != nil {
				return err
			}
			if code != 0 {
				return exitError{int(code)}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&container, "container", "", "container whose command is overridden (default the first)")
	cmd.Flags().BoolVar(&detach, "detach", false, "print the task ARN and return without waiting")
	cmd.Flags().DurationVar(&timeout, "timeout", time.Hour, "stop the task after this long")
	cmd.Flags().BoolVar(&fromTemplate, "from-template", false, "run the manifest's template instead of the released revision: before the first deploy")
	cmd.Flags().BoolVar(&asRelease, "release", false, "run release.task's command (or the one given) as a deploy's release step does: one at a time per app, waiting for any other")
	return cmd
}

func (a *app) runRelease(ctx context.Context, container string, command []string, fromTemplate bool) error {
	if err := a.load(ctx); err != nil {
		return err
	}
	r := a.cfg.Release
	if r == nil {
		return errors.New("--release: ecsd.yml has no release")
	}
	if len(command) == 0 {
		command = r.Command
	}
	if container == "" {
		container = r.Container
	}
	if container == "" {
		if img, ok := a.cfg.ImageFor(r.Task); ok && len(img.Containers) > 0 {
			container = img.Containers[0]
		}
	}
	rev, err := a.revision(ctx, r.Task, fromTemplate)
	if err != nil {
		return err
	}
	cd, err := a.container(ctx, rev, container)
	if err != nil {
		return err
	}
	err = a.env().RunRelease(ctx, rev, aws.ToString(cd.Name), command, map[string]string{"ecsd:run-by": user()})
	if failed := (*deploy.TaskFailed)(nil); errors.As(err, &failed) {
		return exitError{int(failed.Code)}
	}
	return err
}

// user names whoever runs a one-off task, in what startedBy allows.
func user() string {
	u := os.Getenv("USER")
	if gh := os.Getenv("GITHUB_ACTOR"); gh != "" {
		u = gh
	}
	u = regexp.MustCompile(`[^A-Za-z0-9_-]`).ReplaceAllString(u, "_")
	if u == "" {
		u = "unknown"
	}
	return u[:min(len(u), 100)]
}

func (a *app) opener() *session.Opener {
	return &session.Opener{ECS: a.ecsClient(), SSM: a.ssm, Region: a.aws.Region, Profile: os.Getenv("AWS_PROFILE")}
}

func consoleCmd(a *app) *cobra.Command {
	var task, container string
	var keep, stop bool
	cmd := &cobra.Command{
		Use:   "console [-- command...]",
		Short: "Start (or reuse) your one-off task from the released revision and open a shell in it, as heroku run",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.load(ctx); err != nil {
				return err
			}
			command := []string{"sh"}
			if c := a.cfg.Console; c != nil {
				task, container = or(task, c.Task), or(container, c.Container)
				if len(c.Command) > 0 {
					command = c.Command
				}
			}
			if len(args) > 0 {
				command = args
			}
			if task == "" {
				return errors.New("no console task: set console.task in ecsd.yml, or --task")
			}
			c := a.ecsClient()
			startedBy := "ecsd-console-" + user()
			mine, err := c.Active(ctx, startedBy)
			if err != nil {
				return err
			}
			if stop {
				for _, t := range mine {
					if err := c.Stop(ctx, aws.ToString(t.TaskArn), "ecsd console --stop"); err != nil {
						return err
					}
					a.ui.Printf("stopped %s", ecsx.TaskID(aws.ToString(t.TaskArn)))
				}
				return nil
			}
			var arn string
			if len(mine) > 0 {
				arn = aws.ToString(mine[0].TaskArn)
				a.ui.Printf("reusing %s", ecsx.TaskID(arn))
			} else {
				rev, err := a.released(ctx, task)
				if err != nil {
					return err
				}
				net, err := a.oneOffNetwork(ctx, task)
				if err != nil {
					return err
				}
				if arn, err = c.Run(ctx, ecsx.RunInput{
					Revision: rev, StartedBy: startedBy, Network: net, Exec: true,
					LaunchType: a.manifest.LaunchType, Strategy: a.manifest.CapacityProviderStrategy,
				}); err != nil {
					return err
				}
				a.ui.Printf("starting %s (%s)…", ecsx.TaskID(arn), ecsx.Short(rev))
			}
			if !keep {
				defer func() {
					sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					if err := c.Stop(sctx, arn, "ecsd console closed"); err == nil {
						a.ui.Printf("stopped %s", ecsx.TaskID(arn))
					}
				}()
			}
			wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			t, err := c.WaitRunning(wctx, arn, true)
			if err != nil {
				return err
			}
			return a.opener().Exec(ctx, t, container, shellJoin(command))
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "service or task to start (default console.task)")
	cmd.Flags().StringVar(&container, "container", "", "container to open (default console.container, else the first)")
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the task running for the next console to reuse")
	cmd.Flags().BoolVar(&stop, "stop", false, "stop your console tasks")
	return cmd
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// shellJoin quotes arguments for ECS Exec, which takes one command string.
func shellJoin(args []string) string {
	var out []string
	for _, a := range args {
		if a != "" && regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`).MatchString(a) {
			out = append(out, a)
			continue
		}
		out = append(out, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return strings.Join(out, " ")
}

// serviceTask is a running task of the named service.
func (a *app) serviceTask(ctx context.Context, name string) (*types.Task, error) {
	if _, isService, ok := a.manifest.Family(name); !ok || !isService {
		return nil, fmt.Errorf("%s isn't a service in the manifest", name)
	}
	out, err := a.ecs.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster: aws.String(a.manifest.Cluster), ServiceName: aws.String(a.manifest.ServiceName(name)), DesiredStatus: types.DesiredStatusRunning,
	})
	if err != nil {
		return nil, err
	}
	if len(out.TaskArns) == 0 {
		return nil, fmt.Errorf("%s has no running task", name)
	}
	return a.ecsClient().Task(ctx, out.TaskArns[0])
}

func execCmd(a *app) *cobra.Command {
	var container string
	cmd := &cobra.Command{
		Use:   "exec <service> [-- command...]",
		Short: "Run a command (default sh) in one of a service's running tasks, with ECS Exec",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.load(ctx); err != nil {
				return err
			}
			t, err := a.serviceTask(ctx, args[0])
			if err != nil {
				return err
			}
			command := []string{"sh"}
			if len(args) > 1 {
				command = args[1:]
			}
			return a.opener().Exec(ctx, t, container, shellJoin(command))
		},
	}
	cmd.Flags().StringVar(&container, "container", "", "container (default the first)")
	return cmd
}

func tunnelCmd(a *app) *cobra.Command {
	var local int
	var container string
	cmd := &cobra.Command{
		Use:   "tunnel <service> <[host:]port>",
		Short: "Forward a local port to a port in a service's task, or through it to host:port (a database)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.load(ctx); err != nil {
				return err
			}
			host, portStr, found := strings.Cut(args[1], ":")
			if !found {
				host, portStr = "", args[1]
			}
			port, err := strconv.Atoi(portStr)
			if err != nil || port <= 0 || port > 65535 {
				return fmt.Errorf("port %q", portStr)
			}
			if local == 0 {
				local = port
			}
			t, err := a.serviceTask(ctx, args[0])
			if err != nil {
				return err
			}
			a.ui.Printf("localhost:%d → %s via %s", local, args[1], ecsx.TaskID(aws.ToString(t.TaskArn)))
			return a.opener().Forward(ctx, t, container, host, port, local)
		},
	}
	cmd.Flags().IntVar(&local, "local-port", 0, "local port (default the same as the remote)")
	cmd.Flags().StringVar(&container, "container", "", "container (default the first)")
	return cmd
}

// logSources are the awslogs destinations of the named services and tasks (all when none are named), from their
// released revisions, else their templates.
func (a *app) logSources(ctx context.Context, names []string) ([]logs.Source, error) {
	if len(names) == 0 {
		names = a.manifest.Names()
	}
	cur, err := a.store().Current(ctx)
	if err != nil {
		return nil, err
	}
	var out []logs.Source
	seen := map[logs.Source]bool{}
	for _, name := range names {
		fam, _, ok := a.manifest.Family(name)
		if !ok {
			return nil, fmt.Errorf("%s isn't a service or task in the manifest", name)
		}
		rev := fam.Template
		if cur != nil && cur.Revisions[name] != "" {
			rev = cur.Revisions[name]
		}
		def, err := a.ecsClient().Describe(ctx, rev)
		if err != nil {
			return nil, err
		}
		for i := range def.ContainerDefinitions {
			if src, ok := logs.SourceOf(&def.ContainerDefinitions[i]); ok && !seen[src] {
				seen[src] = true
				out = append(out, src)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no awslogs containers among them")
	}
	return out, nil
}

func logsCmd(a *app) *cobra.Command {
	var f logs.Filter
	cmd := &cobra.Command{
		Use:   "logs [name...]",
		Short: "Print services' and tasks' CloudWatch logs (all when none are named)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.load(ctx); err != nil {
				return err
			}
			sources, err := a.logSources(ctx, args)
			if err != nil {
				return err
			}
			f.Poll = 3 * time.Second
			return logs.Tail(ctx, a.logs, sources, f, os.Stdout)
		},
	}
	cmd.Flags().DurationVar(&f.Since, "since", 15*time.Minute, "how far back")
	cmd.Flags().StringVar(&f.Pattern, "filter", "", "CloudWatch filter pattern")
	cmd.Flags().BoolVarP(&f.Follow, "follow", "f", false, "keep printing new events")
	return cmd
}

func insightsCmd(a *app) *cobra.Command {
	var since time.Duration
	var names []string
	cmd := &cobra.Command{
		Use:   "insights <query>",
		Short: "Run a CloudWatch Logs Insights query over the stage's log groups",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := a.load(ctx); err != nil {
				return err
			}
			sources, err := a.logSources(ctx, names)
			if err != nil {
				return err
			}
			var groups []string
			for _, s := range sources {
				if !containsStr(groups, s.Group) {
					groups = append(groups, s.Group)
				}
			}
			return logs.Insights(ctx, a.logs, groups, args[0], since, os.Stdout)
		},
	}
	cmd.Flags().DurationVar(&since, "since", time.Hour, "how far back")
	cmd.Flags().StringSliceVar(&names, "only", nil, "services or tasks whose groups to query (default all)")
	return cmd
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
