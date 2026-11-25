package ecsx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/rkingon/ecsd/internal/testutil"
	"github.com/rkingon/ecsd/internal/ui"
)

func setup(outcome testutil.Outcome) (*Client, *testutil.ECS, string) {
	f := testutil.NewECS()
	old := f.AddTemplate("web", "web:1")
	next := f.AddTemplate("web", "web:2")
	f.AddService("web", old, 2).Outcome = outcome
	return &Client{API: f, Cluster: "c", UI: ui.Discard(), Interval: time.Millisecond}, f, next
}

func TestRollOutcomes(t *testing.T) {
	ctx := context.Background()

	c, f, next := setup(testutil.Complete)
	if err := c.Roll(ctx, "web", next, time.Second); err != nil {
		t.Errorf("complete: %v", err)
	}
	if err := c.Roll(ctx, "web", next, time.Second); err != nil || f.Count("UpdateService") != 1 {
		t.Errorf("roll onto the running revision: %v, %d updates", err, f.Count("UpdateService"))
	}

	c, _, next = setup(testutil.RollBack)
	if err := c.Roll(ctx, "web", next, time.Second); !errors.Is(err, ErrRolledBack) {
		t.Errorf("circuit breaker: %v", err)
	}

	c, _, next = setup(testutil.Fail)
	if err := c.Roll(ctx, "web", next, time.Second); err == nil || !strings.Contains(err.Error(), "circuit breaker") {
		t.Errorf("failed rollout: %v", err)
	}

	c, _, next = setup(testutil.NeverSettle)
	if err := c.Roll(ctx, "web", next, 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "not settled") {
		t.Errorf("timeout: %v", err)
	}
}

func TestRollRefusesOtherControllers(t *testing.T) {
	c, f, next := setup(testutil.Complete)
	f.SetController("web", types.DeploymentControllerTypeCodeDeploy)
	if err := c.Roll(context.Background(), "web", next, time.Second); err == nil || !strings.Contains(err.Error(), "CODE_DEPLOY") {
		t.Errorf("err = %v", err)
	}
}

func TestRegisterKeepsTemplateAndTags(t *testing.T) {
	f := testutil.NewECS()
	tmpl := f.AddTemplate("api", "placeholder", "app", "sidecar")
	c := &Client{API: f, Cluster: "c", UI: ui.Discard()}
	r, err := c.Register(context.Background(), tmpl, "sidecar", "sidecar:2", map[string]string{"ecsd:sha": "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Container != "sidecar" || r.Reused {
		t.Errorf("Registered = %+v", r)
	}
	if f.Image(r.ARN) != "placeholder" {
		t.Errorf("first container's image changed to %s", f.Image(r.ARN))
	}
	if tags := f.Tags(r.ARN); tags["ecsd:sha"] != "abc" || tags["team"] != "shop" {
		t.Errorf("tags = %v", tags)
	}
	if _, err := c.Register(context.Background(), tmpl, "nope", "x", nil); err == nil {
		t.Error("registered with a container the template lacks")
	}
}
