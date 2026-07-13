package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
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
	s := &Server{}
	called := false
	h := s.wrap(func(w http.ResponseWriter, r *http.Request) {
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

func TestIndexCandidates(t *testing.T) {
	cases := []struct {
		name      string
		object    string
		indexName string
		walkUp    bool
		want      []string
	}{
		{
			name:      "single candidate",
			object:    "foo/bar",
			indexName: "index.html",
			walkUp:    false,
			want:      []string{"foo/bar/index.html"},
		},
		{
			name:      "walk up path and root",
			object:    "foo/bar/baz",
			indexName: "index.html",
			walkUp:    true,
			want:      []string{"foo/bar/baz/index.html", "foo/bar/index.html", "foo/index.html"},
		},
		{
			name:      "walk up from trailing slash",
			object:    "foo/bar/",
			indexName: "index.html",
			walkUp:    true,
			want:      []string{"foo/bar/index.html", "foo/index.html"},
		},
		{
			name:      "single segment walk up is unchanged",
			object:    "search",
			indexName: "index.html",
			walkUp:    true,
			want:      []string{"search/index.html"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := indexCandidates(tc.object, tc.indexName, tc.walkUp)
			if err != nil {
				t.Fatalf("indexCandidates() returned error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len(got)=%d len(want)=%d; got=%v want=%v", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("candidates[%d]=%q want %q; got=%v want=%v", i, got[i], tc.want[i], got, tc.want)
				}
			}
		})
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

// newTestServer spins up an in-process fake GCS server and returns a Server
// wired to it. Tests configure the returned Server's fields (defaultIndex,
// spa, etc.) before calling s.handler().
func newTestServer(t *testing.T, objects []fakestorage.Object) *Server {
	t.Helper()
	fakeSrv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		InitialObjects: objects,
		Scheme:         "http",
		Host:           "127.0.0.1",
		Port:           0,
	})
	if err != nil {
		t.Fatalf("fakestorage.NewServerWithOptions: %v", err)
	}
	t.Cleanup(fakeSrv.Stop)
	c := fakeSrv.Client()
	t.Cleanup(func() { _ = c.Close() })
	return &Server{client: c}
}

func newRewriteProxy(t *testing.T, upstreamURL *url.URL, prefix string) *httptest.Server {
	t.Helper()
	rp := httputil.NewSingleHostReverseProxy(upstreamURL)
	baseDirector := rp.Director
	rp.Director = func(req *http.Request) {
		baseDirector(req)
		req.URL.Path = "/" + testBucket + prefix + req.URL.Path
		req.URL.RawPath = req.URL.Path
		req.Host = upstreamURL.Host
	}
	return httptest.NewServer(rp)
}

func TestProxy_OK(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        testObject,
			ContentType: testCType,
		},
		Content: []byte(testContent),
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

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
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: "exists.txt"},
		Content:     []byte("x"),
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/missing.txt", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (body=%q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestProxy_NotModified(t *testing.T) {
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName: testBucket,
			Name:       testObject,
			Updated:    updated,
		},
		Content: []byte(testContent),
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("If-Modified-Since", updated.Format(http.TimeFormat))
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", rec.Body.String())
	}
}

func TestProxy_ModifiedSinceOlder(t *testing.T) {
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName: testBucket,
			Name:       testObject,
			Updated:    updated,
		},
		Content: []byte(testContent),
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("If-Modified-Since", updated.Add(-1*time.Hour).Format(http.TimeFormat))
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_DefaultIndex_EmptyObject(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	s.defaultIndex = "index.html"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

func TestProxy_DefaultIndex_SubdirectoryFallback(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "foo/index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	s.defaultIndex = "index.html"

	rec := httptest.NewRecorder()
	// Request a path that does not exist as an object; fetchObjectAttrs should
	// retry by appending the default index file ("foo/" + "index.html").
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/foo/", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

func TestProxy_DefaultIndex_WalkUpFallback_Disabled(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "site-assets-gcsproxy/mainnet-site/index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	s.defaultIndex = "index.html"
	s.notFoundPath = "404.html"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/site-assets-gcsproxy/mainnet-site/search", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestProxy_DefaultIndex_WalkUpFallback_Enabled(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "site-assets-gcsproxy/mainnet-site/index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	s.defaultIndex = "index.html"
	s.walkUpIndex = true
	s.notFoundPath = "404.html"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/site-assets-gcsproxy/mainnet-site/search", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

func TestE2E_PrefixRewrite_SearchRouteWithoutWalkUp(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "site-assets-gcsproxy/mainnet-site/index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	s.defaultIndex = "index.html"
	s.notFoundPath = "404.html"

	gcsproxyHTTP := httptest.NewServer(s.handler())
	t.Cleanup(gcsproxyHTTP.Close)
	upstreamURL, err := url.Parse(gcsproxyHTTP.URL)
	if err != nil {
		t.Fatalf("url.Parse(gcsproxyHTTP.URL): %v", err)
	}

	rewriteProxy := newRewriteProxy(t, upstreamURL, "/site-assets-gcsproxy/mainnet-site")
	t.Cleanup(rewriteProxy.Close)

	res, err := http.Get(rewriteProxy.URL + "/search?q=test")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (body=%q)", res.StatusCode, http.StatusNotFound, string(body))
	}
}

func TestE2E_PrefixRewrite_SearchRouteWithWalkUp(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "site-assets-gcsproxy/mainnet-site/index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	s.defaultIndex = "index.html"
	s.walkUpIndex = true
	s.notFoundPath = "404.html"

	gcsproxyHTTP := httptest.NewServer(s.handler())
	t.Cleanup(gcsproxyHTTP.Close)
	upstreamURL, err := url.Parse(gcsproxyHTTP.URL)
	if err != nil {
		t.Fatalf("url.Parse(gcsproxyHTTP.URL): %v", err)
	}

	rewriteProxy := newRewriteProxy(t, upstreamURL, "/site-assets-gcsproxy/mainnet-site")
	t.Cleanup(rewriteProxy.Close)

	res, err := http.Get(rewriteProxy.URL + "/search?q=test")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", res.StatusCode, http.StatusOK, string(body))
	}
	if got := string(body); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

func TestProxy_HealthCheckRoute(t *testing.T) {
	// Health endpoint must not require GCS credentials.
	s := newTestServer(t, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_health", nil)
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "OK") {
		t.Errorf("body = %q, want to contain %q", rec.Body.String(), "OK")
	}
}

// --- source bucket (-bucket) mode tests ---

func TestProxy_SourceBucket_OK(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        testObject,
			ContentType: testCType,
		},
		Content: []byte(testContent),
	}})
	s.sourceBucket = testBucket

	rec := httptest.NewRecorder()
	// In source-bucket mode the path no longer carries the bucket name.
	req := httptest.NewRequest(http.MethodGet, "/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

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
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: "exists.txt"},
		Content:     []byte("x"),
	}})
	s.sourceBucket = testBucket

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/missing.txt", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (body=%q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestProxy_SourceBucket_DefaultIndex(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	s.defaultIndex = "index.html"
	s.sourceBucket = testBucket

	rec := httptest.NewRecorder()
	// Root path "/" with default index should resolve to bucket's index.html.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

