package main

import (
	"fmt"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/spf13/cobra"

	"github.com/rkingon/ecsd/internal/deploy"
	"github.com/rkingon/ecsd/internal/ecsx"
	"github.com/rkingon/ecsd/internal/release"
)

type deployFlags struct {
	sha        string
	images     []string
	force      bool
	noRollback bool
	skipGates  bool
}

func (f *deployFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.sha, "sha", "", "commit to deploy (default GITHUB_SHA, else HEAD)")
	cmd.Flags().StringArrayVar(&f.images, "image", nil, "name=image, over ecsd.yml's (repeatable): a digest promoted from another registry")
	cmd.Flags().BoolVar(&f.force, "force", false, "deploy even if superseded, unchanged, or already released")
}

func (f *deployFlags) options(a *app) (deploy.Options, error) {
	sha, err := a.resolveSHA(f.sha)
	if err != nil {
		return deploy.Options{}, err
	}
	images, err := parseImages(f.images)
	if err != nil {
		return deploy.Options{}, err
	}
	return deploy.Options{SHA: sha, Images: images, Force: f.force, NoRollback: f.noRollback, SkipGates: f.skipGates}, nil
}

func deployCmd(a *app) *cobra.Command {
	var f deployFlags
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy a commit: register, gates, release task, roll, health, smoke, record",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.load(cmd.Context()); err != nil {
				return err
			}
			opts, err := f.options(a)
			if err != nil {
				return err
			}
			_, err = a.env().Deploy(cmd.Context(), opts)
			if err != nil {
				a.ui.Errorf("%v", err)
				return exitError{1}
			}
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().BoolVar(&f.noRollback, "no-rollback", false, "leave services where a failure finds them, whatever on_failure says")
	cmd.Flags().BoolVar(&f.skipGates, "skip-gates", false, "don't run before_roll")
	return cmd
}

func planCmd(a *app) *cobra.Command {
	var f deployFlags
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what deploy would do, changing nothing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.load(cmd.Context()); err != nil {
				return err
			}
			opts, err := f.options(a)
			if err != nil {
				return err
			}
			env := a.env()
			p, err := env.Plan(cmd.Context(), opts)
			if err != nil {
				return err
			}
			w := os.Stdout
			fmt.Fprintf(w, "stage    %s (cluster %s)\ncommit   %s\n", a.stage, a.manifest.Cluster, p.SHA)
			if p.Current != nil {
				fmt.Fprintf(w, "released %s (version %d, %s)\n", p.Current.SHA, p.Current.Version, p.Current.At.Format(time.RFC3339))
			} else {
				fmt.Fprintln(w, "released nothing yet")
			}
			if p.Skip != "" {
				fmt.Fprintf(w, "\nnothing to deploy: %s\n", p.Skip)
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "\nNAME\tKIND\tIMAGE")
			for _, name := range a.manifest.Names() {
				_, isService, _ := a.manifest.Family(name)
				kind := "task"
				if isService {
					kind = "service → " + a.manifest.ServiceName(name)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", name, kind, p.Images[name])
			}
			_ = tw.Flush()
			if r := a.cfg.Release; r != nil {
				fmt.Fprintf(w, "\nrelease task: %s %v\n", r.Task, r.Command)
			}
			health, smoke, err := env.Checks(p.SHA)
			if err != nil {
				return err
			}
			for _, c := range health {
				fmt.Fprintf(w, "health: %s\n", c.URL)
			}
			for _, c := range smoke {
				fmt.Fprintf(w, "smoke:  %s %s → %d\n", orGET(c.Method), c.URL, orOK(c.Expect))
			}
			return nil
		},
	}
	f.bind(cmd)
	return cmd
}

func orGET(m string) string {
	if m == "" {
		return "GET"
	}
	return m
}

func orOK(code int) int {
	if code == 0 {
		return 200
	}
	return code
}

func rollbackCmd(a *app) *cobra.Command {
	var opts deploy.RollbackOptions
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Return the stage to a recorded release (default: the one before the current)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.load(cmd.Context()); err != nil {
				return err
			}
			if _, err := a.env().Rollback(cmd.Context(), opts); err != nil {
				a.ui.Errorf("%v", err)
				return exitError{1}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.To, "to", "", "a released commit (7+ characters) or release version (v12)")
	cmd.Flags().BoolVar(&opts.SkipGates, "skip-gates", false, "don't run before_roll")
	return cmd
}

