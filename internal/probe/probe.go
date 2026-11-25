// Package probe makes the HTTP checks that end a deploy: health endpoints that must report the deployed commit, then
// smoke tests.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Check struct {
	Name     string
	URL      string
	Method   string
	Headers  map[string]string
	Body     string
	Expect   int
	Contains string
	Revision string // dot path that must hold the commit
	Attempts int
	Interval time.Duration
	// Streak is how many answers in a row must pass; a health check wants more than one, so a load balancer still
	// sending some requests to an old task can't pass it.
	Streak int
}

type Prober struct {
	HTTP *http.Client
	Log  func(format string, a ...any)
}

// Run makes the check until it passes Streak times in a row, or its attempts run out.
func (p *Prober) Run(ctx context.Context, c Check, sha string) error {
	attempts, interval, streak := max(c.Attempts, 1), c.Interval, max(c.Streak, 1)
	name := c.Name
	if name == "" {
		name = strings.TrimSpace(c.Method + " " + c.URL)
	}
	var last error
	passed := 0
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
		if last = p.once(ctx, c, sha); last == nil {
			passed++
			if passed == streak {
				p.Log("%s: ok", name)
				return nil
			}
			continue
		}
		passed = 0
	}
	return fmt.Errorf("%s: %w", name, last)
}

func (p *Prober) once(ctx context.Context, c Check, sha string) error {
	method := c.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if c.Body != "" {
		body = strings.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL, body)
	if err != nil {
		return err
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "ecsd")
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	want := c.Expect
	if want == 0 {
		want = http.StatusOK
	}
	if resp.StatusCode != want {
		return fmt.Errorf("answered %d, not %d", resp.StatusCode, want)
	}
	if c.Contains != "" && !strings.Contains(string(b), c.Contains) {
		return fmt.Errorf("answered %d without %q", resp.StatusCode, c.Contains)
	}
	if c.Revision != "" {
		got, err := lookup(b, c.Revision)
		if err != nil {
			return err
		}
		if !SameCommit(got, sha) {
			return fmt.Errorf("reports %s %q, not %s", c.Revision, got, sha)
		}
	}
	return nil
}

// SameCommit allows either to be an abbreviation of the other, of 7 or more characters.
func SameCommit(a, b string) bool {
	if len(a) < 7 || len(b) < 7 {
		return false
	}
	n := min(len(a), len(b))
	return strings.EqualFold(a[:n], b[:n])
}

// lookup follows a dot path (.revision, .build.sha) into a JSON body.
func lookup(body []byte, path string) (string, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("body isn't JSON: %w", err)
	}
	for _, key := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		obj, ok := v.(map[string]any)
		if !ok {
			return "", fmt.Errorf("no %s in the body", path)
		}
		if v, ok = obj[key]; !ok {
			return "", fmt.Errorf("no %s in the body", path)
		}
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s isn't a string", path)
	}
	return s, nil
}