func TestProxy_SourceBucket_IgnoresPathBucket(t *testing.T) {
	// When -bucket is set, the first path segment is part of the object key,
	// not a bucket selector. A request like "/other-bucket/file.txt" should
	// look for object "other-bucket/file.txt" in the configured bucket.
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "other-bucket/file.txt",
			ContentType: testCType,
		},
		Content: []byte(testContent),
	}})
	s.sourceBucket = testBucket

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/other-bucket/file.txt", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_SourceBucket_HealthCheckStillWorks(t *testing.T) {
	s := newTestServer(t, nil)
	s.sourceBucket = testBucket

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_health", nil)
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "OK") {
		t.Errorf("body = %q, want to contain %q", rec.Body.String(), "OK")
	}
}

// --- SPA fallback (-spa) tests ---

func TestProxy_SPA_FallbackToRootIndex(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	s.defaultIndex = "index.html"
	s.spa = true

	rec := httptest.NewRecorder()
	// Arbitrary path that doesn't exist as an object.
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/some/spa/route", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != testIndexBody {
		t.Errorf("body = %q, want %q", got, testIndexBody)
	}
}

func TestProxy_SPA_PassthroughForExistingObject(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{
		{
			ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
			Content:     []byte(testContent),
		},
		{
			ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: "index.html", ContentType: "text/html"},
			Content:     []byte(testIndexBody),
		},
	})
	s.defaultIndex = "index.html"
	s.spa = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q (SPA should not override real objects)", got, testContent)
	}
}

func TestProxy_SPA_NoRootIndexReturns404(t *testing.T) {
	// SPA enabled but the root index.html itself does not exist.
	s := newTestServer(t, nil)
	s.defaultIndex = "index.html"
	s.spa = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/whatever", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestProxy_SPA_WithSourceBucket(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "index.html",
			ContentType: "text/html",
		},
		Content: []byte(testIndexBody),
	}})
	s.defaultIndex = "index.html"
	s.spa = true
	s.sourceBucket = testBucket

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/deep/nested/route", nil)
	s.handler().ServeHTTP(rec, req)

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
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "404.html",
			ContentType: "text/html",
		},
		Content: []byte(notFoundBody),
	}})
	s.notFoundPath = "404.html"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/missing.txt", nil)
	s.handler().ServeHTTP(rec, req)

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
	s := newTestServer(t, []fakestorage.Object{
		{
			ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
			Content:     []byte(testContent),
		},
		{
			ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: "404.html", ContentType: "text/html"},
			Content:     []byte("should not be served"),
		},
	})
	s.notFoundPath = "404.html"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_NotFound_MissingPageFallsBackToDefault404(t *testing.T) {
	// The configured not-found object itself doesn't exist in the bucket.
	s := newTestServer(t, nil)
	s.notFoundPath = "404.html"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/missing.txt", nil)
	s.handler().ServeHTTP(rec, req)

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

func TestAccessLog_JSONFormat(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     []byte(testContent),
	}})
	s.verbose = true

	var buf bytes.Buffer
	installLogger(t, slog.NewJSONHandler(&buf, nil))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

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
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     []byte(testContent),
	}})
	// s.verbose left as false (zero value)

	var buf bytes.Buffer
	installLogger(t, slog.NewJSONHandler(&buf, nil))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("unexpected log output without -v: %q", got)
	}
}

// --- Content-Length / chunked transfer tests ---

func TestProxy_DefaultDoesNotSetContentLength(t *testing.T) {
	// By default the handler must not emit a Content-Length header itself.
	// net/http may still auto-fill it for small bodies that fit in its
	// internal buffer, but for anything large enough to matter (e.g. the
	// Cloud Run 32 MiB limit) net/http will fall back to chunked encoding.
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     []byte(testContent),
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length should not be set by the handler, got %q", got)
	}
}

func TestProxy_ContentLengthOptIn(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     []byte(testContent),
	}})
	s.contentLength = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	wantLen := strconv.Itoa(len(testContent))
	if got := rec.Header().Get("Content-Length"); got != wantLen {
		t.Errorf("Content-Length = %q, want %q", got, wantLen)
	}
}

// --- CORS tests ---

func TestProxy_CORSOrigin_Set(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     []byte(testContent),
	}})
	s.corsOrigin = "https://example.com"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "https://example.com")
	}
}

func TestProxy_CORSOrigin_AppliesToErrorResponses(t *testing.T) {
	// CORS header must be present even on 404 so the browser surfaces the
	// real status to JS instead of treating it as an opaque CORS failure.
	s := newTestServer(t, nil)
	s.corsOrigin = "*"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/missing", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "*")
	}
}

func TestProxy_CORSOrigin_Unset(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     []byte(testContent),
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin should be unset, got %q", got)
	}
}

// --- Range parsing unit tests ---

func TestParseSingleRange(t *testing.T) {
	const size = 1000
	cases := []struct {
		name       string
		header     string
		size       int64
		wantErr    error // nil for satisfiable; errRangeIgnore / errRangeUnsatisfiable otherwise
		wantStart  int64
		wantLength int64
	}{
		// Satisfiable
		{"explicit", "bytes=0-499", size, nil, 0, 500},
		{"explicit middle", "bytes=100-199", size, nil, 100, 100},
		{"single byte", "bytes=0-0", size, nil, 0, 1},
		{"open ended", "bytes=500-", size, nil, 500, 500},
		{"suffix", "bytes=-200", size, nil, 800, 200},
		{"suffix larger than size", "bytes=-2000", size, nil, 0, 1000},
		{"end clamped", "bytes=900-2000", size, nil, 900, 100},
		{"multi-range uses first", "bytes=0-99,200-299", size, nil, 0, 100},

		// Ignored (treated as if no Range header)
		{"non-bytes unit", "items=0-10", size, errRangeIgnore, 0, 0},
		{"missing prefix", "0-99", size, errRangeIgnore, 0, 0},
		{"empty", "", size, errRangeIgnore, 0, 0},
		{"no dash", "bytes=100", size, errRangeIgnore, 0, 0},
		{"negative start", "bytes=-0", size, errRangeIgnore, 0, 0},
		{"non-numeric start", "bytes=abc-100", size, errRangeIgnore, 0, 0},
		{"non-numeric end", "bytes=0-def", size, errRangeIgnore, 0, 0},

		// Unsatisfiable
		{"start past end", "bytes=1000-1100", size, errRangeUnsatisfiable, 0, 0},
		{"start past end (open)", "bytes=1500-", size, errRangeUnsatisfiable, 0, 0},
		{"end before start", "bytes=500-100", size, errRangeUnsatisfiable, 0, 0},
		{"zero-size object", "bytes=0-0", 0, errRangeUnsatisfiable, 0, 0},
		{"zero-size suffix", "bytes=-10", 0, errRangeUnsatisfiable, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, length, err := parseSingleRange(tc.header, tc.size)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if err == nil && (start != tc.wantStart || length != tc.wantLength) {
				t.Errorf("(start, length) = (%d, %d), want (%d, %d)", start, length, tc.wantStart, tc.wantLength)
			}
		})
	}
}

