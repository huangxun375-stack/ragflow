package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/minio/minio-go/v7"
)

var ErrObjectTooLarge = errors.New("storage object exceeds read limit")

type boundedGetter interface {
	GetBounded(context.Context, string, string, int64) ([]byte, error)
}

// ReadBounded reads the original stored object without materializing an
// unbounded response. Backends must explicitly support bounded streaming.
func ReadBounded(ctx context.Context, backend Storage, bucket, key string, limit int64) ([]byte, error) {
	if err := checkReadBound(ctx, limit); err != nil {
		return nil, err
	}
	reader, ok := backend.(boundedGetter)
	if !ok {
		return nil, fmt.Errorf("storage backend %T does not support bounded reads", backend)
	}
	return reader.GetBounded(ctx, bucket, key, limit)
}

func checkReadBound(ctx context.Context, limit int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if limit < 0 || limit == math.MaxInt64 {
		return errors.New("bounded object read requires a nonnegative finite limit")
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// The extra byte detects overflow even if object metadata lies or a backend
// does not supply Content-Length. No partial contents escape on failure.
func readBoundedBody(ctx context.Context, body io.ReadCloser, limit int64) ([]byte, error) {
	if body == nil {
		return nil, errors.New("storage returned no object body")
	}
	defer body.Close()
	if err := checkReadBound(ctx, limit); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: body}, limit+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrObjectTooLarge
	}
	return data, nil
}

// GetBounded is promoted by both S3Storage and OSSStorage.
func (b *s3Base) GetBounded(ctx context.Context, bucket, key string, limit int64) ([]byte, error) {
	if err := checkReadBound(ctx, limit); err != nil {
		return nil, err
	}
	if b == nil || b.client == nil || b.resolve == nil {
		return nil, errors.New("S3 storage is not initialized")
	}
	client := b.client()
	if client == nil {
		return nil, errors.New("S3 storage is not initialized")
	}
	bucket, key = b.resolve(bucket, key)
	result, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	return readBoundedBody(ctx, result.Body, limit)
}

func (m *MinioStorage) GetBounded(ctx context.Context, bucket, key string, limit int64) ([]byte, error) {
	if err := checkReadBound(ctx, limit); err != nil {
		return nil, err
	}
	if m == nil || m.client == nil {
		return nil, errors.New("MinIO storage is not initialized")
	}
	bucket, key = m.resolveBucketAndPath(bucket, key)
	body, err := m.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return readBoundedBody(ctx, body, limit)
}

func (m *GCSStorage) GetBounded(ctx context.Context, bucket, key string, limit int64) ([]byte, error) {
	if err := checkReadBound(ctx, limit); err != nil {
		return nil, err
	}
	if m == nil || m.client == nil {
		return nil, errors.New("GCS storage is not initialized")
	}
	body, err := m.client.Bucket(bucket).Object(key).NewReader(ctx)
	if err != nil {
		return nil, err
	}
	return readBoundedBody(ctx, body, limit)
}

func (m *MemoryStorage) GetBounded(ctx context.Context, bucket, key string, limit int64) ([]byte, error) {
	if err := checkReadBound(ctx, limit); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("memory storage is not initialized")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := m.objects[bucket][key]
	if !ok {
		return nil, ErrMemoryNotFound
	}
	if int64(len(data)) > limit {
		return nil, ErrObjectTooLarge
	}
	copyOfData := make([]byte, len(data))
	copy(copyOfData, data)
	return copyOfData, nil
}
