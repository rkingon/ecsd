package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"

	"github.com/rkingon/ecsd/internal/config"
	"github.com/rkingon/ecsd/internal/ecsx"
	"github.com/rkingon/ecsd/internal/iampolicy"
	"github.com/rkingon/ecsd/internal/images"
	"github.com/rkingon/ecsd/internal/logs"
	"github.com/rkingon/ecsd/internal/manifest"
	"github.com/rkingon/ecsd/internal/schema"
	"github.com/rkingon/ecsd/internal/ui"
)

func manifestCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "manifest", Short: "Inspect, check or start a stage manifest"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "show",
			Short: "Print the stage's manifest",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := a.load(cmd.Context()); err != nil {
					return err
				}
				return printJSON(a.manifest)
			},
		},
		&cobra.Command{
			Use:   "validate",
			Short: "Check the manifest and ecsd.yml against each other and the account: templates, services, controllers",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx := cmd.Context()
				if err := a.load(ctx); err != nil {
					return err
				}
				if err := validateLive(ctx, a); err != nil {
					return err
				}
				fmt.Println("ok")
				return nil
			},
		},
		manifestInitCmd(a),
	)
	return cmd
}

func validateLive(ctx context.Context, a *app) error {
	c := a.ecsClient()
	var errs []error
	for _, name := range a.manifest.Names() {
		fam, isService, _ := a.manifest.Family(name)
		def, err := c.Describe(ctx, fam.Template)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if img, ok := a.cfg.ImageFor(name); ok {
			containers := img.Containers
			if len(containers) == 0 {
				containers = []string{""}
			}
			for _, c := range containers {
				if _, err := ecsx.Container(def.TaskDefinition, c); err != nil {
					errs = append(errs, fmt.Errorf("images.%s: %w", name, err))
				}
			}
		} else {
			errs = append(errs, fmt.Errorf("images: nothing for %s, and no *", name))
		}
		if !isService {
			continue
		}
		s, err := c.Service(ctx, a.manifest.ServiceName(name))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if dc := s.DeploymentController; dc != nil && dc.Type != "ECS" {
			errs = append(errs, fmt.Errorf("%s: deployment controller %s; ecsd rolls ECS-controlled services only", name, dc.Type))
		}
		if cb := s.DeploymentConfiguration; cb == nil || cb.DeploymentCircuitBreaker == nil || !cb.DeploymentCircuitBreaker.Enable {
			a.ui.Warnf("%s: no deployment circuit breaker, so a rollout whose tasks never start runs until timeouts.roll", name)
		}
		if family := aws.ToString(def.Family); !strings.HasPrefix(ecsx.Short(aws.ToString(s.TaskDefinition)), family+":") {
			a.ui.Warnf("%s: runs family %s, its template is %s", name, ecsx.Short(aws.ToString(s.TaskDefinition)), family)
		}
	}
	return errors.Join(errs...)
}

