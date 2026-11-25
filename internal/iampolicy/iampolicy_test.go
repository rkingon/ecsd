package iampolicy

import (
	"strings"
	"testing"
)

func TestDeploy(t *testing.T) {
	p := Deploy(Inputs{
		Partition: "aws", Region: "us-east-1", Account: "123", Cluster: "shop-prod",
		Services: []string{"api"}, Families: []string{"shop-api", "shop-worker"},
		PassRoles:         []string{"arn:aws:iam::123:role/exec", "arn:aws:iam::123:role/exec"},
		ManifestParameter: "/shop/prod/deploy", ReleaseParameter: "/shop/prod/release",
	})
	j := p.JSON()
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
