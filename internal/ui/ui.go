// Package ui writes progress to stderr, as GitHub Actions annotations, groups, step summary and outputs when it runs
// there, and as plain lines anywhere else.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

type UI struct {
	mu      sync.Mutex
	out     io.Writer
	actions bool
	summary string
	outputs string
}

// New detects GitHub Actions from its environment.
func New(out io.Writer) *UI {
	return &UI{
		out:     out,
		actions: os.Getenv("GITHUB_ACTIONS") == "true",
		summary: os.Getenv("GITHUB_STEP_SUMMARY"),
		outputs: os.Getenv("GITHUB_OUTPUT"),
	}
}

// Discard is for tests.
func Discard() *UI { return &UI{out: io.Discard} }

func (u *UI) Printf(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintf(u.out, format+"\n", a...)
}

func (u *UI) Step(format string, a ...any) { u.Printf("==> "+format, a...) }

func (u *UI) Warnf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if u.actions {
		u.Printf("::warning::%s", escape(msg))
		return
	}
	u.Printf("warning: %s", msg)
}

func (u *UI) Errorf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if u.actions {
		u.Printf("::error::%s", escape(msg))
		return
	}
	u.Printf("error: %s", msg)
}

// Group folds the lines fn prints under title in Actions' log.
func (u *UI) Group(title string, fn func() error) error {
	if u.actions {
		u.Printf("::group::%s", title)
		defer u.Printf("::endgroup::")
	} else {
		u.Step("%s", title)
	}
	return fn()
}

// Summary appends Markdown to the step summary; outside Actions it's dropped.
func (u *UI) Summary(format string, a ...any) {
	u.appendTo(u.summary, fmt.Sprintf(format, a...)+"\n")
}

// Output sets a step output.
func (u *UI) Output(name, value string) {
	u.appendTo(u.outputs, name+"="+value+"\n")
}

func (u *UI) appendTo(path, s string) {
	if path == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(u.out, "warning: %v\n", err)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(s)
}

// escape is a workflow command's data encoding: a multi-line message stays one annotation.
func escape(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}