// --- Range integration tests (via the full handler stack) ---

func TestProxy_Range_Explicit(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "bytes=2-5")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Body.String(); got != "2345" {
		t.Errorf("body = %q, want %q", got, "2345")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/16" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 2-5/16")
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want %q", got, "bytes")
	}
}

func TestProxy_Range_Suffix(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "bytes=-4")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Body.String(); got != "cdef" {
		t.Errorf("body = %q, want %q", got, "cdef")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 12-15/16" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 12-15/16")
	}
}

func TestProxy_Range_OpenEnded(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "bytes=10-")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Body.String(); got != "abcdef" {
		t.Errorf("body = %q, want %q", got, "abcdef")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 10-15/16" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 10-15/16")
	}
}

func TestProxy_Range_Unsatisfiable(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "bytes=1000-2000")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestedRangeNotSatisfiable)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */16" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes */16")
	}
}

func TestProxy_FullResponse_AdvertisesAcceptRanges(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     []byte(testContent),
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want %q", got, "bytes")
	}
}

func TestProxy_Range_WithSourceBucket(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})
	s.sourceBucket = testBucket

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testObject, nil)
	req.Header.Set("Range", "bytes=0-3")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Body.String(); got != "0123" {
		t.Errorf("body = %q, want %q", got, "0123")
	}
}

func TestProxy_Range_HonorsContentLengthFlag(t *testing.T) {
	// 206 responses follow the same -content-length policy as 200
	// responses: omitted by default (so large ranges bypass the Cloud Run
	// 32 MiB non-streamed payload cap via chunked encoding) and emitted
	// only when -content-length is opted in.
	body := []byte("0123456789abcdef")

	t.Run("default omits Content-Length", func(t *testing.T) {
		s := newTestServer(t, []fakestorage.Object{{
			ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
			Content:     body,
		}})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
		req.Header.Set("Range", "bytes=2-5")
		s.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
		}
		if got := rec.Header().Get("Content-Length"); got != "" {
			t.Errorf("Content-Length should be unset by default, got %q", got)
		}
	})

	t.Run("opt-in emits Content-Length", func(t *testing.T) {
		s := newTestServer(t, []fakestorage.Object{{
			ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
			Content:     body,
		}})
		s.contentLength = true

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
		req.Header.Set("Range", "bytes=2-5")
		s.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
		}
		if got := rec.Header().Get("Content-Length"); got != "4" {
			t.Errorf("Content-Length = %q, want %q", got, "4")
		}
	})
}

func TestProxy_Range_GzippedObject_FallsBackToFullBody(t *testing.T) {
	// Objects stored with Content-Encoding: gzip cannot be served as
	// partial content reliably (GCS transcodes them, and NewRangeReader
	// silently ignores the range — verified against real GCS). The
	// handler should serve the full body with 200 instead of returning a
	// truncated 206.
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:      testBucket,
			Name:            testObject,
			ContentType:     testCType,
			ContentEncoding: "gzip",
		},
		Content: body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "bytes=0-3")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (Range over gzipped object should fall back to 200)", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Errorf("Content-Range should not be set on the fallback, got %q", got)
	}
	// Accept-Ranges: bytes is intentionally omitted for gzip-encoded
	// objects because partial content is not actually supported (the
	// handler falls back to full body), so advertising range support
	// would be misleading.
	if got := rec.Header().Get("Accept-Ranges"); got != "" {
		t.Errorf("Accept-Ranges should not be advertised for gzip-encoded objects, got %q", got)
	}
}

func TestProxy_Range_NonBytesUnit_Ignored(t *testing.T) {
	// RFC 7233 §3.1: unknown range units must be ignored — fall back to
	// a normal 200 response, not 416. This protects against intermediate
	// proxies or clients that send exotic Range headers.
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "items=0-10")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (non-bytes range unit should be ignored)", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != string(body) {
		t.Errorf("body = %q, want full body %q", got, body)
	}
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Errorf("Content-Range should not be set for ignored Range, got %q", got)
	}
}

func TestProxy_Range_MalformedHeader_Ignored(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "bytes=abc-def")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (malformed Range should be ignored)", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != string(body) {
		t.Errorf("body = %q, want full body %q", got, body)
	}
}

func TestProxy_Range_ForwardsContentEncoding(t *testing.T) {
	// Non-gzip encodings (br, deflate, etc.) are not transcoded by GCS,
	// so Range works over the stored bytes. The encoding MUST be forwarded
	// to the client so they know how to interpret the partial body.
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:      testBucket,
			Name:            testObject,
			ContentType:     testCType,
			ContentEncoding: "br",
		},
		Content: body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "bytes=0-3")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Errorf("Content-Encoding = %q, want %q", got, "br")
	}
}

// --- Log level tests ---

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"info", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"DEBUG", slog.LevelDebug, false}, // case-insensitive
		{"Info", slog.LevelInfo, false},
		{"WARN", slog.LevelWarn, false},
		{"", 0, true},
		{"trace", 0, true},
		{"notice", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseLogLevel(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("level = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAccessLog_SuppressedByLogLevel(t *testing.T) {
	// Even with -v on, an access log at INFO is filtered out when the
	// handler level is WARN.
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     []byte(testContent),
	}})
	s.verbose = true

	var buf bytes.Buffer
	installLogger(t, slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("INFO access log should be filtered at WARN, got %q", got)
	}
}

