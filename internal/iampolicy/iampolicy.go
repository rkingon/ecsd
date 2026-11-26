// Package iampolicy writes the least-privilege IAM policies ecsd needs for one stage, from its manifest and templates:
// one for deploys (CI) and one for people operating the stage (console, exec, tunnel, logs).
package iampolicy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

type Inputs struct {
	Partition, Region, Account string
	Cluster                    string   // name
	Services                   []string // ECS service names
	Families                   []string // every task definition family ecsd registers into
	Templates                  []string // template families outside Families: only run --from-template runs them
	PassRoles                  []string // the templates' task and execution roles
	LogGroups                  []string
	ManifestParameter          string // empty when the manifest is a file
	ReleaseParameter           string
	ReleaseFamily              string   // the release task's; empty without one
	ECRRepositories            []string // ARNs of the ECR repositories the images name: their tags are resolved there
}

type Policy struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

type Statement struct {
	Sid       string                    `json:"Sid"`
	Effect    string                    `json:"Effect"`
	Action    []string                  `json:"Action"`
	Resource  []string                  `json:"Resource"`
	Condition map[string]map[string]any `json:"Condition,omitempty"`
}

func (in Inputs) arn(service, resource string) string {
	return fmt.Sprintf("arn:%s:%s:%s:%s:%s", in.Partition, service, in.Region, in.Account, resource)
}

func (in Inputs) clusterARN() string { return in.arn("ecs", "cluster/"+in.Cluster) }
func (in Inputs) tasks() string      { return in.arn("ecs", "task/"+in.Cluster+"/*") }

func (in Inputs) families(names []string) []string {
	var out []string
	for _, f := range names {
		out = append(out, in.arn("ecs", "task-definition/"+f+":*"))
	}
	return out
}

func (in Inputs) parameter(name string) string {
	return in.arn("ssm", "parameter/"+strings.TrimPrefix(name, "/"))
}

func (in Inputs) logGroups(suffix string) []string {
	var out []string
	for _, g := range in.LogGroups {
		out = append(out, in.arn("logs", "log-group:"+g+suffix))
	}
	return out
}

// Deploy is what ecsd deploy and ecsd rollback call.
func Deploy(in Inputs) Policy {
	var services []string
	for _, s := range in.Services {
		services = append(services, in.arn("ecs", "service/"+in.Cluster+"/"+s))
	}
	clusterIs := map[string]map[string]any{"ArnEquals": {"ecs:cluster": in.clusterARN()}}
	tagged := in.families(in.Families)
	st := []Statement{
		{Sid: "Read", Effect: "Allow", Resource: []string{"*"}, Action: []string{
			"ecs:DescribeTaskDefinition", "ecs:DescribeServices", "ecs:DescribeTasks", "ecs:ListTasks", "ecs:ListTagsForResource",
		}},
		{Sid: "RegisterOwnFamilies", Effect: "Allow", Action: []string{"ecs:RegisterTaskDefinition"}, Resource: in.families(in.Families)},
		{Sid: "Roll", Effect: "Allow", Action: []string{"ecs:UpdateService"}, Resource: services},
		{Sid: "PassTaskRoles", Effect: "Allow", Action: []string{"iam:PassRole"}, Resource: in.PassRoles,
			Condition: map[string]map[string]any{"StringEquals": {"iam:PassedToService": "ecs-tasks.amazonaws.com"}}},
		{Sid: "Releases", Effect: "Allow", Resource: []string{in.parameter(in.ReleaseParameter)},
			Action: []string{"ssm:GetParameter", "ssm:GetParameterHistory", "ssm:PutParameter"}},
	}
	if in.ReleaseFamily != "" {
		tagged = append(tagged, in.tasks())
		st = append(st,
			Statement{Sid: "ReleaseTask", Effect: "Allow", Action: []string{"ecs:RunTask"},
				Resource: in.families([]string{in.ReleaseFamily}), Condition: clusterIs},
			Statement{Sid: "StopOwnReleaseTask", Effect: "Allow", Action: []string{"ecs:StopTask"},
				Resource: []string{in.tasks()}, Condition: clusterIs})
	}
	st = append(st, Statement{Sid: "TagWhatItCreates", Effect: "Allow", Action: []string{"ecs:TagResource"}, Resource: tagged,
		Condition: map[string]map[string]any{"StringEquals": {"ecs:CreateAction": []string{"RegisterTaskDefinition", "RunTask"}}}})
	st = append(st, Statement{Sid: "PinImages", Effect: "Allow", Action: []string{"ecr:DescribeImages"}, Resource: in.ECRRepositories})
	if in.ManifestParameter != "" {
		st = append(st, Statement{Sid: "Manifest", Effect: "Allow", Action: []string{"ssm:GetParameter"},
			Resource: []string{in.parameter(in.ManifestParameter)}})
	}
	if len(in.LogGroups) > 0 {
		st = append(st, Statement{Sid: "ReleaseTaskLogs", Effect: "Allow", Action: []string{"logs:GetLogEvents"},
			Resource: in.logGroups(":log-stream:*")})
	}
	return finish(st)
}