func supersededCmd(a *app) *cobra.Command {
	var sha string
	cmd := &cobra.Command{
		Use:   "superseded",
		Short: "Say whether a deploy of the commit would do nothing (output skipped=true|false), to skip a build early",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.load(cmd.Context()); err != nil {
				return err
			}
			full, err := a.resolveSHA(sha)
			if err != nil {
				return err
			}
			p, err := a.env().Plan(cmd.Context(), deploy.Options{SHA: full})
			if err != nil {
				return err
			}
			a.ui.Output("skipped", fmt.Sprint(p.Skip != ""))
			if p.Skip != "" {
				fmt.Println(p.Skip)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sha, "sha", "", "commit (default GITHUB_SHA, else HEAD)")
	return cmd
}

func historyCmd(a *app) *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List releases, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.load(cmd.Context()); err != nil {
				return err
			}
			h, err := a.store().History(cmd.Context(), n)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "VERSION\tCOMMIT\tKIND\tAT\tBY")
			for _, r := range h {
				kind := string(r.Kind)
				if r.Kind == release.Rollback {
					kind += " from " + r.From[:min(12, len(r.From))]
				}
				fmt.Fprintf(tw, "v%d\t%s\t%s\t%s\t%s\n", r.Version, r.SHA[:min(12, len(r.SHA))], kind, r.At.Format(time.RFC3339), r.By)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVarP(&n, "limit", "n", 20, "how many")
	return cmd
}

func statusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show each service's rollout and the current release, and tasks that failed in the last hour",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := a.load(ctx); err != nil {
				return err
			}
			cur, err := a.store().Current(ctx)
			if err != nil {
				return err
			}
			if cur != nil {
				fmt.Printf("release v%d: %s (%s, %s by %s)\n\n", cur.Version, cur.SHA, cur.Kind, cur.At.Format(time.RFC3339), cur.By)
			}
			var names []string
			for _, n := range a.manifest.ServiceNames() {
				names = append(names, a.manifest.ServiceName(n))
			}
			out := &ecs.DescribeServicesOutput{}
			for len(names) > 0 { // 10 per call
				n := min(10, len(names))
				page, err := a.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(a.manifest.Cluster), Services: names[:n]})
				if err != nil {
					return err
				}
				out.Services, out.Failures = append(out.Services, page.Services...), append(out.Failures, page.Failures...)
				names = names[n:]
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tREVISION\tROLLOUT\tRUNNING\tDEPLOYMENTS\tRELEASED")
			for _, s := range out.Services {
				var p types.Deployment
				for _, d := range s.Deployments {
					if aws.ToString(d.Status) == "PRIMARY" {
						p = d
					}
				}
				released := "-"
				if cur != nil {
					released = "no"
					if slices.Contains(mapValues(cur.Revisions), aws.ToString(p.TaskDefinition)) {
						released = "yes"
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%d\t%s\n", aws.ToString(s.ServiceName), ecsx.Short(aws.ToString(p.TaskDefinition)),
					p.RolloutState, s.RunningCount, s.DesiredCount, len(s.Deployments), released)
			}
			for _, f := range out.Failures {
				fmt.Fprintf(tw, "%s\t(%s)\n", aws.ToString(f.Arn), aws.ToString(f.Reason))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			return printFailedTasks(cmd, a)
		},
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// printFailedTasks lists the stopped tasks ECS still reports (about an hour's) that failed to start or exited non-zero.
func printFailedTasks(cmd *cobra.Command, a *app) error {
	ctx := cmd.Context()
	var arns []string
	p := ecs.NewListTasksPaginator(a.ecs, &ecs.ListTasksInput{Cluster: aws.String(a.manifest.Cluster), DesiredStatus: types.DesiredStatusStopped})
	for p.HasMorePages() && len(arns) < 300 {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		arns = append(arns, page.TaskArns...)
	}
	var failed []types.Task
	for len(arns) > 0 {
		n := min(100, len(arns))
		out, err := a.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: aws.String(a.manifest.Cluster), Tasks: arns[:n]})
		if err != nil {
			return err
		}
		for _, t := range out.Tasks {
			if t.StopCode == types.TaskStopCodeTaskFailedToStart || nonZero(t) {
				failed = append(failed, t)
			}
		}
		arns = arns[n:]
	}
	if len(failed) == 0 {
		return nil
	}
	slices.SortFunc(failed, func(x, y types.Task) int { return aws.ToTime(y.StoppedAt).Compare(aws.ToTime(x.StoppedAt)) })
	fmt.Println("\nfailed tasks:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, t := range failed[:min(10, len(failed))] {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", aws.ToTime(t.StoppedAt).Format(time.RFC3339), ecsx.Short(aws.ToString(t.TaskDefinitionArn)),
			ecsx.TaskID(aws.ToString(t.TaskArn)), aws.ToString(t.StoppedReason))
	}
	return tw.Flush()
}

func nonZero(t types.Task) bool {
	for _, c := range t.Containers {
		if c.ExitCode != nil && *c.ExitCode != 0 {
			return true
		}
	}
	return false
}
