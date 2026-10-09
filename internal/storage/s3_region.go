package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// RegionLocator is implemented by drivers that can tell the region of their
// bucket (S3 GetBucketLocation).
type RegionLocator interface {
	// BucketRegion returns the region of the bucket, lower-cased ("" when storage
	// does not tell).
	BucketRegion(ctx context.Context) (string, error)
}

// BucketRegion returns the region of the bucket from GetBucketLocation (needs
// s3:GetBucketLocation). AWS reports us-east-1 as an empty constraint and the
// legacy EU region as "EU": both are mapped to their region names.
func (s *S3Storage) BucketRegion(ctx context.Context) (string, error) {
	out, err := s.client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		return "", fmt.Errorf("read the location of bucket %s (needs s3:GetBucketLocation): %w", s.bucket, err)
	}
	return normalizeBucketLocation(string(out.LocationConstraint), s.client.Options().BaseEndpoint == nil), nil
}

// normalizeBucketLocation maps a LocationConstraint to a region name: on AWS
// (aws true) "" is us-east-1 and "EU" is eu-west-1; elsewhere "" stays unknown.
func normalizeBucketLocation(constraint string, aws bool) string {
	c := strings.ToLower(strings.TrimSpace(constraint))
	switch {
	case c == "" && aws:
		return "us-east-1"
	case c == "eu":
		return "eu-west-1"
	}
	return c
}
