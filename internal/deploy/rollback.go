package deploy

import (
	"context"
	"fmt"
	"time"

	"github.com/rkingon/ecsd/internal/release"
)

type RollbackOptions struct {
	To        string // a commit (7+ characters) or v<version>; empty for the release before the current one
	SkipGates bool
}

// Rollback returns the stage to a recorded release. Every family gets a copy of the release's revision on top, so its
// newest revision is the release's again, and the services roll onto those copies. No release task runs: a schema stays
// migrated, which is why migrations must be backward compatible.
func (e *Env) Rollback(ctx context.Context, opts RollbackOptions) (*release.Release, error) {
	cur, err := e.Store.Current(ctx)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("%s has no release to roll back from", e.Stage)
	}
	history, err := e.Store.History(ctx, 0)
	if err != nil {
		return nil, err
	}
	var target *release.Release
	if opts.To == "" {
		target, err = release.RollbackTarget(history, cur)
	} else {
		target, err = release.Find(history, opts.To)
	}
	if err != nil {
		return nil, err
	}
	if target.SHA == cur.SHA {
		return nil, fmt.Errorf("%s already runs %s", e.Stage, short(cur.SHA))
	}
	e.UI.Step("rolling %s back from %s to %s (release version %d)", e.Stage, short(cur.SHA), short(target.SHA), target.Version)

	if err := e.verifyImages(ctx, target.Images, target.SHA); err != nil {
		return nil, err
	}
	for _, name := range e.Manifest.Names() {
		if target.Revisions[name] == "" {
			return nil, fmt.Errorf("release version %d has no revision for %s, which the manifest now has", target.Version, name)
		}
	}
	regs, err := e.register(ctx, e.Manifest.Names(), &Plan{Holds: map[string]string{}}, target.SHA, target.Revisions)
	if err != nil {
		return nil, err
	}
	if !opts.SkipGates {
		if err := e.gates(ctx, target.SHA); err != nil {
			return nil, err
		}
	}
	revisions := arns(regs)
	if err := e.rollAndCheck(ctx, revisions, target.SHA); err != nil {
		return nil, err
	}
	rel := &release.Release{
		SHA: target.SHA, Kind: release.Rollback, From: cur.SHA, Revisions: revisions, Images: target.Images,
		At: time.Now().UTC(), By: e.Actor,
	}
	if err := e.Store.Record(ctx, rel); err != nil {
		return nil, err
	}
	e.summarize("Rolled back", rel)
	return rel, nil
}
