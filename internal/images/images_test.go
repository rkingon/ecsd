package images

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/smithy-go"
)

const digest = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

type fakeECR struct {
	region string
	calls  int
}

func (f *fakeECR) DescribeImages(_ context.Context, in *ecr.DescribeImagesInput, _ ...func(*ecr.Options)) (*ecr.DescribeImagesOutput, error) {
	f.calls++
	if aws.ToString(in.RepositoryName) == "other/repo" {
		return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized"}
	}
	if aws.ToString(in.RepositoryName) != "shop/api" || aws.ToString(in.ImageIds[0].ImageTag) != "abc" || aws.ToString(in.RegistryId) != "123456789012" {
		return &ecr.DescribeImagesOutput{}, nil
	}
	return &ecr.DescribeImagesOutput{ImageDetails: []types.ImageDetail{{ImageDigest: aws.String(digest)}}}, nil
}

func TestPin(t *testing.T) {
	f := &fakeECR{}
	r := &Resolver{Client: func(region string) DescribeImagesAPI { f.region = region; return f }}
	ctx := context.Background()
	host := "123456789012.dkr.ecr.eu-west-1.amazonaws.com"

	got, err := r.Pin(ctx, host+"/shop/api:abc", false)
	if err != nil || got != host+"/shop/api@"+digest || f.region != "eu-west-1" {
		t.Errorf("Pin(ecr tag) = %q, %v (region %s)", got, err, f.region)
	}
	if _, _ = r.Pin(ctx, host+"/shop/api:abc", false); f.calls != 1 {
		t.Errorf("resolved the same tag %d times", f.calls)
	}
	if _, err := r.Pin(ctx, host+"/shop/api:missing", false); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Pin(missing tag) err = %v", err)
	}
	if _, err := r.Pin(ctx, host+"/other/repo:abc", false); err == nil || !strings.Contains(err.Error(), "pass a digest") {
		t.Errorf("Pin(denied repo) err = %v", err)
	}
	if got, err := r.Pin(ctx, "ghcr.io/o/app:1.2@"+digest, false); err != nil || got != "ghcr.io/o/app@"+digest {
		t.Errorf("Pin(digest) = %q, %v", got, err)
	}
	if _, err := r.Pin(ctx, "ghcr.io/o/app:1.2", false); err == nil {
		t.Error("pinned a non-ECR tag without a digest")
	}
	if got, err := r.Pin(ctx, "ghcr.io/o/app:1.2", true); err != nil || got != "ghcr.io/o/app:1.2" {
		t.Errorf("Pin(unpinned allowed) = %q, %v", got, err)
	}
	if _, err := r.Pin(ctx, "nginx:latest", true); err == nil {
		t.Error("accepted an image without a registry host")
	}
}

func TestInRegistry(t *testing.T) {
	if !InRegistry("reg.example/shop/api@x", "reg.example") || InRegistry("reg.example.evil/x", "reg.example") || InRegistry("x/y", "") {
		t.Error("InRegistry")
	}
}
