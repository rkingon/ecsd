// Package testutil has in-memory fakes of the AWS APIs ecsd calls.
package testutil

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// SSM keeps every version of every parameter.
type SSM struct {
	mu     sync.Mutex
	params map[string][]string
}

func NewSSM() *SSM { return &SSM{params: map[string][]string{}} }

func (s *SSM) Set(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.params[name] = append(s.params[name], value)
}

func (s *SSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.params[aws.ToString(in.Name)]
	if len(vs) == 0 {
		return nil, &types.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &types.Parameter{
		Name: in.Name, Value: aws.String(vs[len(vs)-1]), Version: int64(len(vs)),
	}}, nil
}

func (s *SSM) GetParameterHistory(_ context.Context, in *ssm.GetParameterHistoryInput, _ ...func(*ssm.Options)) (*ssm.GetParameterHistoryOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.params[aws.ToString(in.Name)]
	if len(vs) == 0 {
		return nil, &types.ParameterNotFound{}
	}
	out := &ssm.GetParameterHistoryOutput{}
	for i, v := range vs {
		out.Parameters = append(out.Parameters, types.ParameterHistory{Name: in.Name, Value: aws.String(v), Version: int64(i + 1)})
	}
	return out, nil
}

func (s *SSM) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := aws.ToString(in.Name)
	s.params[name] = append(s.params[name], aws.ToString(in.Value))
	return &ssm.PutParameterOutput{Version: int64(len(s.params[name]))}, nil
}

func (s *SSM) StartSession(_ context.Context, in *ssm.StartSessionInput, _ ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	return &ssm.StartSessionOutput{SessionId: aws.String("s-1"), StreamUrl: aws.String("wss://example"), TokenValue: aws.String("token")}, nil
}