func TestWarnLog_PassesThroughWarnLevel(t *testing.T) {
	// A slog.Warn call should still appear when the handler level is WARN.
	var buf bytes.Buffer
	installLogger(t, slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	slog.Warn("smoke", "key", "value")

	if got := buf.String(); !strings.Contains(got, `"level":"WARN"`) || !strings.Contains(got, `"msg":"smoke"`) {
		t.Errorf("warn log was not emitted at WARN level, got %q", got)
	}
}

// --- HEAD tests ---
//
// HEAD must not open a GCS reader: the previous implementation went through
// the same NewReader / NewRangeReader path as GET, so io.Copy would still
// download every byte from GCS even though net/http discarded the body
// (issue #59).

// countingClient wraps a *storage.Client and records the number of object
// read calls (NewReader / NewRangeReader). We can't introspect those calls
// directly, but we can detect them by checking the recorder's Body — for
// HEAD the handler must not write any body bytes.
//
// (We rely on the absence of body writes as a proxy for "no GCS read",
// since the handler always calls io.Copy immediately after opening a
// reader. Skipping the read implies skipping the io.Copy too.)

func TestProxy_HEAD_NoBody(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.Len(); got != 0 {
		t.Errorf("body length = %d, want 0 (HEAD must not include a body)", got)
	}
	if got := rec.Header().Get("Content-Type"); got != testCType {
		t.Errorf("Content-Type = %q, want %q", got, testCType)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want %q", got, "bytes")
	}
}

func TestProxy_HEAD_ContentLengthOptIn(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})
	s.contentLength = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/"+testBucket+"/"+testObject, nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.Len(); got != 0 {
		t.Errorf("body length = %d, want 0", got)
	}
	wantLen := strconv.Itoa(len(body))
	if got := rec.Header().Get("Content-Length"); got != wantLen {
		t.Errorf("Content-Length = %q, want %q", got, wantLen)
	}
}

func TestProxy_HEAD_NotFound(t *testing.T) {
	s := newTestServer(t, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/"+testBucket+"/missing.txt", nil)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestProxy_HEAD_Range(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "bytes=2-5")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Body.Len(); got != 0 {
		t.Errorf("body length = %d, want 0 (HEAD must not include a body)", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/16" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 2-5/16")
	}
}

func TestProxy_HEAD_Range_Unsatisfiable(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "bytes=1000-2000")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestedRangeNotSatisfiable)
	}
}

func TestProxy_HEAD_Range_NonBytesUnitIgnored(t *testing.T) {
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{BucketName: testBucket, Name: testObject, ContentType: testCType},
		Content:     body,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/"+testBucket+"/"+testObject, nil)
	req.Header.Set("Range", "items=0-10")
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.Len(); got != 0 {
		t.Errorf("body length = %d, want 0", got)
	}
}

func TestProxy_HEAD_GzippedObject(t *testing.T) {
	// HEAD on a gzip-stored object should still avoid the body copy and
	// produce sensible headers based on Accept-Encoding negotiation.
	body := []byte("0123456789abcdef")
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:      testBucket,
			Name:            testObject,
			ContentType:     testCType,
			ContentEncoding: "gzip",
		},
		Content: body,
	}})
	s.contentLength = true

	t.Run("Accept-Encoding: gzip", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodHead, "/"+testBucket+"/"+testObject, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		s.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Body.Len(); got != 0 {
			t.Errorf("body length = %d, want 0", got)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
		}
	})

	t.Run("no Accept-Encoding", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodHead, "/"+testBucket+"/"+testObject, nil)
		s.handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Body.Len(); got != 0 {
			t.Errorf("body length = %d, want 0", got)
		}
		// GCS would transcode for GET in this case, so neither
		// Content-Encoding nor a definitive Content-Length applies.
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty", got)
		}
	})
}

// --- Conditional request tests (ETag / RFC 9110 preconditions) ---

const (
	testEtag     = "v1abc"
	testCacheCtl = "max-age=60"
)

var testUpdated = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// newETagServer returns a Server backed by a single object with known
// validators (ETag, Last-Modified, Cache-Control) so conditional tests can
// assert literal header values.
func newETagServer(t *testing.T) *Server {
	t.Helper()
	return newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:   testBucket,
			Name:         testObject,
			ContentType:  testCType,
			CacheControl: testCacheCtl,
			Etag:         testEtag,
			Updated:      testUpdated,
		},
		Content: []byte(testContent),
	}})
}

// newGzipETagServer stores plaintext gzip-compressed with
// Content-Encoding: gzip, so fake-gcs-server transcodes it on download
// exactly like real GCS.
func newGzipETagServer(t *testing.T, plaintext string) *Server {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(plaintext)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:      testBucket,
			Name:            testObject,
			ContentType:     testCType,
			ContentEncoding: "gzip",
			Etag:            testEtag,
			Updated:         testUpdated,
		},
		Content: buf.Bytes(),
	}})
}

// doRequest performs a request for the standard test object with the given
// extra headers and returns the recorded response.
func doRequest(t *testing.T, s *Server, method string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/"+testBucket+"/"+testObject, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	s.handler().ServeHTTP(rec, req)
	return rec
}

