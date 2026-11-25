package testutil

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// Logs serves the same lines from every stream it's asked for.
type Logs struct {
	mu    sync.Mutex
	Lines []string
	Asked []string // group/stream of each GetLogEvents
}

func (l *Logs) GetLogEvents(_ context.Context, in *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Asked = append(l.Asked, aws.ToString(in.LogGroupName)+"/"+aws.ToString(in.LogStreamName))
	out := &cloudwatchlogs.GetLogEventsOutput{NextForwardToken: aws.String("end")}
	if aws.ToString(in.NextToken) == "end" {
		return out, nil
	}
	for _, line := range l.Lines {
		out.Events = append(out.Events, cwtypes.OutputLogEvent{Message: aws.String(line)})
	}
	return out, nil
}

func (l *Logs) FilterLogEvents(_ context.Context, in *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := &cloudwatchlogs.FilterLogEventsOutput{}
	for i, line := range l.Lines {
		out.Events = append(out.Events, cwtypes.FilteredLogEvent{
			EventId: aws.String(aws.ToString(in.LogGroupName) + string(rune('a'+i))), Message: aws.String(line),
			Timestamp: aws.Int64(1_700_000_000_000 + int64(i)), LogStreamName: aws.String("ecs/app/0123456789abcdef"),
		})
	}
	return out, nil
}

func (l *Logs) StartQuery(context.Context, *cloudwatchlogs.StartQueryInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.StartQueryOutput, error) {
	return &cloudwatchlogs.StartQueryOutput{QueryId: aws.String("q")}, nil
}

func (l *Logs) GetQueryResults(context.Context, *cloudwatchlogs.GetQueryResultsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetQueryResultsOutput, error) {
	return &cloudwatchlogs.GetQueryResultsOutput{Status: cwtypes.QueryStatusComplete, Results: [][]cwtypes.ResultField{{
		{Field: aws.String("@timestamp"), Value: aws.String("t")}, {Field: aws.String("@message"), Value: aws.String("m")},
		{Field: aws.String("@ptr"), Value: aws.String("p")},
	}}}, nil
}