func manifestInitCmd(a *app) *cobra.Command {
	var cluster string
	cmd := &cobra.Command{
		Use:   "init --cluster <name>",
		Short: "Print a starting manifest from a cluster's services: each one's current task definition as its template",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := a.loadAWS(ctx); err != nil {
				return err
			}
			var arns []string
			p := ecs.NewListServicesPaginator(a.ecs, &ecs.ListServicesInput{Cluster: aws.String(cluster)})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				arns = append(arns, page.ServiceArns...)
			}
			if len(arns) == 0 {
				return fmt.Errorf("%s has no services", cluster)
			}
			m := manifest.Manifest{Cluster: cluster, Region: a.aws.Region, Services: map[string]manifest.Family{}, Hosts: map[string]string{}}
			for len(arns) > 0 {
				n := min(10, len(arns))
				out, err := a.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(cluster), Services: arns[:n]})
				if err != nil {
					return err
				}
				for _, s := range out.Services {
					m.Services[aws.ToString(s.ServiceName)] = manifest.Family{Template: aws.ToString(s.TaskDefinition)}
				}
				arns = arns[n:]
			}
			fmt.Fprintln(os.Stderr, "# The templates are what each service runs now. Point them at task definitions your infrastructure owns,")
			fmt.Fprintln(os.Stderr, "# write this to /<app>/<stage>/deploy in SSM, and add hosts for health and smoke checks.")
			return printJSON(m)
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "cluster name")
	_ = cmd.MarkFlagRequired("cluster")
	return cmd
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func iamPolicyCmd(a *app) *cobra.Command {
	var operator bool
	cmd := &cobra.Command{
		Use:   "iam-policy",
		Short: "Print the least-privilege IAM policy for deploys (or --operator: console, exec, tunnel, logs) on this stage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := a.load(ctx); err != nil {
				return err
			}
			id, err := sts.NewFromConfig(a.aws).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return err
			}
			caller, err := arn.Parse(aws.ToString(id.Arn))
			if err != nil {
				return err
			}
			in := iampolicy.Inputs{
				Partition: caller.Partition, Region: a.aws.Region, Account: aws.ToString(id.Account),
				Cluster: clusterName(a.manifest.Cluster), ReleaseParameter: a.releaseParameter(),
			}
			if ref := a.manifestParameter(); !strings.HasPrefix(ref, "file:") {
				in.ManifestParameter = strings.TrimPrefix(ref, "ssm:")
			}
			for _, name := range a.manifest.ServiceNames() {
				in.Services = append(in.Services, a.manifest.ServiceName(name))
			}
			c := a.ecsClient()
			for _, name := range a.manifest.Names() {
				fam, _, _ := a.manifest.Family(name)
				def, err := c.Describe(ctx, fam.Template)
				if err != nil {
					return err
				}
				in.Families = appendNew(in.Families, aws.ToString(def.Family))
				for _, r := range []*string{def.TaskRoleArn, def.ExecutionRoleArn} {
					if r != nil {
						in.PassRoles = appendNew(in.PassRoles, *r)
					}
				}
				for i := range def.ContainerDefinitions {
					if src, ok := logs.SourceOf(&def.ContainerDefinitions[i]); ok {
						in.LogGroups = appendNew(in.LogGroups, src.Group)
					}
				}
			}
			if r := a.cfg.Release; r != nil {
				fam, _, _ := a.manifest.Family(r.Task)
				def, err := c.Describe(ctx, fam.Template)
				if err != nil {
					return err
				}
				in.ReleaseFamily = aws.ToString(def.Family)
			}
			env := a.env()
			for _, name := range a.manifest.Names() {
				img, err := env.Image(name, strings.Repeat("0", 40))
				if err != nil {
					return err
				}
				ref, err := images.Parse(img)
				if err != nil {
					return err
				}
				if acct, region, ok := ref.ECR(); ok {
					in.ECRRepositories = appendNew(in.ECRRepositories,
						fmt.Sprintf("arn:%s:ecr:%s:%s:repository/%s", caller.Partition, region, acct, ref.Repo))
				}
			}
			if operator {
				return printJSON(iampolicy.Operator(in))
			}
			return printJSON(iampolicy.Deploy(in))
		},
	}
	cmd.Flags().BoolVar(&operator, "operator", false, "the policy for people operating the stage")
	return cmd
}

func appendNew(ss []string, s string) []string {
	if slices.Contains(ss, s) {
		return ss
	}
	return append(ss, s)
}

func clusterName(c string) string {
	if i := strings.LastIndex(c, "/"); i >= 0 {
		return c[i+1:]
	}
	return c
}

func schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "schema <config|manifest>",
		Short:     "Print the JSON Schema of ecsd.yml or the stage manifest",
		Args:      cobra.ExactArgs(1),
		ValidArgs: schema.Names,
		RunE: func(_ *cobra.Command, args []string) error {
			b, err := schema.Generate(args[0])
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(b)
			return err
		},
	}
}

// checkCmd is offline: ecsd.yml alone, or against a manifest file, for CI on pull requests.
func checkCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Validate ecsd.yml offline, and against --manifest file:path when given",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(a.configPath)
			if err != nil {
				return err
			}
			if path, ok := strings.CutPrefix(a.manifestRef, "file:"); ok {
				m, err := manifest.Load(path)
				if err != nil {
					return err
				}
				if err := crossCheck(cfg, m); err != nil {
					return err
				}
			}
			ui.New(os.Stderr).Printf("%s: ok", a.configPath)
			return nil
		},
	}
}
