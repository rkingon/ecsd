// Package release records what a stage runs. A release is one SSM parameter whose versions are the history: SSM keeps
// the latest 100, which is every rollback target anyone will want.
package release

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type Kind string

const (
	Deploy   Kind = "deploy"
	Rollback Kind = "rollback"
)

type Release struct {
	SHA       string            `json:"sha"`
	Kind      Kind              `json:"kind"`
	From      string            `json:"from,omitempty"` // a rollback's: the commit it replaced
	Revisions map[string]string `json:"revisions"`      // service or task name → task definition ARN
	Images    map[string]string `json:"images"`
	At        time.Time         `json:"at"`
	By        string            `json:"by,omitempty"`

	Version int64 `json:"-"` // the parameter version that holds it
}

type API interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	GetParameterHistory(context.Context, *ssm.GetParameterHistoryInput, ...func(*ssm.Options)) (*ssm.GetParameterHistoryOutput, error)
	PutParameter(context.Context, *ssm.PutParameterInput, ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
}

type Store struct {
	API       API
	Parameter string
}

// Current is the latest release, or nil before the first.
func (s *Store) Current(ctx context.Context) (*Release, error) {
	out, err := s.API.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(s.Parameter)})
	if err != nil {
		var nf *types.ParameterNotFound
		if errors.As(err, &nf) {
			return nil, nil
		}
		return nil, fmt.Errorf("release %s: %w", s.Parameter, err)
	}
	return decode(aws.ToString(out.Parameter.Value), out.Parameter.Version)
}

// History is up to limit releases, newest first.
func (s *Store) History(ctx context.Context, limit int) ([]Release, error) {
	var all []Release
	p := ssm.NewGetParameterHistoryPaginator(s.API, &ssm.GetParameterHistoryInput{Name: aws.String(s.Parameter)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			var nf *types.ParameterNotFound
			if errors.As(err, &nf) {
				return nil, nil
			}
			return nil, fmt.Errorf("release history %s: %w", s.Parameter, err)
		}
		for _, h := range page.Parameters {
			r, err := decode(aws.ToString(h.Value), h.Version)
			if err != nil {
				return nil, err
			}
			all = append(all, *r)
		}
	}
	slices.SortFunc(all, func(a, b Release) int { return cmp.Compare(b.Version, a.Version) })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (s *Store) Record(ctx context.Context, r *Release) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	out, err := s.API.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(s.Parameter),
		Value:     aws.String(string(b)),
		Type:      types.ParameterTypeString,
		Overwrite: aws.Bool(true),
		// Standard holds 4 KB; a stage with many services can outgrow it, and only then is it billed as advanced.
		Tier: types.ParameterTierIntelligentTiering,
	})
	if err != nil {
		return fmt.Errorf("record release in %s: %w", s.Parameter, err)
	}
	r.Version = out.Version
	return nil
}

func decode(v string, version int64) (*Release, error) {
	var r Release
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return nil, fmt.Errorf("release version %d: %w", version, err)
	}
	r.Version = version
	return &r, nil
}

// RolledBackFrom is the rollback away from sha, when nothing has released sha since: a commit that shouldn't ship
// again without someone deciding it should. history is newest first.
func RolledBackFrom(history []Release, sha string) (*Release, bool) {
	for i := range history {
		r := &history[i]
		if r.SHA == sha {
			return nil, false
		}
		if r.Kind == Rollback && r.From == sha {
			return r, true
		}
	}
	return nil, false
}

// RollbackTarget is the newest release that isn't current's commit and wasn't rolled back from since.
func RollbackTarget(history []Release, current *Release) (*Release, error) {
	if current == nil {
		return nil, errors.New("nothing released yet")
	}
	for i := range history {
		r := &history[i]
		if r.SHA == current.SHA {
			continue
		}
		if _, bad := RolledBackFrom(history[:i], r.SHA); !bad {
			return r, nil
		}
	}
	return nil, errors.New("no earlier release to roll back to")
}

// Find is the newest release of sha (a prefix of 7 or more characters matches), or the release with version v when
// ref is "v<n>".
func Find(history []Release, ref string) (*Release, error) {
	var version int64
	if _, err := fmt.Sscanf(ref, "v%d", &version); err == nil {
		for i := range history {
			if history[i].Version == version {
				return &history[i], nil
			}
		}
		return nil, fmt.Errorf("no release version %d in the history", version)
	}
	if len(ref) < 7 {
		return nil, fmt.Errorf("%q: give 7 or more characters of a commit, or a version as v<n>", ref)
	}
	for i := range history {
		if len(history[i].SHA) >= len(ref) && history[i].SHA[:len(ref)] == ref {
			return &history[i], nil
		}
	}
	return nil, fmt.Errorf("no release of %s in the history", ref)
}
