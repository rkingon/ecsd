package iampolicy

import (
	"slices"
	"strings"
	"testing"
)

func TestDeploy(t *testing.T) {
	p := Deploy(Inputs{
		Partition: "aws", Region: "us-east-1", Account: "123", Cluster: "shop-prod",
		Services: []string{"api"}, Families: []string{"shop-api", "shop-worker"},
		PassRoles:         []string{"arn:aws:iam::123:role/exec", "arn:aws:iam::123:role/exec"},
		ManifestParameter: "/shop/prod/deploy", ReleaseParameter: "/shop/prod/release",
		ReleaseFamily: "shop-api", ECRRepositories: []string{"arn:aws:ecr:us-west-2:999:repository/shop/api"},
	})
	j := p.JSON()
	for _, st := range p.Statement {
		if slices.Contains(st.Action, "ecs:RegisterTaskDefinition") && slices.Contains(st.Resource, "*") {
			t.Error("RegisterTaskDefinition on *")
		}
		if st.Sid == "ReleaseTask" && (len(st.Resource) != 1 || !strings.HasSuffix(st.Resource[0], "task-definition/shop-api:*")) {
			t.Errorf("RunTask on %v, want the release task's family only", st.Resource)
		}
	}
	if !strings.Contains(j, `"arn:aws:ecr:us-west-2:999:repository/shop/api"`) || strings.Contains(j, "repository/*") {
		t.Errorf("no ecr:DescribeImages on the registry:\n%s", j)
	}
	if strings.Contains(Deploy(Inputs{Partition: "aws", Region: "r", Account: "1", Cluster: "c", ReleaseParameter: "/a"}).JSON(), "ecs:RunTask") {
		t.Error("RunTask without a release task")
	}
	for _, want := range []string{
		`"arn:aws:ecs:us-east-1:123:service/shop-prod/api"`,
		`"arn:aws:ecs:us-east-1:123:task-definition/shop-worker:*"`,
		`"arn:aws:ssm:us-east-1:123:parameter/shop/prod/release"`,
		`"ecs-tasks.amazonaws.com"`,
	} {
		if !strings.Contains(j, want) {
			t.Errorf("policy lacks %s:\n%s", want, j)
		}
	}
	if strings.Count(j, "role/exec") != 1 {
		t.Errorf("PassRole resources not deduplicated:\n%s", j)
	}
	if strings.Contains(j, "ReleaseTaskLogs") {
		t.Error("a log statement without log groups")
	}
}

func TestOperatorDropsEmptyStatements(t *testing.T) {
	p := Operator(Inputs{Partition: "aws", Region: "us-east-1", Account: "1", Cluster: "c", ReleaseParameter: "/a/b/release"})
	for _, s := range p.Statement {
		if len(s.Resource) == 0 {
			t.Errorf("%s has no resources", s.Sid)
		}
	}
}