func TestObjectETag(t *testing.T) {
	cases := []struct {
		name  string
		attrs *storage.ObjectAttrs
		want  string
	}{
		{"identity", &storage.ObjectAttrs{Etag: "v1abc"}, `"v1abc"`},
		{"gzip is weak", &storage.ObjectAttrs{Etag: "v1abc", ContentEncoding: "gzip"}, `W/"v1abc"`},
		{"gzip case-insensitive", &storage.ObjectAttrs{Etag: "v1abc", ContentEncoding: "GZIP"}, `W/"v1abc"`},
		{"other encoding stays strong", &storage.ObjectAttrs{Etag: "v1abc", ContentEncoding: "br"}, `"v1abc"`},
		{"empty etag omitted", &storage.ObjectAttrs{}, ""},
		{"empty etag gzip omitted", &storage.ObjectAttrs{ContentEncoding: "gzip"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := objectETag(tc.attrs); got != tc.want {
				t.Errorf("objectETag() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScanETag(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantETag   string
		wantRemain string
	}{
		{"strong", `"x"`, `"x"`, ""},
		{"weak", `W/"x"`, `W/"x"`, ""},
		{"leading whitespace", `  "x"`, `"x"`, ""},
		{"list remainder", `"a", "b"`, `"a"`, `, "b"`},
		{"empty opaque tag", `""`, `""`, ""},
		{"obs-text bytes allowed", "\"caf\xc3\xa9\"", "\"caf\xc3\xa9\"", ""},
		{"unterminated", `"x`, "", ""},
		{"missing quotes", `x`, "", ""},
		{"long but unquoted", `ab`, "", ""},
		{"lone weak prefix", `W/`, "", ""},
		{"control char inside", "\"x\x01y\"", "", ""},
		{"embedded space invalid", `"a b"`, "", ""},
		{"empty input", ``, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			etag, remain := scanETag(tc.in)
			if etag != tc.wantETag || remain != tc.wantRemain {
				t.Errorf("scanETag(%q) = (%q, %q), want (%q, %q)", tc.in, etag, remain, tc.wantETag, tc.wantRemain)
			}
		})
	}
}

func TestAnyETagMatches(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		target string
		strong bool
		want   bool
	}{
		{"strong exact", []string{`"x"`}, `"x"`, true, true},
		{"strong rejects weak header tag", []string{`W/"x"`}, `"x"`, true, false},
		{"strong rejects weak target", []string{`"x"`}, `W/"x"`, true, false},
		{"weak accepts weak header tag", []string{`W/"x"`}, `"x"`, false, true},
		{"weak accepts weak target", []string{`"x"`}, `W/"x"`, false, true},
		{"identical weak tags fail strong compare", []string{`W/"x"`}, `W/"x"`, true, false},
		{"identical weak tags pass weak compare", []string{`W/"x"`}, `W/"x"`, false, true},
		{"mismatch", []string{`"y"`}, `"x"`, true, false},
		{"list with spaces", []string{`"a", "b" , "x"`}, `"x"`, true, true},
		{"multiple header values", []string{`"a"`, `"x"`}, `"x"`, true, true},
		{"star", []string{`*`}, `"x"`, true, true},
		{"star with empty target", []string{`*`}, ``, true, true},
		{"empty target never matches tags", []string{`"x"`, `""`}, ``, false, false},
		{"malformed stops that value", []string{`garbage, "x"`}, `"x"`, true, false},
		{"malformed value then matching value", []string{`garbage`, `"x"`}, `"x"`, true, true},
		{"empty values", nil, `"x"`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anyETagMatches(tc.values, tc.target, tc.strong); got != tc.want {
				t.Errorf("anyETagMatches(%q, %q, %v) = %v, want %v", tc.values, tc.target, tc.strong, got, tc.want)
			}
		})
	}
}

func TestIfRangeAllowsPartial(t *testing.T) {
	attrs := &storage.ObjectAttrs{Etag: "v1abc", Updated: testUpdated}
	weakAttrs := &storage.ObjectAttrs{Etag: "v1abc", ContentEncoding: "gzip", Updated: testUpdated}
	noTimeAttrs := &storage.ObjectAttrs{Etag: "v1abc"}
	cases := []struct {
		name    string
		ifRange string
		attrs   *storage.ObjectAttrs
		want    bool
	}{
		{"absent", "", attrs, true},
		{"strong match", `"v1abc"`, attrs, true},
		{"strong mismatch", `"other"`, attrs, false},
		{"weak tag never matches", `W/"v1abc"`, attrs, false},
		{"strong tag against weak object tag", `"v1abc"`, weakAttrs, false},
		{"identical weak tags still never match", `W/"v1abc"`, weakAttrs, false},
		{"date equal", testUpdated.Format(http.TimeFormat), attrs, true},
		{"date earlier", testUpdated.Add(-time.Hour).Format(http.TimeFormat), attrs, false},
		{"date with zero updated", testUpdated.Format(http.TimeFormat), noTimeAttrs, false},
		{"garbage", "garbage", attrs, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tc.ifRange != "" {
				req.Header.Set("If-Range", tc.ifRange)
			}
			if got := ifRangeAllowsPartial(req, tc.attrs); got != tc.want {
				t.Errorf("ifRangeAllowsPartial(If-Range: %q) = %v, want %v", tc.ifRange, got, tc.want)
			}
		})
	}

	t.Run("empty value treated as absent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("If-Range", "")
		if !ifRangeAllowsPartial(req, attrs) {
			t.Error("ifRangeAllowsPartial(If-Range: <empty>) = false, want true")
		}
	})
}

func TestHasETagValues(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   bool
	}{
		{"nil", nil, false},
		{"single empty", []string{""}, false},
		{"whitespace only", []string{"  "}, false},
		{"empty then tag", []string{"", `"x"`}, true},
		{"tag", []string{`"x"`}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasETagValues(tc.values); got != tc.want {
				t.Errorf("hasETagValues(%q) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

func TestProxy_ETag_OnGet(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("ETag"); got != `"`+testEtag+`"` {
		t.Errorf("ETag = %q, want %q", got, `"`+testEtag+`"`)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_ETag_OnHead(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodHead, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("ETag"); got != `"`+testEtag+`"` {
		t.Errorf("ETag = %q, want %q", got, `"`+testEtag+`"`)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", rec.Body.String())
	}
}

func TestProxy_ETag_On206Range(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"Range": "bytes=2-5"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Header().Get("ETag"); got != `"`+testEtag+`"` {
		t.Errorf("ETag = %q, want %q", got, `"`+testEtag+`"`)
	}
	if got := rec.Body.String(); got != testContent[2:6] {
		t.Errorf("body = %q, want %q", got, testContent[2:6])
	}
	if got := rec.Header().Get("Content-Range"); got == "" {
		t.Error("Content-Range header is missing")
	}
}

func TestProxy_ETag_GzipObject_WeakWithVary(t *testing.T) {
	const plaintext = "gzip me please, gcsproxy"
	wantETag := `W/"` + testEtag + `"`

	t.Run("Accept-Encoding gzip", func(t *testing.T) {
		s := newGzipETagServer(t, plaintext)
		rec := doRequest(t, s, http.MethodGet, map[string]string{"Accept-Encoding": "gzip"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("ETag"); got != wantETag {
			t.Errorf("ETag = %q, want %q", got, wantETag)
		}
		if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("Vary = %q, want %q", got, "Accept-Encoding")
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("body is not gzip: %v", err)
		}
		decompressed, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		if string(decompressed) != plaintext {
			t.Errorf("decompressed body = %q, want %q", decompressed, plaintext)
		}
	})

	t.Run("no Accept-Encoding", func(t *testing.T) {
		s := newGzipETagServer(t, plaintext)
		rec := doRequest(t, s, http.MethodGet, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("ETag"); got != wantETag {
			t.Errorf("ETag = %q, want %q", got, wantETag)
		}
		if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("Vary = %q, want %q", got, "Accept-Encoding")
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty (transcoded)", got)
		}
		if got := rec.Body.String(); got != plaintext {
			t.Errorf("body = %q, want %q", got, plaintext)
		}
	})
}

func TestProxy_IfNoneMatch_Match(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-None-Match": `"` + testEtag + `"`})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", rec.Body.String())
	}
	// 304 must carry the validators the 200 would have (RFC 9110 §15.4.5).
	if got := rec.Header().Get("ETag"); got != `"`+testEtag+`"` {
		t.Errorf("ETag = %q, want %q", got, `"`+testEtag+`"`)
	}
	if got := rec.Header().Get("Last-Modified"); got != testUpdated.Format(http.TimeFormat) {
		t.Errorf("Last-Modified = %q, want %q", got, testUpdated.Format(http.TimeFormat))
	}
	if got := rec.Header().Get("Cache-Control"); got != testCacheCtl {
		t.Errorf("Cache-Control = %q, want %q", got, testCacheCtl)
	}
	if got := rec.Header().Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want empty for non-gzip object", got)
	}
}

func TestProxy_IfNoneMatch_WeakFormMatch(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-None-Match": `W/"` + testEtag + `"`})
	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want %d (weak comparison)", rec.Code, http.StatusNotModified)
	}
}

func TestProxy_IfNoneMatch_ListMatch(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-None-Match": `"nope", W/"other", "` + testEtag + `"`})
	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
}

func TestProxy_IfNoneMatch_Star(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
}

func TestProxy_IfNoneMatch_Mismatch(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-None-Match": `"stale"`})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_IfNoneMatch_MultipleHeaderLines(t *testing.T) {
	s := newETagServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/"+testObject, nil)
	req.Header.Add("If-None-Match", `"a"`)
	req.Header.Add("If-None-Match", `"`+testEtag+`"`)
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want %d (all header lines must be scanned)", rec.Code, http.StatusNotModified)
	}
}

func TestProxy_IfNoneMatch_Malformed(t *testing.T) {
	s := newETagServer(t)
	// Unquoted value is not a valid entity-tag: treated as no-match.
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-None-Match": testEtag})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestProxy_IfNoneMatch_HEAD(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodHead, map[string]string{"If-None-Match": `"` + testEtag + `"`})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", rec.Body.String())
	}
}

