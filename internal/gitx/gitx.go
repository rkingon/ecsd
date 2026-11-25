// Package gitx answers the questions a deploy asks of the repository: is this commit behind the released one, and what
// changed since.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type Repo struct{ Dir string }

func (r Repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Resolve is rev's full commit hash.
func (r Repo) Resolve(rev string) (string, error) {
	return r.git("rev-parse", "--verify", rev+"^{commit}")
}

// Has reports whether the commit is in the repository; a shallow clone often lacks the released one.
func (r Repo) Has(sha string) bool {
	_, err := r.git("cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// IsAncestor reports whether a is b or one of b's ancestors.
func (r Repo) IsAncestor(a, b string) (bool, error) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", a, b)
	cmd.Dir = r.Dir
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	default:
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", a, b, err)
	}
}

// Changed lists the files that differ between two commits.
func (r Repo) Changed(from, to string) ([]string, error) {
	out, err := r.git("diff", "--name-only", from, to)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// Matches applies gitignore-style patterns in order: the last that matches decides, and !pattern excludes. When the
// first pattern excludes, everything else starts included, so ["!docs/**"] means "anything but docs".
func Matches(path string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	included := strings.HasPrefix(patterns[0], "!")
	for _, p := range patterns {
		negate := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		if ok, _ := doublestar.Match(p, path); ok {
			included = !negate
		}
	}
	return included
}

// AnyMatches is the first path the patterns include, if any.
func AnyMatches(paths, patterns []string) (string, bool) {
	for _, p := range paths {
		if Matches(p, patterns) {
			return p, true
		}
	}
	return "", false
}
