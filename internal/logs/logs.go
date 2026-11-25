// Package logs reads containers' CloudWatch logs, found from their task definitions' awslogs configuration.
package logs

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

type API interface {
	GetLogEvents(context.Context, *cloudwatchlogs.GetLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
	FilterLogEvents(context.Context, *cloudwatchlogs.FilterLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error)
	StartQuery(context.Context, *cloudwatchlogs.StartQueryInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.StartQueryOutput, error)
	GetQueryResults(context.Context, *cloudwatchlogs.GetQueryResultsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetQueryResultsOutput, error)
}

// Source is where one container's logs go.
type Source struct {
	Group  string
	Prefix string // awslogs-stream-prefix/container; a task's stream appends /<task id>
}

// SourceOf reads a container's awslogs options; false when it logs elsewhere.
func SourceOf(cd *types.ContainerDefinition) (Source, bool) {
	lc := cd.LogConfiguration
	if lc == nil || lc.LogDriver != types.LogDriverAwslogs || lc.Options["awslogs-group"] == "" {
		return Source{}, false
	}
	s := Source{Group: lc.Options["awslogs-group"]}
	if p := lc.Options["awslogs-stream-prefix"]; p != "" {
		s.Prefix = p + "/" + aws.ToString(cd.Name)
	}
	return s, true
}

// Task prints one task's whole stream; it needs a stream prefix to name the stream.
func Task(ctx context.Context, api API, src Source, taskID string, w io.Writer) error {
	if src.Prefix == "" {
		return fmt.Errorf("%s has no awslogs-stream-prefix, so a task's stream can't be named", src.Group)
	}
	in := &cloudwatchlogs.GetLogEventsInput{
		LogGroupName: aws.String(src.Group), LogStreamName: aws.String(src.Prefix + "/" + taskID), StartFromHead: aws.Bool(true),
	}
	for {
		out, err := api.GetLogEvents(ctx, in)
		if err != nil {
			return err
		}
		for _, e := range out.Events {
			fmt.Fprintln(w, strings.TrimRight(aws.ToString(e.Message), "\n"))
		}
		// The forward token repeats once the stream's end is reached.
		if out.NextForwardToken == nil || aws.ToString(out.NextForwardToken) == aws.ToString(in.NextToken) {
			return nil
		}
		in.NextToken = out.NextForwardToken
	}
}

type Filter struct {
	Since   time.Duration
	Pattern string // CloudWatch filter pattern
	Follow  bool
	Poll    time.Duration
}

// Tail prints events from every source since f.Since, oldest first, and keeps polling when f.Follow is set.
func Tail(ctx context.Context, api API, sources []Source, f Filter, w io.Writer) error {
	start := time.Now().Add(-f.Since).UnixMilli()
	seen := map[string]bool{}
	for {
		var events []cwtypes.FilteredLogEvent
		for _, src := range sources {
			in := &cloudwatchlogs.FilterLogEventsInput{LogGroupName: aws.String(src.Group), StartTime: aws.Int64(start)}
			if src.Prefix != "" {
				in.LogStreamNamePrefix = aws.String(src.Prefix)
			}
			if f.Pattern != "" {
				in.FilterPattern = aws.String(f.Pattern)
			}
			p := cloudwatchlogs.NewFilterLogEventsPaginator(api, in)
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return fmt.Errorf("%s: %w", src.Group, err)
				}
				events = append(events, page.Events...)
			}
		}
		sort.SliceStable(events, func(i, j int) bool { return aws.ToInt64(events[i].Timestamp) < aws.ToInt64(events[j].Timestamp) })
		for _, e := range events {
			if id := aws.ToString(e.EventId); seen[id] {
				continue
			} else {
				seen[id] = true
			}
			at := time.UnixMilli(aws.ToInt64(e.Timestamp)).UTC().Format(time.RFC3339)
			fmt.Fprintf(w, "%s %s %s\n", at, streamTask(aws.ToString(e.LogStreamName)), strings.TrimRight(aws.ToString(e.Message), "\n"))
			start = max(start, aws.ToInt64(e.Timestamp))
		}
		if !f.Follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(f.Poll):
		}
	}
}

// streamTask shortens prefix/container/<task id> to container/<first 8 of the id>.
func streamTask(stream string) string {
	parts := strings.Split(stream, "/")
	if len(parts) >= 3 {
		id := parts[len(parts)-1]
		return parts[len(parts)-2] + "/" + id[:min(8, len(id))]
	}
	return stream
}

// Insights runs a Logs Insights query over groups and prints its rows as tab-separated fields.
func Insights(ctx context.Context, api API, groups []string, query string, since time.Duration, w io.Writer) error {
	now := time.Now()
	out, err := api.StartQuery(ctx, &cloudwatchlogs.StartQueryInput{
		LogGroupNames: groups, QueryString: aws.String(query),
		StartTime: aws.Int64(now.Add(-since).Unix()), EndTime: aws.Int64(now.Unix()),
	})
	if err != nil {
		return err
	}
	for {
		res, err := api.GetQueryResults(ctx, &cloudwatchlogs.GetQueryResultsInput{QueryId: out.QueryId})
		if err != nil {
			return err
		}
		switch res.Status {
		case cwtypes.QueryStatusComplete:
			for _, row := range res.Results {
				var fields []string
				for _, f := range row {
					if aws.ToString(f.Field) != "@ptr" {
						fields = append(fields, aws.ToString(f.Value))
					}
				}
				fmt.Fprintln(w, strings.Join(fields, "\t"))
			}
			return nil
		case cwtypes.QueryStatusFailed, cwtypes.QueryStatusCancelled, cwtypes.QueryStatusTimeout:
			return fmt.Errorf("query %s", res.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