func TestProxy_IfNoneMatch_GzipVariants(t *testing.T) {
	const plaintext = "gzip me please, gcsproxy"
	cases := []struct {
		name    string
		inm     string
		headers map[string]string
	}{
		{"weak tag with gzip accept", `W/"` + testEtag + `"`, map[string]string{"Accept-Encoding": "gzip"}},
		{"weak tag without accept-encoding", `W/"` + testEtag + `"`, nil},
		{"strong-form tag still matches weakly", `"` + testEtag + `"`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newGzipETagServer(t, plaintext)
			headers := map[string]string{"If-None-Match": tc.inm}
			for k, v := range tc.headers {
				headers[k] = v
			}
			rec := doRequest(t, s, http.MethodGet, headers)
			if rec.Code != http.StatusNotModified {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotModified)
			}
			if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("Vary = %q, want %q (304 for gzip object)", got, "Accept-Encoding")
			}
		})
	}
}

func TestProxy_IfNoneMatch_TakesPrecedenceOverIMS(t *testing.T) {
	t.Run("INM mismatch ignores satisfied IMS", func(t *testing.T) {
		s := newETagServer(t)
		rec := doRequest(t, s, http.MethodGet, map[string]string{
			"If-None-Match":     `"stale"`,
			"If-Modified-Since": testUpdated.Format(http.TimeFormat),
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (IMS must be ignored when INM present)", rec.Code, http.StatusOK)
		}
		if got := rec.Body.String(); got != testContent {
			t.Errorf("body = %q, want %q", got, testContent)
		}
	})

	t.Run("INM match wins over stale IMS", func(t *testing.T) {
		s := newETagServer(t)
		rec := doRequest(t, s, http.MethodGet, map[string]string{
			"If-None-Match":     `"` + testEtag + `"`,
			"If-Modified-Since": testUpdated.Add(-time.Hour).Format(http.TimeFormat),
		})
		if rec.Code != http.StatusNotModified {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotModified)
		}
	})
}

func TestProxy_IfModifiedSince_304_IncludesValidatorHeaders(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"If-Modified-Since": testUpdated.Format(http.TimeFormat),
	})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
	if got := rec.Header().Get("ETag"); got != `"`+testEtag+`"` {
		t.Errorf("ETag = %q, want %q", got, `"`+testEtag+`"`)
	}
	if got := rec.Header().Get("Last-Modified"); got != testUpdated.Format(http.TimeFormat) {
		t.Errorf("Last-Modified = %q, want %q", got, testUpdated.Format(http.TimeFormat))
	}
	if got := rec.Header().Get("Cache-Control"); got != testCacheCtl {
		t.Errorf("Cache-Control = %q, want %q", got, testCacheCtl)
	}
	// Representation headers must be absent on 304.
	if got := rec.Header().Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q, want empty on 304", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "" {
		t.Errorf("Accept-Ranges = %q, want empty on 304", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", rec.Body.String())
	}
}

func TestProxy_Range_IfNoneMatchMatch_Returns304(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"Range":         "bytes=0-3",
		"If-None-Match": `"` + testEtag + `"`,
	})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d (preconditions run before Range)", rec.Code, http.StatusNotModified)
	}
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Errorf("Content-Range = %q, want empty on 304", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", rec.Body.String())
	}
}

func TestProxy_IfRange_ETagMatch_Serves206(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"Range":    "bytes=2-5",
		"If-Range": `"` + testEtag + `"`,
	})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Body.String(); got != testContent[2:6] {
		t.Errorf("body = %q, want %q", got, testContent[2:6])
	}
}

func TestProxy_IfRange_ETagMismatch_ServesFull200(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"Range":    "bytes=2-5",
		"If-Range": `"old"`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (changed validator must disable Range)", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want full body %q", got, testContent)
	}
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Errorf("Content-Range = %q, want empty", got)
	}
}

func TestProxy_IfRange_WeakETag_ServesFull200(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"Range":    "bytes=2-5",
		"If-Range": `W/"` + testEtag + `"`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (If-Range requires strong comparison)", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want full body %q", got, testContent)
	}
}

func TestProxy_IfRange_DateMatch_Serves206(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"Range":    "bytes=2-5",
		"If-Range": testUpdated.Format(http.TimeFormat),
	})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Body.String(); got != testContent[2:6] {
		t.Errorf("body = %q, want %q", got, testContent[2:6])
	}
}

func TestProxy_IfRange_DateMismatch_ServesFull200(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"Range":    "bytes=2-5",
		"If-Range": testUpdated.Add(-time.Hour).Format(http.TimeFormat),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (non-matching date must disable Range)", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want full body %q", got, testContent)
	}
}

