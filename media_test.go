package socialkit

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func multipartUpload(t *testing.T, method, path string, content []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "img.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(content)
	_ = w.Close()
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func insertPost(t *testing.T, rt *Runtime) string {
	t.Helper()
	var id string
	if err := rt.store.pool.QueryRow(context.Background(),
		`INSERT INTO `+rt.store.t.posts+` (author_id, title, body) VALUES ('admin','t','b') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("insert post: %v", err)
	}
	return id
}

func TestMedia_PollOptionImageUpload(t *testing.T) {
	media := &fakeMedia{}
	rt, _ := newTestRuntime(t, Options{Authz: allowAll{}, Perms: Perms{PollWrite: "root:polls:update"}, Media: media})
	admin := Actor{ID: "admin"}
	pv, err := rt.polls.create(context.Background(), admin, createPollInput{Question: "Q?", Options: []createOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatalf("create poll: %v", err)
	}
	oid := pv.Options[0].ID

	req := multipartUpload(t, "POST", "/polls/options/"+oid+"/image", []byte("PNGDATA"))
	req = req.WithContext(withActor(req.Context(), admin))
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := media.stored("polls/options/" + oid + ".png"); !ok {
		t.Fatal("image not written to the media store")
	}
	got, _ := rt.polls.get(context.Background(), admin, pv.ID)
	var found string
	for _, o := range got.Options {
		if o.ID == oid {
			found = o.ImageURL
		}
	}
	if found == "" {
		t.Fatal("option image_url not persisted")
	}
}

func TestMedia_PostCoverUpload(t *testing.T) {
	media := &fakeMedia{}
	rt, pool := newTestRuntime(t, Options{Authz: allowAll{}, Perms: Perms{PostWrite: "root:post:update"}, Media: media})
	id := insertPost(t, rt)

	req := multipartUpload(t, "POST", "/posts/"+id+"/cover", []byte("IMGBYTES"))
	req = req.WithContext(withActor(req.Context(), Actor{ID: "admin"}))
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := media.stored("posts/" + id + "/cover.png"); !ok {
		t.Fatal("cover not written to the media store")
	}
	var cover *string
	if err := pool.QueryRow(context.Background(), `SELECT cover_url FROM `+rt.store.t.posts+` WHERE id = $1`, id).Scan(&cover); err != nil {
		t.Fatalf("read cover: %v", err)
	}
	if cover == nil || *cover == "" {
		t.Fatal("cover_url not persisted")
	}
}

func TestMedia_UploadRequiresPerm(t *testing.T) {
	rt, _ := newTestRuntime(t, Options{Authz: denyAll{}, Perms: Perms{PostWrite: "root:post:update"}, Media: &fakeMedia{}})
	id := insertPost(t, rt)
	req := multipartUpload(t, "POST", "/posts/"+id+"/cover", []byte("IMG"))
	req = req.WithContext(withActor(req.Context(), Actor{ID: "nonadmin"}))
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for non-permitted uploader, got %d", rec.Code)
	}
}

// Pure (no container): the S3 store rejects an incomplete config; resolveMedia
// falls back to unsupported when neither Media nor Storage is set.
func TestStorage_ConfigValidation(t *testing.T) {
	if _, err := newS3Store(StorageConfig{Region: "us-east-1"}); err == nil {
		t.Fatal("expected error for missing Bucket")
	}
	if _, err := newS3Store(StorageConfig{Bucket: "b"}); err == nil {
		t.Fatal("expected error for missing PublicBaseURL")
	}
	if _, err := newS3Store(StorageConfig{Bucket: "b", PublicBaseURL: "https://cdn.test"}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	m, err := resolveMedia(Options{})
	if err != nil {
		t.Fatalf("resolveMedia default: %v", err)
	}
	if _, err := m.Put(context.Background(), "k", nil, ""); err == nil {
		t.Fatal("unsupported media store should error on Put")
	}
}
