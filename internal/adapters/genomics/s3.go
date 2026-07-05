// s3.go — S3/MinIO backend helpers for the genomics adapter.
// Requires: github.com/aws/aws-sdk-go-v2 (go get github.com/aws/aws-sdk-go-v2/...)
// Credentials: AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY env vars, or IAM instance role.
// For MinIO: set endpoint in genomics.endpoint
package genomics

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3Object is a lightweight S3 object listing entry.
type s3Object struct {
	Key  string
	Size int64
}

// newS3Client creates an S3 client, optionally pointing to a custom endpoint (MinIO).
func newS3Client(ctx context.Context, endpoint, region string) (*s3.Client, error) {
	opts := []func(*config.LoadOptions) error{}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	} else {
		opts = append(opts, config.WithRegion("us-east-1")) // default, required by SDK
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("S3 config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true // required for MinIO
		})
	}
	return s3.NewFromConfig(cfg, clientOpts...), nil
}

// listS3Objects pages through a bucket listing and returns all matching objects.
func listS3Objects(ctx context.Context, client *s3.Client, bucket, prefix string) ([]s3Object, error) {
	var objects []s3Object
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("S3 list objects (bucket=%s, prefix=%s): %w", bucket, prefix, err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, s3Object{
				Key:  aws.ToString(obj.Key),
				Size: aws.ToInt64(obj.Size),
			})
		}
	}
	return objects, nil
}

// findS3Index checks whether an index object exists in S3/MinIO.
func findS3Index(ctx context.Context, client *s3.Client, bucket, objKey, endpoint, format string) (string, error) {
	candidates, ok := indexExts[format]
	if !ok {
		return "", nil
	}

	base := objKey
	for _, ext := range extMap[format] {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}

	for _, idxExt := range candidates {
		candidate := base + idxExt
		_, err := client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(candidate),
		})
		if err == nil {
			if endpoint != "" {
				return fmt.Sprintf("%s/%s/%s", strings.TrimRight(endpoint, "/"), bucket, candidate), nil
			}
			return fmt.Sprintf("s3://%s/%s", bucket, candidate), nil
		}
	}
	return "", fmt.Errorf("index not found in S3 for s3://%s/%s (tried %v)", bucket, objKey, candidates)
}
