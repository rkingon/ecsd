// Package images pins image references to digests, so a deploy, its release record and a rollback to it all mean the
// same bytes whatever happens to a tag later.
package images

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/smithy-go"
)

// Ref is a parsed image reference.
type Ref struct {
	Host, Repo, Tag, Digest string
}

var (
	refPattern = regexp.MustCompile(`^([^/]+)/([^:@]+)(?::([^@]+))?(?:@(sha256:[a-f0-9]{64}))?$`)
	ecrHost    = regexp.MustCompile(`^(\d{12})\.dkr\.ecr(?:-fips)?\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?$`)
)

// Parse reads host/repo[:tag][@sha256:…]. Images without a registry host (Docker Hub shorthand) aren't accepted: a
// deploy names where its images come from.
func Parse(image string) (Ref, error) {
	m := refPattern.FindStringSubmatch(image)
	if m == nil || !strings.ContainsAny(m[1], ".:") {
		return Ref{}, fmt.Errorf("image %q: want registry-host/repository[:tag][@sha256:…]", image)
	}
	return Ref{Host: m[1], Repo: m[2], Tag: m[3], Digest: m[4]}, nil
}

func (r Ref) Pinned() string { return r.Host + "/" + r.Repo + "@" + r.Digest }

// ECR is the account and region of an ECR host, if it is one.
func (r Ref) ECR() (account, region string, ok bool) {
	m := ecrHost.FindStringSubmatch(r.Host)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

type DescribeImagesAPI interface {
	DescribeImages(context.Context, *ecr.DescribeImagesInput, ...func(*ecr.Options)) (*ecr.DescribeImagesOutput, error)
}

// Resolver pins ECR tags by asking ECR; Client returns a client for a region.
type Resolver struct {
	Client func(region string) DescribeImagesAPI

	mu    sync.Mutex
	cache map[string]string
}

// Pin returns image as host/repo@digest. ECR tags are resolved; anything else must already carry its digest, unless
// allowTags, when it's returned as it is.
func (r *Resolver) Pin(ctx context.Context, image string, allowTags bool) (string, error) {
	ref, err := Parse(image)
	if err != nil {
		return "", err
	}
	if ref.Digest != "" {
		return ref.Pinned(), nil
	}
	account, region, ok := ref.ECR()
	if !ok {
		if allowTags {
			return image, nil
		}
		return "", fmt.Errorf("image %s: not in ECR, so its tag can't be resolved; give its digest (@sha256:…) or set image_policy.unpinned", image)
	}
	r.mu.Lock()
	if d, ok := r.cache[image]; ok {
		r.mu.Unlock()
		return d, nil
	}
	r.mu.Unlock()
	tag := ref.Tag
	if tag == "" {
		tag = "latest"
	}
	out, err := r.Client(region).DescribeImages(ctx, &ecr.DescribeImagesInput{
		RegistryId: aws.String(account), RepositoryName: aws.String(ref.Repo), ImageIds: []types.ImageIdentifier{{ImageTag: aws.String(tag)}},
	})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDeniedException" {
		return "", fmt.Errorf("image %s: %w (ecsd iam-policy grants ecr:DescribeImages only on the repositories ecsd.yml names: pass a digest, or widen the role)", image, err)
	}
	if err != nil {
		return "", fmt.Errorf("image %s: %w", image, err)
	}
	if len(out.ImageDetails) != 1 || out.ImageDetails[0].ImageDigest == nil {
		return "", fmt.Errorf("image %s: not found in ECR", image)
	}
	ref.Digest = aws.ToString(out.ImageDetails[0].ImageDigest)
	pinned := ref.Pinned()
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]string{}
	}
	r.cache[image] = pinned
	r.mu.Unlock()
	return pinned, nil
}

// InRegistry reports whether image is under registry (a host, or host/path prefix).
func InRegistry(image, registry string) bool {
	return registry != "" && strings.HasPrefix(image, strings.TrimSuffix(registry, "/")+"/")
}
