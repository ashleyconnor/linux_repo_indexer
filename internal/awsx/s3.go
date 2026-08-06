// Package awsx holds the thin adapters between this service's interfaces and
// the AWS SDK. Everything with interesting behaviour lives elsewhere and is
// tested without AWS; what is here is plumbing.
package awsx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/ingest"
)

// digestMetadata records an object's SHA256 so a republish can tell unchanged
// content from changed content without downloading it. ETag would nearly do,
// but it is only an MD5 for single-part uploads.
const digestMetadata = "content-sha256"

// deleteBatchSize is S3's maximum for DeleteObjects.
const deleteBatchSize = 1000

// S3 wraps the bucket holding both packages and published indexes.
type S3 struct {
	Client *s3.Client
	Bucket string
	Log    *slog.Logger
}

func (c *S3) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Get fetches an object for ingest.
func (c *S3) Get(ctx context.Context, key string) (io.ReadCloser, ingest.ObjectInfo, error) {
	out, err := c.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, ingest.ObjectInfo{}, fmt.Errorf("awsx: getting s3://%s/%s: %w", c.Bucket, key, err)
	}

	info := ingest.ObjectInfo{}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}
	return out.Body, info, nil
}

// Put writes artifacts, skipping any whose content is already in place.
func (c *S3) Put(ctx context.Context, artifacts []index.Artifact) error {
	for _, a := range artifacts {
		digest := index.SHA256Hex(a.Body)

		unchanged, err := c.matches(ctx, a.Path, digest)
		if err != nil {
			return err
		}
		if unchanged {
			continue
		}

		if _, err := c.Client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(c.Bucket),
			Key:         aws.String(a.Path),
			Body:        bytes.NewReader(a.Body),
			ContentType: aws.String(a.ContentType),
			Metadata:    map[string]string{digestMetadata: digest},
		}); err != nil {
			return fmt.Errorf("awsx: putting s3://%s/%s: %w", c.Bucket, a.Path, err)
		}
	}
	return nil
}

// matches reports whether the object already holds exactly this content.
func (c *S3) matches(ctx context.Context, key, digest string) (bool, error) {
	out, err := c.Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("awsx: heading s3://%s/%s: %w", c.Bucket, key, err)
	}
	return out.Metadata[digestMetadata] == digest, nil
}

// PutBlob stores an oversized package record.
func (c *S3) PutBlob(ctx context.Context, key string, body []byte) error {
	_, err := c.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/gzip"),
	})
	if err != nil {
		return fmt.Errorf("awsx: putting blob s3://%s/%s: %w", c.Bucket, key, err)
	}
	return nil
}

// GetBlob reads an oversized package record.
func (c *S3) GetBlob(ctx context.Context, key string) ([]byte, error) {
	out, err := c.Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("awsx: getting blob s3://%s/%s: %w", c.Bucket, key, err)
	}
	defer out.Body.Close()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("awsx: reading blob s3://%s/%s: %w", c.Bucket, key, err)
	}
	return body, nil
}

// Prune deletes objects under dir that are neither current nor recent.
//
// Both conditions matter. A client reads Release or repomd.xml and then
// fetches the files it names; deleting a superseded file the instant it stops
// being current would break a refresh that is already in flight. The age
// check gives those clients a grace period.
func (c *S3) Prune(ctx context.Context, dir string, keep map[string]bool, cutoff time.Time) error {
	prefix := strings.TrimSuffix(dir, "/") + "/"

	var (
		stale   []s3types.ObjectIdentifier
		token   *string
		deleted int
	)

	for {
		page, err := c.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(c.Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("awsx: listing s3://%s/%s: %w", c.Bucket, prefix, err)
		}

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if keep[key] {
				continue
			}
			if obj.LastModified != nil && obj.LastModified.After(cutoff) {
				continue
			}
			stale = append(stale, s3types.ObjectIdentifier{Key: obj.Key})
		}

		if !aws.ToBool(page.IsTruncated) {
			break
		}
		token = page.NextContinuationToken
	}

	for chunk := range slicesChunk(stale, deleteBatchSize) {
		out, err := c.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.Bucket),
			Delete: &s3types.Delete{Objects: chunk, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("awsx: pruning s3://%s/%s: %w", c.Bucket, prefix, err)
		}
		if len(out.Errors) > 0 {
			return fmt.Errorf("awsx: pruning s3://%s/%s: %d objects failed, first: %s",
				c.Bucket, prefix, len(out.Errors), aws.ToString(out.Errors[0].Message))
		}
		deleted += len(chunk)
	}

	if deleted > 0 {
		c.log().Info("pruned superseded objects", "prefix", prefix, "deleted", deleted)
	}
	return nil
}

// GetConfig reads the repository definition from the bucket.
func (c *S3) GetConfig(ctx context.Context, key string) ([]byte, error) {
	return c.GetBlob(ctx, key)
}

func isNotFound(err error) bool {
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}

// slicesChunk yields fixed-size windows over s.
func slicesChunk[T any](s []T, size int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for start := 0; start < len(s); start += size {
			end := min(start+size, len(s))
			if !yield(s[start:end]) {
				return
			}
		}
	}
}