// Operator is what status, logs, insights, history, run, console, exec and tunnel call.
func Operator(in Inputs) Policy {
	clusterIs := map[string]map[string]any{"ArnEquals": {"ecs:cluster": in.clusterARN()}}
	st := []Statement{
		{Sid: "Read", Effect: "Allow", Resource: []string{"*"}, Action: []string{
			"ecs:DescribeTaskDefinition", "ecs:DescribeServices", "ecs:DescribeTasks", "ecs:ListTasks", "ecs:ListTagsForResource",
		}},
		{Sid: "OneOffTasks", Effect: "Allow", Action: []string{"ecs:RunTask"}, Resource: in.families(append(slices.Clone(in.Families), in.Templates...)), Condition: clusterIs},
		{Sid: "TagOneOffTasks", Effect: "Allow", Action: []string{"ecs:TagResource"}, Resource: []string{in.tasks()},
			Condition: map[string]map[string]any{"StringEquals": {"ecs:CreateAction": "RunTask"}}},
		{Sid: "StopAndExec", Effect: "Allow", Action: []string{"ecs:StopTask", "ecs:ExecuteCommand"},
			Resource: []string{in.tasks(), in.clusterARN()}},
		{Sid: "PortForward", Effect: "Allow", Action: []string{"ssm:StartSession"}, Resource: []string{
			in.tasks(),
			fmt.Sprintf("arn:%s:ssm:%s::document/AWS-StartPortForwardingSession", in.Partition, in.Region),
			fmt.Sprintf("arn:%s:ssm:%s::document/AWS-StartPortForwardingSessionToRemoteHost", in.Partition, in.Region),
		}},
		{Sid: "PassTaskRoles", Effect: "Allow", Action: []string{"iam:PassRole"}, Resource: in.PassRoles,
			Condition: map[string]map[string]any{"StringEquals": {"iam:PassedToService": "ecs-tasks.amazonaws.com"}}},
		{Sid: "Releases", Effect: "Allow", Action: []string{"ssm:GetParameter", "ssm:GetParameterHistory"},
			Resource: []string{in.parameter(in.ReleaseParameter)}},
	}
	if in.ManifestParameter != "" {
		st = append(st, Statement{Sid: "Manifest", Effect: "Allow", Action: []string{"ssm:GetParameter"},
			Resource: []string{in.parameter(in.ManifestParameter)}})
	}
	if len(in.LogGroups) > 0 {
		st = append(st,
			Statement{Sid: "Logs", Effect: "Allow", Action: []string{"logs:FilterLogEvents", "logs:GetLogEvents", "logs:StartQuery"},
				Resource: append(in.logGroups(""), in.logGroups(":*")...)},
			Statement{Sid: "QueryResults", Effect: "Allow", Action: []string{"logs:GetQueryResults"}, Resource: []string{"*"}})
	}
	return finish(st)
}

// finish drops statements left without resources and sorts each list, so the output diffs cleanly.
func finish(st []Statement) Policy {
	var out []Statement
	for _, s := range st {
		if len(s.Resource) == 0 {
			continue
		}
		slices.Sort(s.Action)
		s.Resource = slices.Compact(slices.Sorted(slices.Values(s.Resource)))
		out = append(out, s)
	}
	return Policy{Version: "2012-10-17", Statement: out}
}

func (p Policy) JSON() string {
	b, _ := json.MarshalIndent(p, "", "  ")
	return string(b)
}
