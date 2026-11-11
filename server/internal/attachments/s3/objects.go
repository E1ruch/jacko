package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"jacko/server/internal/attachments"
)

type Objects struct {
	client *minio.Client
	bucket string
}

func New(endpoint, region, bucket, accessKey, secretKey string) (*Objects, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" ||
		region == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("invalid S3 configuration")
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       u.Scheme == "https",
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize S3 client: %w", err)
	}
	return &Objects{client: client, bucket: bucket}, nil
}

func (s *Objects) SignUpload(ctx context.Context, key, mediaType string, size int64, expiresAt time.Time) (string, map[string]string, error) {
	if size < 1 || size > attachments.MaxBytes {
		return "", nil, attachments.ErrInvalid
	}
	policy := minio.NewPostPolicy()
	for _, err := range []error{
		policy.SetBucket(s.bucket),
		policy.SetKey(key),
		policy.SetExpires(expiresAt),
		policy.SetContentType(mediaType),
		policy.SetContentLengthRange(size, size),
	} {
		if err != nil {
			return "", nil, storageError("create upload policy", err)
		}
	}
	u, fields, err := s.client.PresignedPostPolicy(ctx, policy)
	if err != nil {
		return "", nil, storageError("sign upload", err)
	}
	return u.String(), fields, nil
}

func (s *Objects) ReadUpload(ctx context.Context, key string, expectedSize int64) ([]byte, error) {
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, readError(err)
	}
	defer object.Close()
	data, err := io.ReadAll(io.LimitReader(object, expectedSize+1))
	if err != nil {
		return nil, readError(err)
	}
	if int64(len(data)) != expectedSize {
		return nil, attachments.ErrIncomplete
	}
	return data, nil
}

func (s *Objects) Save(ctx context.Context, key, mediaType string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType:  mediaType,
		CacheControl: "private, no-store",
	})
	if err != nil {
		return storageError("save verified image", err)
	}
	return nil
}

func (s *Objects) Remove(ctx context.Context, key string) {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		slog.Warn("remove unused image object failed", "error", err)
	}
}

func (s *Objects) SignDownload(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, nil)
	if err != nil {
		return "", storageError("sign download", err)
	}
	return u.String(), nil
}

func readError(err error) error {
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return attachments.ErrIncomplete
	}
	return storageError("read uploaded image", err)
}

func storageError(operation string, err error) error {
	return fmt.Errorf("%s: %w: %w", operation, attachments.ErrStorage, err)
}
