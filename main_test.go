package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fsouza/fake-gcs-server/fakestorage"
)

func TestHeader(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		key     string
		wantVal string
		wantOk  bool
	}{
		{"nil header", nil, "X-Foo", "", false},
		{"missing key", http.Header{"X-Bar": {"v"}}, "X-Foo", "", false},
		{"present", http.Header{"X-Foo": {"v"}}, "X-Foo", "v", true},
		{"empty values slice", http.Header{"X-Foo": {}}, "X-Foo", "", false},
		{"first of many", http.Header{"X-Foo": {"a", "b"}}, "X-Foo", "a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Header: tc.headers}
			got, ok := header(r, tc.key)
			if got != tc.wantVal || ok != tc.wantOk {
				t.Errorf("header() = (%q, %v), want (%q, %v)", got, ok, tc.wantVal, tc.wantOk)
			}
		})
	}
}

func TestSetStrHeader(t *testing.T) {
	w := httptest.NewRecorder()
	setStrHeader(w, "X-Empty", "")
	setStrHeader(w, "X-Set", "value")
	if v := w.Header().Get("X-Empty"); v != "" {
		t.Errorf("X-Empty should not be set, got %q", v)
	}
	if v := w.Header().Get("X-Set"); v != "value" {
		t.Errorf("X-Set = %q, want %q", v, "value")
	}
}

func TestSetIntHeader(t *testing.T) {
	w := httptest.NewRecorder()
	setIntHeader(w, "X-Zero", 0)
	setIntHeader(w, "X-Negative", -1)
	setIntHeader(w, "X-Positive", 123)
	if v := w.Header().Get("X-Zero"); v != "" {
		t.Errorf("X-Zero should not be set, got %q", v)
	}
	if v := w.Header().Get("X-Negative"); v != "" {
		t.Errorf("X-Negative should not be set, got %q", v)
	}
	if v := w.Header().Get("X-Positive"); v != "123" {
		t.Errorf("X-Positive = %q, want %q", v, "123")
	}
}

func TestSetTimeHeader(t *testing.T) {
	w := httptest.NewRecorder()
	setTimeHeader(w, "X-Zero", time.Time{})
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	setTimeHeader(w, "X-Set", ts)
	if v := w.Header().Get("X-Zero"); v != "" {
		t.Errorf("X-Zero should not be set, got %q", v)
	}
	if v := w.Header().Get("X-Set"); v != ts.Format(http.TimeFormat) {
		t.Errorf("X-Set = %q, want %q", v, ts.Format(http.TimeFormat))
	}
}

func TestWrapResponseWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &wrapResponseWriter{ResponseWriter: rec, status: http.StatusOK}
	w.WriteHeader(http.StatusTeapot)
	if w.status != http.StatusTeapot {
		t.Errorf("status = %d, want %d", w.status, http.StatusTeapot)
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("underlying status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

func TestWrapperCapturesCustomStatus(t *testing.T) {
	called := false
	h := wrapper(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "missing")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	h(rec, req)
	if !called {
		t.Fatal("wrapped handler was not called")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Body.String(); got != "missing" {
		t.Errorf("body = %q, want %q", got, "missing")
	}
}

func TestHealthCheck(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_health", nil)
	healthCheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "OK\n" {
		t.Errorf("body = %q, want %q", got, "OK\n")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/plain")
	}
}

// --- proxy / fetchObjectAttrs integration tests (fake-gcs-server) ---

const (
	testBucket    = "test-bucket"
	testObject    = "path/to/file.txt"
	testContent   = "hello gcsproxy"
	testCType     = "text/plain"
	testIndexBody = "<html>index</html>"
)

func newTestServer(t *testing.T, objects []fakestorage.Object) *fakestorage.Server {
	t.Helper()
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		InitialObjects: objects,
		Scheme:         "http",
		Host:           "127.0.0.1",
		Port:           0,
	})
	if err != nil {
		t.Fatalf("fakestorage.NewServerWithOptions: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv
}

// installTestClient replaces the package-level client/flags with values pointing
// at the fake GCS server, restoring originals on test cleanup.
func installTestClient(t *testing.T, srv *fakestorage.Server, idx string) {
	t.Helper()
	c := srv.Client()
	prevClient := client
	prevIdx := *defaultIndex
	client = c
	*defaultIndex = idx
	t.Cleanup(func() {
		client = prevClient
		*defaultIndex = prevIdx
		_ = c.Close()
	})
}

func setSPA(t *testing.T, enabled bool) {
	t.Helper()
	prev := *spa
	*spa = enabled
	t.Cleanup(func() { *spa = prev })
}

func setNotFoundPath(t *testing.T, path string) {
	t.Helper()
	prev := *notFoundPath
	*notFoundPath = path
	t.Cleanup(func() { *notFoundPath = prev })
}

func TestProxy_OK(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        testObject,
			ContentType: testCType,
		},
		Content: []byte(testContent),
	}})
	installTestClient(t, srv, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
	if ct := rec.Header().Get("Content-Type"); ct != testCType {
		t.Errorf("Content-Type = %q, want %q", ct, testCType)
	}
	if lm := rec.Header().Get("Last-Modified"); lm == "" {
		t.Error("Last-Modified header is missing")
	}
}