func TestProxy_IfMatch_Match_Serves200(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-Match": `"` + testEtag + `"`})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_IfMatch_Mismatch_Returns412(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-Match": `"stale"`})
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusPreconditionFailed)
	}
}

func TestProxy_IfMatch_Star_Serves200(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestProxy_IfMatch_WeakForm_Returns412(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-Match": `W/"` + testEtag + `"`})
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want %d (If-Match requires strong comparison)", rec.Code, http.StatusPreconditionFailed)
	}
}

func TestProxy_IfUnmodifiedSince_Satisfied_Serves200(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"If-Unmodified-Since": testUpdated.Format(http.TimeFormat),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_IfUnmodifiedSince_Stale_Returns412(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"If-Unmodified-Since": testUpdated.Add(-time.Hour).Format(http.TimeFormat),
	})
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusPreconditionFailed)
	}
}

func TestProxy_IfMatch_PrecedenceOverIfUnmodifiedSince(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"If-Match":            `"` + testEtag + `"`,
		"If-Unmodified-Since": testUpdated.Add(-time.Hour).Format(http.TimeFormat),
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (IUS must be ignored when If-Match present)", rec.Code, http.StatusOK)
	}
}

func TestProxy_IfUnmodifiedSince_InvalidDate_Ignored(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"If-Unmodified-Since": "not-a-date",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (invalid HTTP-date must be ignored)", rec.Code, http.StatusOK)
	}
}

func TestProxy_IfMatch_EmptyHeader_TreatedAsAbsent(t *testing.T) {
	// A present-but-empty If-Match must not fail the precondition
	// (net/http semantics for invalid field syntax).
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{"If-Match": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_IfNoneMatch_EmptyHeader_DoesNotDisableIMS(t *testing.T) {
	// A present-but-empty If-None-Match is treated as absent, so a matching
	// If-Modified-Since must still produce a 304.
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"If-None-Match":     "",
		"If-Modified-Since": testUpdated.Format(http.TimeFormat),
	})
	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
}

func TestProxy_IfModifiedSince_InvalidDate_Ignored(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"If-Modified-Since": "not-a-date",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (invalid HTTP-date must be ignored)", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != testContent {
		t.Errorf("body = %q, want %q", got, testContent)
	}
}

func TestProxy_Range_IfMatchMismatch_Returns412(t *testing.T) {
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"Range":    "bytes=2-5",
		"If-Match": `"stale"`,
	})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d (preconditions run before Range)", rec.Code, http.StatusPreconditionFailed)
	}
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Errorf("Content-Range = %q, want empty on 412", got)
	}
}

func TestProxy_IfRange_MatchButRangeUnsatisfiable_Returns416(t *testing.T) {
	// If-Range only decides whether Range is honored; an in-date validator
	// with an out-of-bounds range must still produce 416.
	s := newETagServer(t)
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"Range":    "bytes=9999-",
		"If-Range": `"` + testEtag + `"`,
	})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestedRangeNotSatisfiable)
	}
	if got := rec.Header().Get("Content-Range"); !strings.HasPrefix(got, "bytes */") {
		t.Errorf("Content-Range = %q, want bytes */<size>", got)
	}
}

func TestProxy_IfUnmodifiedSince_ExactSecondBoundary(t *testing.T) {
	// Last-Modified truncates to whole seconds; sub-second object precision
	// must not turn an equal-second IUS into a spurious 412.
	updated := testUpdated.Add(300 * time.Millisecond)
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName: testBucket,
			Name:       testObject,
			Etag:       testEtag,
			Updated:    updated,
		},
		Content: []byte(testContent),
	}})
	rec := doRequest(t, s, http.MethodGet, map[string]string{
		"If-Unmodified-Since": updated.Truncate(time.Second).Format(http.TimeFormat),
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestProxy_DateFormats(t *testing.T) {
	// http.ParseTime accepts the three HTTP-date formats of RFC 9110 §5.6.7.
	formats := []struct {
		name   string
		layout string
	}{
		{"IMF-fixdate", http.TimeFormat},
		{"RFC850", time.RFC850},
		{"asctime", time.ANSIC},
	}
	for _, f := range formats {
		t.Run(f.name, func(t *testing.T) {
			t.Run("If-Modified-Since 304", func(t *testing.T) {
				s := newETagServer(t)
				rec := doRequest(t, s, http.MethodGet, map[string]string{
					"If-Modified-Since": testUpdated.Format(f.layout),
				})
				if rec.Code != http.StatusNotModified {
					t.Errorf("status = %d, want %d", rec.Code, http.StatusNotModified)
				}
			})
			t.Run("If-Unmodified-Since 412", func(t *testing.T) {
				s := newETagServer(t)
				rec := doRequest(t, s, http.MethodGet, map[string]string{
					"If-Unmodified-Since": testUpdated.Add(-time.Hour).Format(f.layout),
				})
				if rec.Code != http.StatusPreconditionFailed {
					t.Errorf("status = %d, want %d", rec.Code, http.StatusPreconditionFailed)
				}
			})
			t.Run("If-Range 206", func(t *testing.T) {
				s := newETagServer(t)
				rec := doRequest(t, s, http.MethodGet, map[string]string{
					"Range":    "bytes=2-5",
					"If-Range": testUpdated.Format(f.layout),
				})
				if rec.Code != http.StatusPartialContent {
					t.Errorf("status = %d, want %d", rec.Code, http.StatusPartialContent)
				}
			})
		})
	}
}

func TestCheckPreconditions_ZeroUpdated(t *testing.T) {
	// Unknown (zero) modification time disables the date conditionals
	// instead of producing a bogus 304/412. The fake always assigns an
	// Updated time, so this is exercised at the unit level.
	s := &Server{}
	attrs := &storage.ObjectAttrs{Etag: testEtag}
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"If-Modified-Since ignored", "If-Modified-Since", testUpdated.Format(http.TimeFormat)},
		{"If-Unmodified-Since ignored", "If-Unmodified-Since", testUpdated.Add(-time.Hour).Format(http.TimeFormat)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set(tc.key, tc.value)
			if s.checkPreconditions(rec, req, attrs) {
				t.Errorf("checkPreconditions wrote %d, want pass-through for zero Updated", rec.Code)
			}
		})
	}

	t.Run("If-None-Match still validates", func(t *testing.T) {
		// ETag validation is independent of the modification time: a match
		// must 304 even with zero Updated, just without a Last-Modified.
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("If-None-Match", `"`+testEtag+`"`)
		if !s.checkPreconditions(rec, req, attrs) {
			t.Fatal("checkPreconditions = false, want 304 written")
		}
		if rec.Code != http.StatusNotModified {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotModified)
		}
		if got := rec.Header().Get("Last-Modified"); got != "" {
			t.Errorf("Last-Modified = %q, want empty for zero Updated", got)
		}
	})
}

