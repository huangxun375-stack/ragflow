package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

func TestReadBoundedMemoryRejectsOversizeAndCopiesAcceptedData(t *testing.T) {
	backend := NewMemoryStorage()
	if err := backend.Put(t.Context(), "bucket", "key", []byte("12345")); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadBounded(t.Context(), backend, "bucket", "key", 4); !errors.Is(err, ErrObjectTooLarge) || data != nil {
		t.Fatalf("oversize: %q %v", data, err)
	}
	data, err := ReadBounded(t.Context(), backend, "bucket", "key", 5)
	if err != nil || string(data) != "12345" {
		t.Fatal(string(data), err)
	}
	data[0] = 'x'
	original, err := backend.Get(t.Context(), "bucket", "key")
	if err != nil || string(original) != "12345" {
		t.Fatal("bounded read exposed backing slice")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = ReadBounded(ctx, backend, "bucket", "key", 5); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type onlyUnboundedStorage struct {
	Storage
	called bool
}

func (s *onlyUnboundedStorage) Get(context.Context, string, string, ...string) ([]byte, error) {
	s.called = true
	return []byte("unbounded"), nil
}
func TestReadBoundedDoesNotFallbackToUnboundedBackend(t *testing.T) {
	backend := &onlyUnboundedStorage{}
	if _, err := ReadBounded(t.Context(), backend, "bucket", "key", 3); err == nil {
		t.Fatal("unsupported bounded storage accepted")
	}
	if backend.called {
		t.Fatal("fell back to unbounded Get")
	}
}

type measuredBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *measuredBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *measuredBody) Close() error { b.closed = true; return nil }

func TestReadBoundedStreamLimitsActualReadsAndCloses(t *testing.T) {
	body := &measuredBody{Reader: strings.NewReader(strings.Repeat("x", 1000))}
	data, err := readBoundedBody(t.Context(), body, 9)
	if !errors.Is(err, ErrObjectTooLarge) || data != nil || body.read != 10 || !body.closed {
		t.Fatalf("read=%d closed=%v data=%q err=%v", body.read, body.closed, data, err)
	}
}

func TestS3BoundedReaderDoesNotTrustSmallContentLength(t *testing.T) {
	body := &measuredBody{Reader: strings.NewReader(strings.Repeat("x", 100))}
	base := newBareS3Base(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: 2, Request: req}, nil
	}, "s3")
	if data, err := base.GetBounded(t.Context(), "bucket", "key", 8); !errors.Is(err, ErrObjectTooLarge) || data != nil {
		t.Fatalf("oversize=%q %v", data, err)
	}
	if !body.closed || body.read > 9 {
		t.Fatalf("read=%d closed=%v", body.read, body.closed)
	}
}

func TestMinioBoundedReaderUsesRealHTTPStream(t *testing.T) {
	backend := newMinioHealthTestStorage(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bucket/key" {
			t.Errorf("unexpected object path %s", r.URL.Path)
		}
		w.Header().Set("Content-Length", "100")
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		w.Header().Set("ETag", `"object-etag"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.Repeat("x", 100))
	})
	if data, err := ReadBounded(t.Context(), backend, "bucket", "key", 8); !errors.Is(err, ErrObjectTooLarge) || data != nil {
		t.Fatalf("oversize=%q %v", data, err)
	}
}

func TestGCSBoundedReaderClosesResponseWithoutDraining(t *testing.T) {
	body := &measuredBody{Reader: strings.NewReader(strings.Repeat("x", 100))}
	client, err := gcs.NewClient(t.Context(), option.WithoutAuthentication(), option.WithHTTPClient(&http.Client{Transport: gcsRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: 100, Request: req}, nil
	})}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if data, err := ReadBounded(t.Context(), &GCSStorage{client: client}, "bucket", "key", 8); !errors.Is(err, ErrObjectTooLarge) || data != nil {
		t.Fatalf("oversize=%q %v", data, err)
	}
	if !body.closed || body.read > 9 {
		t.Fatalf("read=%d closed=%v", body.read, body.closed)
	}
}

func TestMinioBoundedReadCancelsInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	backend := newMinioHealthTestStorage(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := ReadBounded(ctx, backend, "bucket", "key", 8); result <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bounded read ignored cancellation")
	}
}