func TestProxy_NotFound(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: "exists.txt"},
		Content:     []byte("x"),
	}})
	installTestClient(t, srv, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/missing.txt", nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (body=%q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestProxy_NotModified(t *testing.T) {
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName: testBucket,
			Name:       testObject,
			Updated:    updated,
		},
		Content: []byte(testContent),
	}})
	installTestClient(t, srv, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("If-Modified-Since", updated.Format(http.TimeFormat))
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", rec.Body.String())
	}
}

func TestProxy_ModifiedSinceOlder(t *testing.T) {
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName: testBucket,
			Name:       testObject,
			Updated:    updated,
		},
		Content: []byte(testContent),
	}})
	installTestClient(t, srv, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("If-Modified-Since", updated.Add(-1*time.Hour).Format(http.TimeFormat))
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_DefaultIndex_EmptyObject(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	installTestClient(t, srv, "index.html")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/", nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

func TestProxy_DefaultIndex_SubdirectoryFallback(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "foo/index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	installTestClient(t, srv, "index.html")

	rec := httptest.NewRecorder()
	// Request a path that does not exist as an object; fetchObjectAttrs should
	// retry by appending the default index file ("foo/" + "index.html").
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/foo/", nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

func TestProxy_HealthCheckRoute(t *testing.T) {
	// Health endpoint must not require GCS credentials.
	srv := newTestServer(t, nil)
	installTestClient(t, srv, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_health", nil)
	newRouter("").ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "OK") {
		t.Errorf("body = %q, want to contain %q", rec.Body.String(), "OK")
	}
}

// --- source bucket (-s) mode tests ---

func TestProxy_SourceBucket_OK(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        testObject,
			ContentType: testCType,
		},
		Content: []byte(testContent),
	}})
	installTestClient(t, srv, "")

	rec := httptest.NewRecorder()
	// In source-bucket mode the path no longer carries the bucket name.
	req := httptest.NewRequest(http.MethodGet, "/"+testObject, nil)
	newRouter(testBucket).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
	if ct := rec.Header().Get("Content-Type"); ct != testCType {
		t.Errorf("Content-Type = %q, want %q", ct, testCType)
	}
}

func TestProxy_SourceBucket_NotFound(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: "exists.txt"},
		Content:     []byte("x"),
	}})
	installTestClient(t, srv, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/missing.txt", nil)
	newRouter(testBucket).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (body=%q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestProxy_SourceBucket_DefaultIndex(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	installTestClient(t, srv, "index.html")

	rec := httptest.NewRecorder()
	// Root path "/" with default index should resolve to bucket's index.html.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	newRouter(testBucket).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

func TestProxy_SourceBucket_IgnoresPathBucket(t *testing.T) {
	// When -s is set, the first path segment is part of the object key,
	// not a bucket selector. A request like "/other-bucket/file.txt" should
	// look for object "other-bucket/file.txt" in the configured bucket.
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "other-bucket/file.txt",
			ContentType: testCType,
		},
		Content: []byte(testContent),
	}})
	installTestClient(t, srv, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/other-bucket/file.txt", nil)
	newRouter(testBucket).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_SourceBucket_HealthCheckStillWorks(t *testing.T) {
	srv := newTestServer(t, nil)
	installTestClient(t, srv, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_health", nil)
	newRouter(testBucket).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "OK") {
		t.Errorf("body = %q, want to contain %q", rec.Body.String(), "OK")
	}
}

// --- SPA fallback (-spa) tests ---

func TestProxy_SPA_FallbackToRootIndex(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	installTestClient(t, srv, "index.html")
	setSPA(t, true)

	rec := httptest.NewRecorder()
	// Arbitrary path that doesn't exist as an object.
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/some/spa/route", nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

func TestProxy_SPA_PassthroughForExistingObject(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{
		{
			ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
			Content:     []byte(testContent),
		},
		{
			ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: "index.html", ContentType: "text/html"},
			Content:     []byte(testIndexBody),
		},
	})
	installTestClient(t, srv, "index.html")
	setSPA(t, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q (SPA should not override real objects)", got, testContent)
	}
}

