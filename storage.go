package socialkit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// StorageConfig configures socialkit's built-in, S3-backed media store. When
// set (Options.Storage), socialkit owns file upload itself: it writes poll/post
// images to a PUBLIC bucket and returns the public URL — no presigning. Works
// with AWS S3, MinIO, or R2 (set Endpoint + UsePathStyle for the latter two).
type StorageConfig struct {
	Bucket          string
	Region          string
	Endpoint        string // custom S3 endpoint (MinIO/R2); empty = AWS default
	AccessKeyID     string
	SecretAccessKey string
	PublicBaseURL   string // public origin serving the bucket, e.g. https://cdn.example.com
	UsePathStyle    bool   // true for MinIO / most self-hosted S3
}

// s3Store is socialkit's built-in MediaStore over aws-sdk-go-v2, writing to a
// public bucket. It satisfies the MediaStore port (Put) and adds Delete.
type s3Store struct {
	client        *s3.Client
	bucket        string
	publicBaseURL string
}

func newS3Store(cfg StorageConfig) (*s3Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("socialkit: Storage.Bucket is required")
	}
	if cfg.PublicBaseURL == "" {
		return nil, fmt.Errorf("socialkit: Storage.PublicBaseURL is required (public bucket, no presigning)")
	}
	opts := s3.Options{
		Region:       cfg.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle: cfg.UsePathStyle,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	return &s3Store{
		client:        s3.New(opts),
		bucket:        cfg.Bucket,
		publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/"),
	}, nil
}

// Put writes bytes to the bucket at key and returns the public URL. The object
// is stored as-is (the host is responsible for any transcoding it wants).
func (s *s3Store) Put(ctx context.Context, key string, data []byte, contentType string) (string, error) {
	key = strings.TrimLeft(key, "/")
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	}); err != nil {
		return "", fmt.Errorf("socialkit: put object %q: %w", key, err)
	}
	return s.publicBaseURL + "/" + key, nil
}

// Delete removes an object (best-effort; DeleteByURL routes replaced images here).
func (s *s3Store) Delete(ctx context.Context, key string) error {
	key = strings.TrimLeft(key, "/")
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}

// DeleteByURL implements mediaURLDeleter: it recovers the key from a URL a
// previous Put returned and deletes the object. URLs outside this store's
// public origin (host-side or legacy-relative paths) are left alone.
func (s *s3Store) DeleteByURL(ctx context.Context, url string) error {
	key, ok := strings.CutPrefix(url, s.publicBaseURL+"/")
	if !ok || key == "" {
		return nil
	}
	return s.Delete(ctx, key)
}

// mediaURLDeleter is an optional MediaStore extension: delete the object behind
// a URL a previous Put returned. Used best-effort when an image is replaced
// under a different key (extension changed), so the old object is not orphaned.
type mediaURLDeleter interface {
	DeleteByURL(ctx context.Context, url string) error
}

// deleteMediaByURL best-effort removes a replaced object when the store
// supports deletion; a failure just leaves an orphan (same as before).
func (rt *Runtime) deleteMediaByURL(ctx context.Context, url string) {
	if d, ok := rt.media.(mediaURLDeleter); ok && url != "" {
		_ = d.DeleteByURL(ctx, url)
	}
}

// resolveMedia picks the MediaStore: an explicit Media port wins (host override);
// else a configured Storage builds the built-in S3 store; else uploads are
// unsupported (Put errors).
func resolveMedia(opts Options) (MediaStore, error) {
	if opts.Media != nil {
		return opts.Media, nil
	}
	if opts.Storage != nil {
		return newS3Store(*opts.Storage)
	}
	return unsupportedMediaStore{}, nil
}

// maxUploadBytes caps an image upload (10 MiB).
const maxUploadBytes = 10 << 20

// readUpload reads the multipart "file" field with a size cap and returns its
// bytes + content type + a filename extension. Oversize uploads are REJECTED
// (400), never silently truncated to a corrupt image.
func readUpload(r *http.Request) (data []byte, contentType, ext string, err error) {
	// Cap the whole body before parsing: ParseMultipartForm's argument is only
	// the in-memory threshold (the rest spills to temp files), not a limit. The
	// slack covers multipart framing around a max-size file.
	r.Body = http.MaxBytesReader(nil, r.Body, maxUploadBytes+(64<<10))
	if err = r.ParseMultipartForm(maxUploadBytes); err != nil {
		return nil, "", "", badRequest("invalid multipart form: %v", err)
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, "", "", badRequest("missing 'file' field: %v", err)
	}
	defer f.Close()
	var b bytes.Buffer
	n, err := b.ReadFrom(io.LimitReader(f, maxUploadBytes+1))
	if err != nil {
		return nil, "", "", err
	}
	if n > maxUploadBytes {
		return nil, "", "", badRequest("file exceeds the %d MiB upload limit", maxUploadBytes>>20)
	}
	contentType = hdr.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return b.Bytes(), contentType, extFromName(hdr.Filename), nil
}

func extFromName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 && i < len(name)-1 {
		return strings.ToLower(name[i+1:])
	}
	return "bin"
}