func TestCheckPreconditions_InvalidDate_VerboseWarnLogged(t *testing.T) {
	s := &Server{verbose: true}
	attrs := &storage.ObjectAttrs{Etag: testEtag, Updated: testUpdated}
	cases := []struct {
		key     string
		wantLog string
	}{
		{"If-Unmodified-Since", "If-Unmodified-Since"},
		{"If-Modified-Since", "If-Modified-Since"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			var buf bytes.Buffer
			installLogger(t, slog.NewTextHandler(&buf, nil))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set(tc.key, "not-a-date")
			if s.checkPreconditions(rec, req, attrs) {
				t.Errorf("checkPreconditions wrote %d, want pass-through for invalid date", rec.Code)
			}
			if !strings.Contains(buf.String(), tc.wantLog) {
				t.Errorf("log output %q does not mention %q", buf.String(), tc.wantLog)
			}
		})
	}
}

func TestProxy_E2E_DateHeader(t *testing.T) {
	// The Date header is added by net/http's server, which recorder-based
	// tests bypass — so drive a real server.
	s := newETagServer(t)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	get := func(t *testing.T, inm string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/"+testBucket+"/"+testObject, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	t.Run("200", func(t *testing.T) {
		resp := get(t, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if resp.Header.Get("Date") == "" {
			t.Error("Date header is missing on 200")
		}
	})

	t.Run("304", func(t *testing.T) {
		resp := get(t, `"`+testEtag+`"`)
		if resp.StatusCode != http.StatusNotModified {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotModified)
		}
		if resp.Header.Get("Date") == "" {
			t.Error("Date header is missing on 304")
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if len(body) != 0 {
			t.Errorf("body should be empty, got %q", body)
		}
	})
}

func TestProxy_SPA_Fallback_Conditional(t *testing.T) {
	newSPAServer := func(t *testing.T) *Server {
		t.Helper()
		s := newTestServer(t, []fakestorage.Object{{
			ObjectAttrs: fakestorage.ObjectAttrs{
				BucketName:  testBucket,
				Name:        "index.html",
				ContentType: "text/html",
				Etag:        "idx1",
				Updated:     testUpdated,
			},
			Content: []byte(testIndexBody),
		}})
		s.defaultIndex = "index.html"
		s.spa = true
		return s
	}

	t.Run("first fetch carries index ETag", func(t *testing.T) {
		s := newSPAServer(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/some/spa/route", nil)
		s.handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("ETag"); got != `"idx1"` {
			t.Errorf("ETag = %q, want %q", got, `"idx1"`)
		}
	})

	t.Run("revalidation returns 304", func(t *testing.T) {
		s := newSPAServer(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/some/spa/route", nil)
		req.Header.Set("If-None-Match", `"idx1"`)
		s.handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotModified)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body should be empty, got %q", rec.Body.String())
		}
	})
}

func TestProxy_NotFound_Fallback_IgnoresConditionals(t *testing.T) {
	const notFoundBody = "<html>oops</html>"
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "404.html",
			ContentType: "text/html",
			Etag:        "nf1",
			Updated:     testUpdated,
		},
		Content: []byte(notFoundBody),
	}})
	s.notFoundPath = "404.html"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/missing.txt", nil)
	// Preconditions apply only to would-be-2xx responses: even validators
	// matching the 404 page itself must not turn the 404 into a 304/412.
	req.Header.Set("If-None-Match", `"nf1"`)
	req.Header.Set("If-Modified-Since", testUpdated.Format(http.TimeFormat))
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Body.String(); got != notFoundBody {
		t.Errorf("body = %q, want %q", got, notFoundBody)
	}
}

func TestProxy_DefaultIndex_ServesResolvedIndexETag(t *testing.T) {
	newIndexServer := func(t *testing.T) *Server {
		t.Helper()
		s := newTestServer(t, []fakestorage.Object{{
			ObjectAttrs: fakestorage.ObjectAttrs{
				BucketName:  testBucket,
				Name:        "foo/index.html",
				ContentType: "text/html",
				Etag:        "idx1",
				Updated:     testUpdated,
			},
			Content: []byte(testIndexBody),
		}})
		s.defaultIndex = "index.html"
		return s
	}

	t.Run("resolved index carries its ETag", func(t *testing.T) {
		s := newIndexServer(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/foo/", nil)
		s.handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
		}
		if got := rec.Header().Get("ETag"); got != `"idx1"` {
			t.Errorf("ETag = %q, want %q", got, `"idx1"`)
		}
	})

	t.Run("revalidation returns 304", func(t *testing.T) {
		s := newIndexServer(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/foo/", nil)
		req.Header.Set("If-None-Match", `"idx1"`)
		s.handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotModified)
		}
	})
}

func TestProxy_WalkUpIndex_Conditional(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        "foo/index.html",
			ContentType: "text/html",
			Etag:        "idx1",
			Updated:     testUpdated,
		},
		Content: []byte(testIndexBody),
	}})
	s.defaultIndex = "index.html"
	s.walkUpIndex = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/"+testBucket+"/foo/bar/search", nil)
	req.Header.Set("If-None-Match", `"idx1"`)
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d (walk-up resolved index must revalidate)", rec.Code, http.StatusNotModified)
	}
	if got := rec.Header().Get("ETag"); got != `"idx1"` {
		t.Errorf("ETag = %q, want %q", got, `"idx1"`)
	}
}

func TestProxy_GenerationPinnedRead_Smoke(t *testing.T) {
	// Readers are pinned to the attrs' generation; both reader types must
	// round-trip the generation parameter through the (fake) GCS API.
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName: testBucket,
			Name:       testObject,
			Etag:       testEtag,
			Generation: 1234,
		},
		Content: []byte(testContent),
	}})

	t.Run("full read", func(t *testing.T) {
		rec := doRequest(t, s, http.MethodGet, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
		}
		if got := rec.Body.String(); got != testContent {
			t.Errorf("body = %q, want %q", got, testContent)
		}
	})

	t.Run("range read", func(t *testing.T) {
		rec := doRequest(t, s, http.MethodGet, map[string]string{"Range": "bytes=2-5"})
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusPartialContent, rec.Body.String())
		}
		if got := rec.Body.String(); got != testContent[2:6] {
			t.Errorf("body = %q, want %q", got, testContent[2:6])
		}
	})
}