func TestProxy_SPA_NoRootIndexReturns404(t *testing.T) {
	// SPA enabled but the root index.html itself does not exist.
	srv := newTestServer(t, nil)
	installTestClient(t, srv, "index.html")
	setSPA(t, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/whatever", nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestProxy_SPA_WithSourceBucket(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	installTestClient(t, srv, "index.html")
	setSPA(t, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/deep/nested/route", nil)
	newRouter(testBucket).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

// --- Custom not-found (-not-found) tests ---

func TestProxy_NotFound_ServesCustomPage(t *testing.T) {
	const notFoundBody = "<html>oops</html>"
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "404.html",
			ContentType: "text/html",
		},
		Content: []byte(notFoundBody),
	}})
	installTestClient(t, srv, "")
	setNotFoundPath(t, "404.html")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/missing.txt", nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if got := rec.Body.String(); got != notFoundBody {
		t.Errorf("body = %q, want %q", got, notFoundBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html")
	}
}

func TestProxy_NotFound_NotInvokedForExistingObject(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{
		{
			ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
			Content:     []byte(testContent),
		},
		{
			ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: "404.html", ContentType: "text/html"},
			Content:     []byte("should not be served"),
		},
	})
	installTestClient(t, srv, "")
	setNotFoundPath(t, "404.html")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_NotFound_MissingPageFallsBackToDefault404(t *testing.T) {
	// The configured not-found object itself doesn't exist in the bucket.
	srv := newTestServer(t, nil)
	installTestClient(t, srv, "")
	setNotFoundPath(t, "404.html")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/missing.txt", nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// --- structured logging tests ---

// installLogger redirects slog.Default() to the given handler for the test
// duration, restoring the previous default on cleanup.
func installLogger(t *testing.T, h slog.Handler) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func setVerbose(t *testing.T, enabled bool) {
	t.Helper()
	prev := *verbose
	*verbose = enabled
	t.Cleanup(func() { *verbose = prev })
}

func TestAccessLog_JSONFormat(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     []byte(testContent),
	}})
	installTestClient(t, srv, "")
	setVerbose(t, true)

	var buf bytes.Buffer
	installLogger(t, slog.NewJSONHandler(&buf, nil))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Verify the access log was emitted as a single JSON line with the
	// expected structured fields.
	logLine := strings.TrimSpace(buf.String())
	if logLine == "" {
		t.Fatal("no log output captured")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(logLine), &entry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\nraw: %s", err, logLine)
	}
	if got := entry["msg"]; got != "access" {
		t.Errorf("msg = %v, want %q", got, "access")
	}
	if got := entry["method"]; got != "GET" {
		t.Errorf("method = %v, want %q", got, "GET")
	}
	if got := entry["status"]; got != float64(http.StatusOK) { // json numbers are float64
		t.Errorf("status = %v, want %d", got, http.StatusOK)
	}
	if got, ok := entry["url"].(string); !ok || !strings.HasSuffix(got, testObject) {
		t.Errorf("url = %v, want suffix %q", entry["url"], testObject)
	}
	if _, ok := entry["elapsed"].(float64); !ok {
		t.Errorf("elapsed is missing or not a number: %v", entry["elapsed"])
	}
}

func TestAccessLog_NotEmittedWithoutVerbose(t *testing.T) {
	srv := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     []byte(testContent),
	}})
	installTestClient(t, srv, "")
	setVerbose(t, false)

	var buf bytes.Buffer
	installLogger(t, slog.NewJSONHandler(&buf, nil))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	newRouter("").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("unexpected log output without -v: %q", got)
	}
}
