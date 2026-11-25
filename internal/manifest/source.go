package manifest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type GetParameterAPI interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// Fetch reads a manifest from ref: ssm:/path (or a bare /path) or file:path.
func Fetch(ctx context.Context, client GetParameterAPI, ref string) (*Manifest, error) {
	if path, ok := strings.CutPrefix(ref, "file:"); ok {
		return Load(path)
	}
	name := strings.TrimPrefix(ref, "ssm:")
	if !strings.HasPrefix(name, "/") {
		return nil, fmt.Errorf("manifest %q: want ssm:/path, /path or file:path", ref)
	}
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
	if err != nil {
		var nf *types.ParameterNotFound
		if errors.As(err, &nf) {
			return nil, fmt.Errorf("manifest %s: no such parameter (has the stage's infrastructure been applied?)", name)
		}
		return nil, fmt.Errorf("manifest %s: %w", name, err)
	}
	m, err := Parse([]byte(aws.ToString(out.Parameter.Value)))
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", name, err)
	}
	return m, nil
}
