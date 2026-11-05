package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"google.golang.org/api/option"
)

const (
	testBucket    = "test-bucket"
	testObject    = "path/to/file.txt"
	testContent   = "hello gcsproxy"
	testCType     = "text/plain"
	testIndexBody = "<html>index</html>"
	testEtag      = "v1abc"
	testCacheCtl  = "max-age=60"
)

var testUpdated = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func newTestServer(t *testing.T, objects []fakestorage.Object) *Server {
	t.Helper()
	s, _ := newFaultyServer(t, objects, nil)
	return s
}

// gcsOp classifies a request the storage client sends to GCS.
type gcsOp int

const (
	opAttrs gcsOp = iota
	opRead
)

// gcsCalls counts the requests per operation that reached the fake.
type gcsCalls struct {
	mu sync.Mutex
	n  map[gcsOp]int
}

func (c *gcsCalls) get(op gcsOp) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[op]
}

// newFaultyServer is newTestServer with a hook in front of the fake: for the
// nth request (1-based) of each operation, fault returns an HTTP status to
// answer with instead, or 0 to pass the request through.
func newFaultyServer(t *testing.T, objects []fakestorage.Object, fault func(op gcsOp, n int) int) (*Server, *gcsCalls) {
	t.Helper()
	fakeSrv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		InitialObjects: objects,
		Scheme:         "http",
		Host:           "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("fakestorage.NewServerWithOptions: %v", err)
	}
	t.Cleanup(fakeSrv.Stop)

	calls := &gcsCalls{n: map[gcsOp]int{}}
	base := fakeSrv.HTTPClient().Transport
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		op := opRead
		if strings.HasPrefix(r.URL.Path, "/storage/v1/") && r.URL.Query().Get("alt") != "media" {
			op = opAttrs
		}
		calls.mu.Lock()
		calls.n[op]++
		n := calls.n[op]
		calls.mu.Unlock()
		if fault != nil {
			if status := fault(op, n); status != 0 {
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"code":` + http.StatusText(status) + `}}`)),
					Request:    r,
				}, nil
			}
		}
		return base.RoundTrip(r)
	})

	c, err := storage.NewClient(t.Context(), option.WithHTTPClient(&http.Client{Transport: rt}), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// Injected 5xx would otherwise be retried with backoff.
	c.SetRetry(storage.WithPolicy(storage.RetryNever))
	return &Server{client: c}, calls
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// object builds a fake object in testBucket.
func object(name, content string, opts ...func(*fakestorage.ObjectAttrs)) fakestorage.Object {
	attrs := fakestorage.ObjectAttrs{BucketName: testBucket, Name: name}
	for _, o := range opts {
		o(&attrs)
	}
	return fakestorage.Object{ObjectAttrs: attrs, Content: []byte(content)}
}

func withType(ct string) func(*fakestorage.ObjectAttrs) {
	return func(a *fakestorage.ObjectAttrs) { a.ContentType = ct }
}

func withEncoding(enc string) func(*fakestorage.ObjectAttrs) {
	return func(a *fakestorage.ObjectAttrs) { a.ContentEncoding = enc }
}

func withValidators(etag string, updated time.Time) func(*fakestorage.ObjectAttrs) {
	return func(a *fakestorage.ObjectAttrs) { a.Etag, a.Updated = etag, updated }
}

func withCacheControl(cc string) func(*fakestorage.ObjectAttrs) {
	return func(a *fakestorage.ObjectAttrs) { a.CacheControl = cc }
}

// testFile is the object most tests serve: testObject with fixed validators.
func testFile() fakestorage.Object {
	return object(testObject, testContent, withType(testCType), withCacheControl(testCacheCtl), withValidators(testEtag, testUpdated))
}

func gzipped(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.String()
}

// do sends a request through the full handler stack.
func do(t *testing.T, s *Server, method, target string, h http.Header) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	s.handler().ServeHTTP(rec, req)
	return rec
}

// hdr builds an http.Header from key/value pairs; a repeated key adds a field
// line.
func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

const filePath = "/" + testBucket + "/" + testObject

// assertResponse checks the status, the body (unless wantBody is nil) and each
// header in want; an empty header value means the header must be absent.
func assertResponse(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantBody *string, want map[string]string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, wantStatus, rec.Body.String())
	}
	if wantBody != nil && rec.Body.String() != *wantBody {
		t.Errorf("body = %q, want %q", rec.Body.String(), *wantBody)
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func ptr(s string) *string { return &s }

// captureLogs routes slog.Default() to a JSON buffer at level for the test.
func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func logEntries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("log line is not JSON: %v\nraw: %s", err, line)
		}
		entries = append(entries, e)
	}
	return entries
}

// findLog returns the first entry with the given msg, or nil.
func findLog(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for _, e := range logEntries(t, buf) {
		if e["msg"] == msg {
			return e
		}
	}
	return nil
}

func TestIndexCandidates(t *testing.T) {
	cases := []struct {
		name   string
		object string
		walkUp bool
		want   []string
	}{
		{"single candidate", "foo/bar", false, []string{"foo/bar/index.html"}},
		{"walk up path and root", "foo/bar/baz", true, []string{"foo/bar/baz/index.html", "foo/bar/index.html", "foo/index.html"}},
		{"walk up from trailing slash", "foo/bar/", true, []string{"foo/bar/index.html", "foo/index.html"}},
		{"single segment walk up is unchanged", "search", true, []string{"search/index.html"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := indexCandidates(tc.object, "index.html", tc.walkUp)
			if err != nil {
				t.Fatalf("indexCandidates: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("indexCandidates(%q, walkUp=%v) = %v, want %v", tc.object, tc.walkUp, got, tc.want)
			}
		})
	}
}

// TestProxy_Routing covers how a request path resolves to an object across
// the -bucket, -i, -walk-up-index, -spa and -not-found modes.
func TestProxy_Routing(t *testing.T) {
	const notFoundBody = "<html>oops</html>"
	file := object(testObject, testContent, withType(testCType))
	rootIndex := object("index.html", testIndexBody, withType("text/html"))
	fooIndex := object("foo/index.html", testIndexBody, withType("text/html"))
	siteIndex := object("site/index.html", testIndexBody, withType("text/html"))
	notFoundPage := object("404.html", notFoundBody, withType("text/html"))

	type opts struct {
		bucket   bool
		index    bool
		walkUp   bool
		spa      bool
		notFound bool
	}
	cases := []struct {
		name     string
		objects  []fakestorage.Object
		opts     opts
		path     string
		wantCode int
		wantBody string
		wantType string
	}{
		{"object", []fakestorage.Object{file}, opts{}, filePath, 200, testContent, testCType},
		{"missing object", []fakestorage.Object{file}, opts{}, "/" + testBucket + "/missing.txt", 404, "", ""},
		{"health route", nil, opts{}, "/_health", 200, "OK\n", "text/plain"},

		{"index at bucket root", []fakestorage.Object{rootIndex}, opts{index: true}, "/" + testBucket + "/", 200, testIndexBody, "text/html"},
		{"index in subdirectory", []fakestorage.Object{fooIndex}, opts{index: true}, "/" + testBucket + "/foo/", 200, testIndexBody, "text/html"},
		{"index without trailing slash", []fakestorage.Object{fooIndex}, opts{index: true}, "/" + testBucket + "/foo", 200, testIndexBody, "text/html"},
		{"no walk-up by default", []fakestorage.Object{siteIndex}, opts{index: true}, "/" + testBucket + "/site/search", 404, "", ""},
		{"walk-up to parent index", []fakestorage.Object{siteIndex}, opts{index: true, walkUp: true}, "/" + testBucket + "/site/search", 200, testIndexBody, "text/html"},

		{"spa route serves root index", []fakestorage.Object{rootIndex}, opts{index: true, spa: true}, "/" + testBucket + "/some/spa/route", 200, testIndexBody, "text/html"},
		{"spa keeps real objects", []fakestorage.Object{file, rootIndex}, opts{index: true, spa: true}, filePath, 200, testContent, testCType},
		{"spa without root index", nil, opts{index: true, spa: true}, "/" + testBucket + "/whatever", 404, "", ""},

		{"not-found page", []fakestorage.Object{notFoundPage}, opts{notFound: true}, "/" + testBucket + "/missing.txt", 404, notFoundBody, "text/html"},
		{"not-found page skips real objects", []fakestorage.Object{file, notFoundPage}, opts{notFound: true}, filePath, 200, testContent, testCType},
		{"not-found page missing", nil, opts{notFound: true}, "/" + testBucket + "/missing.txt", 404, "", ""},

		{"bucket mode object", []fakestorage.Object{file}, opts{bucket: true}, "/" + testObject, 200, testContent, testCType},
		{"bucket mode missing", []fakestorage.Object{file}, opts{bucket: true}, "/missing.txt", 404, "", ""},
		{"bucket mode first segment is not a bucket", []fakestorage.Object{object("other-bucket/file.txt", testContent)}, opts{bucket: true}, "/other-bucket/file.txt", 200, testContent, ""},
		{"bucket mode root index", []fakestorage.Object{rootIndex}, opts{bucket: true, index: true}, "/", 200, testIndexBody, "text/html"},
		{"bucket mode spa", []fakestorage.Object{rootIndex}, opts{bucket: true, index: true, spa: true}, "/deep/nested/route", 200, testIndexBody, "text/html"},
		{"bucket mode health route", nil, opts{bucket: true}, "/_health", 200, "OK\n", "text/plain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, tc.objects)
			if tc.opts.bucket {
				s.sourceBucket = testBucket
			}
			if tc.opts.index {
				s.defaultIndex = "index.html"
			}
			s.walkUpIndex = tc.opts.walkUp
			s.spa = tc.opts.spa
			if tc.opts.notFound {
				s.notFoundPath = "404.html"
			}

			rec := do(t, s, http.MethodGet, tc.path, nil)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
			if tc.wantType != "" && rec.Header().Get("Content-Type") != tc.wantType {
				t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), tc.wantType)
			}
		})
	}
}

// TestProxy_BehindPrefixRewritingProxy runs gcsproxy behind a reverse proxy
// that maps "/" onto a bucket prefix, as a load balancer URL map would.
func TestProxy_BehindPrefixRewritingProxy(t *testing.T) {
	for _, tc := range []struct {
		walkUp   bool
		wantCode int
	}{
		{false, http.StatusNotFound},
		{true, http.StatusOK},
	} {
		t.Run("walkUp="+map[bool]string{false: "off", true: "on"}[tc.walkUp], func(t *testing.T) {
			s := newTestServer(t, []fakestorage.Object{object("site/index.html", testIndexBody)})
			s.defaultIndex = "index.html"
			s.walkUpIndex = tc.walkUp
			s.notFoundPath = "404.html"

			upstream := httptest.NewServer(s.handler())
			t.Cleanup(upstream.Close)
			upstreamURL, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(upstreamURL)
				pr.Out.URL.Path = "/" + testBucket + "/site" + pr.In.URL.Path
				pr.Out.URL.RawPath = ""
			}}
			front := httptest.NewServer(rp)
			t.Cleanup(front.Close)

			res, err := http.Get(front.URL + "/search?q=test")
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%q)", res.StatusCode, tc.wantCode, body)
			}
			if tc.wantCode == http.StatusOK && string(body) != testIndexBody {
				t.Errorf("body = %q, want %q", body, testIndexBody)
			}
		})
	}
}

// TestProxy_ResponseHeaders covers the headers forwarded from object metadata
// and the -content-length / -cors-origin options, for GET and HEAD alike.
func TestProxy_ResponseHeaders(t *testing.T) {
	full := object(testObject, testContent,
		withType(testCType), withCacheControl(testCacheCtl), withValidators(testEtag, testUpdated),
		func(a *fakestorage.ObjectAttrs) {
			a.ContentLanguage = "en"
			a.ContentDisposition = "attachment"
		})
	contentLen := strconv.Itoa(len(testContent))

	cases := []struct {
		name          string
		method        string
		contentLength bool
		cors          string
		path          string
		wantCode      int
		want          map[string]string
	}{
		{"metadata on GET", http.MethodGet, false, "", filePath, 200, map[string]string{
			"Content-Type":        testCType,
			"Content-Language":    "en",
			"Content-Disposition": "attachment",
			"Cache-Control":       testCacheCtl,
			"Last-Modified":       testUpdated.Format(http.TimeFormat),
			"ETag":                `"` + testEtag + `"`,
			"Accept-Ranges":       "bytes",
			"Content-Length":      "",
			"Vary":                "",
		}},
		{"metadata on HEAD", http.MethodHead, false, "", filePath, 200, map[string]string{
			"Content-Type":   testCType,
			"ETag":           `"` + testEtag + `"`,
			"Accept-Ranges":  "bytes",
			"Content-Length": "",
		}},
		{"content-length on GET", http.MethodGet, true, "", filePath, 200, map[string]string{"Content-Length": contentLen}},
		{"content-length on HEAD", http.MethodHead, true, "", filePath, 200, map[string]string{"Content-Length": contentLen}},
		{"cors", http.MethodGet, false, "https://example.com", filePath, 200, map[string]string{"Access-Control-Allow-Origin": "https://example.com"}},
		{"cors on errors", http.MethodGet, false, "*", "/" + testBucket + "/missing", 404, map[string]string{"Access-Control-Allow-Origin": "*"}},
		{"no cors by default", http.MethodGet, false, "", filePath, 200, map[string]string{"Access-Control-Allow-Origin": ""}},
		{"HEAD on missing object", http.MethodHead, false, "", "/" + testBucket + "/missing", 404, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, []fakestorage.Object{full})
			s.contentLength = tc.contentLength
			s.corsOrigin = tc.cors

			rec := do(t, s, tc.method, tc.path, nil)
			var wantBody *string
			if tc.wantCode == http.StatusOK {
				wantBody = ptr(testContent)
				if tc.method == http.MethodHead {
					wantBody = ptr("")
				}
			}
			assertResponse(t, rec, tc.wantCode, wantBody, tc.want)
		})
	}
}

// TestProxy_ContentEncoding covers gzip-stored objects, which GCS serves raw
// or transcoded depending on Accept-Encoding, and other stored encodings,
// which are passed through untouched.
func TestProxy_ContentEncoding(t *testing.T) {
	const plaintext = "gzip me please, gcsproxy"
	zipped := gzipped(t, plaintext)
	weakETag := `W/"` + testEtag + `"`

	cases := []struct {
		name     string
		encoding string
		content  string
		method   string
		accept   string
		wantBody *string
		want     map[string]string
	}{
		{"gzip accepted", "gzip", zipped, http.MethodGet, "gzip", ptr(zipped), map[string]string{
			"Content-Encoding": "gzip", "ETag": weakETag, "Vary": "Accept-Encoding", "Accept-Ranges": "",
		}},
		{"gzip not accepted is transcoded", "gzip", zipped, http.MethodGet, "", ptr(plaintext), map[string]string{
			"Content-Encoding": "", "ETag": weakETag, "Vary": "Accept-Encoding", "Accept-Ranges": "",
		}},
		{"gzip refused with q=0 is transcoded", "gzip", zipped, http.MethodGet, "gzip;q=0", ptr(plaintext), map[string]string{
			"Content-Encoding": "",
		}},
		{"HEAD gzip accepted", "gzip", zipped, http.MethodHead, "gzip", ptr(""), map[string]string{
			"Content-Encoding": "gzip", "Content-Length": strconv.Itoa(len(zipped)), "Vary": "Accept-Encoding",
		}},
		{"HEAD gzip not accepted", "gzip", zipped, http.MethodHead, "", ptr(""), map[string]string{
			"Content-Encoding": "", "Content-Length": "",
		}},
		{"other encodings pass through", "br", "brotli-bytes", http.MethodGet, "", ptr("brotli-bytes"), map[string]string{
			"Content-Encoding": "br", "ETag": `"` + testEtag + `"`, "Vary": "", "Accept-Ranges": "bytes",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, []fakestorage.Object{
				object(testObject, tc.content, withType(testCType), withEncoding(tc.encoding), withValidators(testEtag, testUpdated)),
			})
			s.contentLength = true
			var h http.Header
			if tc.accept != "" {
				h = hdr("Accept-Encoding", tc.accept)
			}
			assertResponse(t, do(t, s, tc.method, filePath, h), http.StatusOK, tc.wantBody, tc.want)
		})
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   bool
	}{
		{"absent", nil, false},
		{"gzip", []string{"gzip"}, true},
		{"list", []string{"br, gzip, deflate"}, true},
		{"case and spaces", []string{"  GZip ;Q=0.5 "}, true},
		{"x-gzip", []string{"x-gzip"}, true},
		{"q=0 refuses", []string{"gzip;q=0"}, false},
		{"q=0.0 refuses", []string{"br, gzip;q=0.0"}, false},
		{"wildcard", []string{"*"}, true},
		{"wildcard refused", []string{"*;q=0"}, false},
		{"explicit gzip beats wildcard", []string{"*, gzip;q=0"}, false},
		{"explicit gzip beats refused wildcard", []string{"*;q=0, gzip"}, true},
		{"other codings only", []string{"br, deflate"}, false},
		{"multiple field lines", []string{"br", "gzip"}, true},
		{"unparsable q keeps default", []string{"gzip;q=abc"}, true},
		{"malformed parameter keeps default", []string{"gzip;q"}, true},
		{"empty members", []string{", ,gzip"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptsGzip(tc.values); got != tc.want {
				t.Errorf("acceptsGzip(%q) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

func TestHeaderHelpers(t *testing.T) {
	t.Run("header", func(t *testing.T) {
		cases := []struct {
			name    string
			headers http.Header
			wantVal string
			wantOk  bool
		}{
			{"nil header", nil, "", false},
			{"missing key", http.Header{"X-Bar": {"v"}}, "", false},
			{"present", http.Header{"X-Foo": {"v"}}, "v", true},
			{"empty values slice", http.Header{"X-Foo": {}}, "", false},
			{"first of many", http.Header{"X-Foo": {"a", "b"}}, "a", true},
		}
		for _, tc := range cases {
			got, ok := header(&http.Request{Header: tc.headers}, "X-Foo")
			if got != tc.wantVal || ok != tc.wantOk {
				t.Errorf("%s: header() = (%q, %v), want (%q, %v)", tc.name, got, ok, tc.wantVal, tc.wantOk)
			}
		}
	})

	t.Run("setters skip zero values", func(t *testing.T) {
		w := httptest.NewRecorder()
		setStrHeader(w, "X-Str-Empty", "")
		setStrHeader(w, "X-Str", "value")
		setIntHeader(w, "X-Int-Zero", 0)
		setIntHeader(w, "X-Int-Negative", -1)
		setIntHeader(w, "X-Int", 123)
		setTimeHeader(w, "X-Time-Zero", time.Time{})
		setTimeHeader(w, "X-Time", testUpdated)
		want := map[string]string{
			"X-Str-Empty": "", "X-Str": "value",
			"X-Int-Zero": "", "X-Int-Negative": "", "X-Int": "123",
			"X-Time-Zero": "", "X-Time": testUpdated.Format(http.TimeFormat),
		}
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
	})
}

func TestParseSingleRange(t *testing.T) {
	const size = 1000
	cases := []struct {
		name       string
		header     string
		size       int64
		wantErr    error
		wantStart  int64
		wantLength int64
	}{
		{"explicit", "bytes=0-499", size, nil, 0, 500},
		{"explicit middle", "bytes=100-199", size, nil, 100, 100},
		{"single byte", "bytes=0-0", size, nil, 0, 1},
		{"open ended", "bytes=500-", size, nil, 500, 500},
		{"suffix", "bytes=-200", size, nil, 800, 200},
		{"suffix larger than size", "bytes=-2000", size, nil, 0, 1000},
		{"end clamped", "bytes=900-2000", size, nil, 900, 100},
		{"multi-range uses first", "bytes=0-99,200-299", size, nil, 0, 100},

		{"non-bytes unit", "items=0-10", size, errRangeIgnore, 0, 0},
		{"missing prefix", "0-99", size, errRangeIgnore, 0, 0},
		{"empty", "", size, errRangeIgnore, 0, 0},
		{"no dash", "bytes=100", size, errRangeIgnore, 0, 0},
		{"zero-length suffix", "bytes=-0", size, errRangeIgnore, 0, 0},
		{"negative start", "bytes=--5", size, errRangeIgnore, 0, 0},
		{"non-numeric start", "bytes=abc-100", size, errRangeIgnore, 0, 0},
		{"non-numeric end", "bytes=0-def", size, errRangeIgnore, 0, 0},

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

// TestProxy_Range drives Range requests through the handler for GET and HEAD.
func TestProxy_Range(t *testing.T) {
	const body = "0123456789abcdef"

	cases := []struct {
		name          string
		encoding      string
		contentLength bool
		bucketMode    bool
		rng           string
		wantCode      int
		wantBody      string
		want          map[string]string
	}{
		{"explicit", "", false, false, "bytes=2-5", 206, "2345", map[string]string{
			"Content-Range": "bytes 2-5/16", "Accept-Ranges": "bytes", "Content-Length": "",
		}},
		{"suffix", "", false, false, "bytes=-4", 206, "cdef", map[string]string{"Content-Range": "bytes 12-15/16"}},
		{"open ended", "", false, false, "bytes=10-", 206, "abcdef", map[string]string{"Content-Range": "bytes 10-15/16"}},
		{"content-length opt-in", "", true, false, "bytes=2-5", 206, "2345", map[string]string{"Content-Length": "4"}},
		{"bucket mode", "", false, true, "bytes=0-3", 206, "0123", map[string]string{"Content-Range": "bytes 0-3/16"}},
		{"unsatisfiable", "", false, false, "bytes=1000-2000", 416, "", map[string]string{"Content-Range": "bytes */16"}},
		{"non-bytes unit ignored", "", false, false, "items=0-10", 200, body, map[string]string{"Content-Range": ""}},
		{"malformed range ignored", "", false, false, "bytes=abc-def", 200, body, map[string]string{"Content-Range": ""}},
		{"gzip-stored serves full body", "gzip", false, false, "bytes=0-3", 200, "", map[string]string{
			"Content-Range": "", "Accept-Ranges": "",
		}},
		{"other encodings range over stored bytes", "br", false, false, "bytes=0-3", 206, "0123", map[string]string{
			"Content-Encoding": "br",
		}},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				s := newTestServer(t, []fakestorage.Object{
					object(testObject, body, withType(testCType), withEncoding(tc.encoding)),
				})
				s.contentLength = tc.contentLength
				path := filePath
				if tc.bucketMode {
					s.sourceBucket = testBucket
					path = "/" + testObject
				}

				rec := do(t, s, method, path, hdr("Range", tc.rng))
				var wantBody *string
				switch {
				case method == http.MethodHead && tc.wantCode != http.StatusRequestedRangeNotSatisfiable:
					wantBody = ptr("")
				case tc.wantBody != "":
					wantBody = ptr(tc.wantBody)
				}
				assertResponse(t, rec, tc.wantCode, wantBody, tc.want)
			})
		}
	}
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

func TestHasETagValues(t *testing.T) {
	cases := []struct {
		values []string
		want   bool
	}{
		{nil, false},
		{[]string{""}, false},
		{[]string{"  "}, false},
		{[]string{"", `"x"`}, true},
		{[]string{`"x"`}, true},
	}
	for _, tc := range cases {
		if got := hasETagValues(tc.values); got != tc.want {
			t.Errorf("hasETagValues(%q) = %v, want %v", tc.values, got, tc.want)
		}
	}
}

func TestIfRangeAllowsPartial(t *testing.T) {
	attrs := &storage.ObjectAttrs{Etag: "v1abc", Updated: testUpdated}
	weakAttrs := &storage.ObjectAttrs{Etag: "v1abc", ContentEncoding: "gzip", Updated: testUpdated}
	noTimeAttrs := &storage.ObjectAttrs{Etag: "v1abc"}
	cases := []struct {
		name    string
		ifRange []string
		attrs   *storage.ObjectAttrs
		want    bool
	}{
		{"absent", nil, attrs, true},
		{"empty value", []string{""}, attrs, true},
		{"strong match", []string{`"v1abc"`}, attrs, true},
		{"strong mismatch", []string{`"other"`}, attrs, false},
		{"weak tag never matches", []string{`W/"v1abc"`}, attrs, false},
		{"strong tag against weak object tag", []string{`"v1abc"`}, weakAttrs, false},
		{"identical weak tags still never match", []string{`W/"v1abc"`}, weakAttrs, false},
		{"date equal", []string{testUpdated.Format(http.TimeFormat)}, attrs, true},
		{"date earlier", []string{testUpdated.Add(-time.Hour).Format(http.TimeFormat)}, attrs, false},
		{"date with zero updated", []string{testUpdated.Format(http.TimeFormat)}, noTimeAttrs, false},
		{"garbage", []string{"garbage"}, attrs, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header["If-Range"] = tc.ifRange
			if got := ifRangeAllowsPartial(req, tc.attrs); got != tc.want {
				t.Errorf("ifRangeAllowsPartial(If-Range: %q) = %v, want %v", tc.ifRange, got, tc.want)
			}
		})
	}
}

// TestCheckPreconditions_ZeroUpdated covers objects without a modification
// time; the fake always assigns Updated, so this is only reachable here.
func TestCheckPreconditions_ZeroUpdated(t *testing.T) {
	attrs := &storage.ObjectAttrs{Etag: testEtag}
	cases := []struct {
		key, value string
		wantCode   int
	}{
		{"If-Modified-Since", testUpdated.Format(http.TimeFormat), 0},
		{"If-Unmodified-Since", testUpdated.Add(-time.Hour).Format(http.TimeFormat), 0},
		{"If-None-Match", `"` + testEtag + `"`, http.StatusNotModified},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set(tc.key, tc.value)
			written := checkPreconditions(rec, req, attrs)
			if written != (tc.wantCode != 0) || (written && rec.Code != tc.wantCode) {
				t.Fatalf("checkPreconditions = %v (status %d), want status %d", written, rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("Last-Modified"); got != "" {
				t.Errorf("Last-Modified = %q, want empty for zero Updated", got)
			}
		})
	}
}

func TestCheckPreconditions_InvalidDateLoggedAtDebug(t *testing.T) {
	for _, key := range []string{"If-Unmodified-Since", "If-Modified-Since"} {
		t.Run(key, func(t *testing.T) {
			logs := captureLogs(t, slog.LevelDebug)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set(key, "not-a-date")
			if checkPreconditions(rec, req, &storage.ObjectAttrs{Etag: testEtag, Updated: testUpdated}) {
				t.Errorf("checkPreconditions wrote %d, want pass-through", rec.Code)
			}
			e := findLog(t, logs, "ignoring invalid "+key)
			if e == nil || e["level"] != "DEBUG" {
				t.Errorf("want a DEBUG %q log, got %s", "ignoring invalid "+key, logs)
			}
		})
	}
}

// TestProxy_Preconditions drives every precondition through the handler
// against testFile (and its gzip-stored twin), in RFC 9110 §13.2.2 order.
func TestProxy_Preconditions(t *testing.T) {
	strongTag := `"` + testEtag + `"`
	weakTag := `W/"` + testEtag + `"`
	lastMod := testUpdated.Format(http.TimeFormat)
	earlier := testUpdated.Add(-time.Hour).Format(http.TimeFormat)
	partial := testContent[2:6]

	notModified := map[string]string{
		"ETag": strongTag, "Last-Modified": lastMod, "Cache-Control": testCacheCtl,
		"Content-Type": "", "Accept-Ranges": "", "Content-Range": "", "Vary": "",
	}
	noRange := map[string]string{"Content-Range": ""}

	type tcase struct {
		name     string
		gzip     bool
		method   string
		h        http.Header
		wantCode int
		wantBody *string
		want     map[string]string
	}
	cases := []tcase{
		{"If-None-Match match", false, "", hdr("If-None-Match", strongTag), 304, ptr(""), notModified},
		{"If-None-Match weak form", false, "", hdr("If-None-Match", weakTag), 304, ptr(""), nil},
		{"If-None-Match list", false, "", hdr("If-None-Match", `"nope", W/"other", `+strongTag), 304, ptr(""), nil},
		{"If-None-Match field lines", false, "", hdr("If-None-Match", `"a"`, "If-None-Match", strongTag), 304, ptr(""), nil},
		{"If-None-Match star", false, "", hdr("If-None-Match", "*"), 304, ptr(""), nil},
		{"If-None-Match mismatch", false, "", hdr("If-None-Match", `"stale"`), 200, ptr(testContent), nil},
		{"If-None-Match unquoted never matches", false, "", hdr("If-None-Match", testEtag), 200, ptr(testContent), nil},
		{"If-None-Match on HEAD", false, http.MethodHead, hdr("If-None-Match", strongTag), 304, ptr(""), nil},
		{"If-None-Match mismatch ignores If-Modified-Since", false, "", hdr("If-None-Match", `"stale"`, "If-Modified-Since", lastMod), 200, ptr(testContent), nil},
		{"If-None-Match match beats stale If-Modified-Since", false, "", hdr("If-None-Match", strongTag, "If-Modified-Since", earlier), 304, ptr(""), nil},
		{"empty If-None-Match keeps If-Modified-Since", false, "", hdr("If-None-Match", "", "If-Modified-Since", lastMod), 304, ptr(""), nil},

		{"If-Modified-Since not modified", false, "", hdr("If-Modified-Since", lastMod), 304, ptr(""), notModified},
		{"If-Modified-Since modified", false, "", hdr("If-Modified-Since", earlier), 200, ptr(testContent), nil},
		{"If-Modified-Since invalid", false, "", hdr("If-Modified-Since", "not-a-date"), 200, ptr(testContent), nil},

		{"If-Match match", false, "", hdr("If-Match", strongTag), 200, ptr(testContent), nil},
		{"If-Match star", false, "", hdr("If-Match", "*"), 200, ptr(testContent), nil},
		{"If-Match mismatch", false, "", hdr("If-Match", `"stale"`), 412, nil, nil},
		{"If-Match weak form", false, "", hdr("If-Match", weakTag), 412, nil, nil},
		{"empty If-Match", false, "", hdr("If-Match", ""), 200, ptr(testContent), nil},
		{"If-Match overrides If-Unmodified-Since", false, "", hdr("If-Match", strongTag, "If-Unmodified-Since", earlier), 200, ptr(testContent), nil},

		{"If-Unmodified-Since satisfied", false, "", hdr("If-Unmodified-Since", lastMod), 200, ptr(testContent), nil},
		{"If-Unmodified-Since stale", false, "", hdr("If-Unmodified-Since", earlier), 412, nil, nil},
		{"If-Unmodified-Since invalid", false, "", hdr("If-Unmodified-Since", "not-a-date"), 200, ptr(testContent), nil},

		{"Range with matching If-None-Match", false, "", hdr("Range", "bytes=2-5", "If-None-Match", strongTag), 304, ptr(""), noRange},
		{"Range with failing If-Match", false, "", hdr("Range", "bytes=2-5", "If-Match", `"stale"`), 412, nil, noRange},
		{"If-Range etag match", false, "", hdr("Range", "bytes=2-5", "If-Range", strongTag), 206, ptr(partial), nil},
		{"If-Range etag mismatch", false, "", hdr("Range", "bytes=2-5", "If-Range", `"old"`), 200, ptr(testContent), noRange},
		{"If-Range weak etag", false, "", hdr("Range", "bytes=2-5", "If-Range", weakTag), 200, ptr(testContent), noRange},
		{"If-Range date match", false, "", hdr("Range", "bytes=2-5", "If-Range", lastMod), 206, ptr(partial), nil},
		{"If-Range date mismatch", false, "", hdr("Range", "bytes=2-5", "If-Range", earlier), 200, ptr(testContent), noRange},
		{"If-Range match with unsatisfiable range", false, "", hdr("Range", "bytes=9999-", "If-Range", strongTag), 416, nil, map[string]string{
			"Content-Range": "bytes */14",
		}},

		{"gzip If-None-Match weak tag", true, "", hdr("If-None-Match", weakTag, "Accept-Encoding", "gzip"), 304, ptr(""), map[string]string{
			"ETag": weakTag, "Vary": "Accept-Encoding",
		}},
		{"gzip If-None-Match weak tag, transcoded", true, "", hdr("If-None-Match", weakTag), 304, ptr(""), map[string]string{"Vary": "Accept-Encoding"}},
		{"gzip If-None-Match strong form matches weakly", true, "", hdr("If-None-Match", strongTag), 304, ptr(""), nil},
		{"gzip If-Match never matches the weak tag", true, "", hdr("If-Match", weakTag), 412, nil, nil},
	}
	for _, f := range []struct{ name, layout string }{
		{"IMF-fixdate", http.TimeFormat},
		{"RFC 850", time.RFC850},
		{"asctime", time.ANSIC},
	} {
		cases = append(cases,
			tcase{"If-Modified-Since " + f.name, false, "", hdr("If-Modified-Since", testUpdated.Format(f.layout)), 304, ptr(""), nil},
			tcase{"If-Unmodified-Since " + f.name, false, "", hdr("If-Unmodified-Since", testUpdated.Add(-time.Hour).Format(f.layout)), 412, nil, nil},
			tcase{"If-Range " + f.name, false, "", hdr("Range", "bytes=2-5", "If-Range", testUpdated.Format(f.layout)), 206, ptr(partial), nil},
		)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := testFile()
			if tc.gzip {
				obj = object(testObject, gzipped(t, testContent), withType(testCType), withEncoding("gzip"), withValidators(testEtag, testUpdated))
			}
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			rec := do(t, newTestServer(t, []fakestorage.Object{obj}), method, filePath, tc.h)
			assertResponse(t, rec, tc.wantCode, tc.wantBody, tc.want)
		})
	}
}

// Last-Modified has second precision, so sub-second object precision must
// not turn an equal-second If-Unmodified-Since into a spurious 412.
func TestProxy_IfUnmodifiedSince_SubSecondUpdated(t *testing.T) {
	updated := testUpdated.Add(300 * time.Millisecond)
	s := newTestServer(t, []fakestorage.Object{object(testObject, testContent, withValidators(testEtag, updated))})
	rec := do(t, s, http.MethodGet, filePath, hdr("If-Unmodified-Since", updated.Truncate(time.Second).Format(http.TimeFormat)))
	assertResponse(t, rec, http.StatusOK, ptr(testContent), nil)
}

// TestProxy_ResolvedObjectPreconditions checks that index, walk-up and SPA
// responses revalidate against the resolved object, and that the -not-found
// page never does.
func TestProxy_ResolvedObjectPreconditions(t *testing.T) {
	const notFoundBody = "<html>oops</html>"
	idx := withValidators("idx1", testUpdated)
	cases := []struct {
		name     string
		objects  []fakestorage.Object
		setup    func(*Server)
		path     string
		h        http.Header
		wantCode int
		wantETag string
	}{
		{"index", []fakestorage.Object{object("foo/index.html", testIndexBody, idx)},
			func(s *Server) { s.defaultIndex = "index.html" }, "/foo/", nil, 200, `"idx1"`},
		{"index revalidated", []fakestorage.Object{object("foo/index.html", testIndexBody, idx)},
			func(s *Server) { s.defaultIndex = "index.html" }, "/foo/", hdr("If-None-Match", `"idx1"`), 304, `"idx1"`},
		{"walk-up index revalidated", []fakestorage.Object{object("foo/index.html", testIndexBody, idx)},
			func(s *Server) { s.defaultIndex, s.walkUpIndex = "index.html", true }, "/foo/bar/search", hdr("If-None-Match", `"idx1"`), 304, `"idx1"`},
		{"spa route", []fakestorage.Object{object("index.html", testIndexBody, idx)},
			func(s *Server) { s.defaultIndex, s.spa = "index.html", true }, "/some/spa/route", nil, 200, `"idx1"`},
		{"spa route revalidated", []fakestorage.Object{object("index.html", testIndexBody, idx)},
			func(s *Server) { s.defaultIndex, s.spa = "index.html", true }, "/some/spa/route", hdr("If-None-Match", `"idx1"`), 304, `"idx1"`},
		{"spa route failing If-Match", []fakestorage.Object{object("index.html", testIndexBody, idx)},
			func(s *Server) { s.defaultIndex, s.spa = "index.html", true }, "/some/spa/route", hdr("If-Match", `"other"`), 412, ""},
		{"not-found page ignores preconditions", []fakestorage.Object{object("404.html", notFoundBody, withValidators("nf1", testUpdated))},
			func(s *Server) { s.notFoundPath = "404.html" }, "/missing.txt", hdr("If-None-Match", `"nf1"`, "If-Modified-Since", testUpdated.Format(http.TimeFormat)), 404, `"nf1"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, tc.objects)
			tc.setup(s)
			rec := do(t, s, http.MethodGet, "/"+testBucket+tc.path, tc.h)
			assertResponse(t, rec, tc.wantCode, nil, map[string]string{"ETag": tc.wantETag})
		})
	}
}

// net/http's server adds Date, which recorder-based tests bypass.
func TestProxy_DateHeaderOnRealServer(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t, []fakestorage.Object{testFile()}).handler())
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name     string
		inm      string
		wantCode int
	}{
		{"200", "", http.StatusOK},
		{"304", `"` + testEtag + `"`, http.StatusNotModified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+filePath, nil)
			if tc.inm != "" {
				req.Header.Set("If-None-Match", tc.inm)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.wantCode)
			}
			if res.Header.Get("Date") == "" {
				t.Error("Date header is missing")
			}
			if tc.wantCode == http.StatusNotModified && len(body) != 0 {
				t.Errorf("body = %q, want empty", body)
			}
		})
	}
}

// failNth fails the nth request (1-based) of op with status; with n == 0 it
// fails every request of op.
func failNth(op gcsOp, n, status int) func(gcsOp, int) int {
	return func(gotOp gcsOp, gotN int) int {
		if gotOp == op && (n == 0 || gotN == n) {
			return status
		}
		return 0
	}
}

// TestProxy_ErrorResponses covers what clients see when a lookup or read
// fails, with error redaction on (the default) and off.
func TestProxy_ErrorResponses(t *testing.T) {
	cases := []struct {
		name     string
		objects  []fakestorage.Object
		setup    func(*Server)
		fault    func(gcsOp, int) int
		path     string
		rng      string
		redact   bool
		wantCode int
		wantBody string
	}{
		{"missing object, redacted", nil, nil, nil, "/missing.txt", "", true, 404, "Not Found\n"},
		{"missing object, unredacted", nil, nil, nil, "/missing.txt", "", false, 404, storage.ErrObjectNotExist.Error()},
		{"attrs forbidden", []fakestorage.Object{testFile()}, nil, failNth(opAttrs, 1, http.StatusForbidden), "/" + testObject, "", true, 500, "Internal Server Error\n"},
		{"index candidate lookup forbidden", []fakestorage.Object{object("foo/index.html", testIndexBody)},
			func(s *Server) { s.defaultIndex = "index.html" }, failNth(opAttrs, 2, http.StatusForbidden), "/foo", "", true, 500, "Internal Server Error\n"},
		{"read forbidden", []fakestorage.Object{testFile()}, nil, failNth(opRead, 0, http.StatusForbidden), "/" + testObject, "", true, 500, "Internal Server Error\n"},
		{"range read forbidden", []fakestorage.Object{testFile()}, nil, failNth(opRead, 0, http.StatusForbidden), "/" + testObject, "bytes=0-3", true, 500, "Internal Server Error\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newFaultyServer(t, tc.objects, tc.fault)
			s.redactErrors = tc.redact
			s.contentLength = true
			if tc.setup != nil {
				tc.setup(s)
			}
			var h http.Header
			if tc.rng != "" {
				h = hdr("Range", tc.rng)
			}
			rec := do(t, s, http.MethodGet, "/"+testBucket+tc.path, h)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.redact && rec.Body.String() != tc.wantBody || !tc.redact && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
			// A failed read must not leak the object's headers onto the error.
			for _, k := range []string{"Accept-Ranges", "Cache-Control", "Content-Range", "ETag", "Last-Modified"} {
				if got := rec.Header().Get(k); got != "" {
					t.Errorf("%s = %q, want empty", k, got)
				}
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain", got)
			}
		})
	}
}

// TestProxy_GenerationChangedRetry simulates an overwrite between the attrs
// lookup and the generation-pinned read: the request is re-evaluated once
// against fresh attrs, and answers 503 if the generation changes again.
func TestProxy_GenerationChangedRetry(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(*Server)
		objects   []fakestorage.Object
		path      string
		rng       string
		failReads int
		wantCode  int
		wantBody  string
	}{
		{"full read recovers", nil, []fakestorage.Object{testFile()}, filePath, "", 1, 200, testContent},
		{"range read recovers", nil, []fakestorage.Object{testFile()}, filePath, "bytes=2-5", 1, 206, testContent[2:6]},
		{"spa fallback recovers", func(s *Server) { s.defaultIndex, s.spa = "index.html", true },
			[]fakestorage.Object{object("index.html", testIndexBody)}, "/" + testBucket + "/route", "", 1, 200, testIndexBody},
		{"changes twice", nil, []fakestorage.Object{testFile()}, filePath, "", 2, 503, "Service Unavailable\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := newFaultyServer(t, tc.objects, func(op gcsOp, n int) int {
				if op == opRead && n <= tc.failReads {
					return http.StatusNotFound
				}
				return 0
			})
			s.redactErrors = true
			if tc.setup != nil {
				tc.setup(s)
			}
			logs := captureLogs(t, slog.LevelDebug)

			var h http.Header
			if tc.rng != "" {
				h = hdr("Range", tc.rng)
			}
			assertResponse(t, do(t, s, http.MethodGet, tc.path, h), tc.wantCode, ptr(tc.wantBody), nil)
			if got := calls.get(opRead); got != 2 {
				t.Errorf("reads = %d, want 2", got)
			}
			if findLog(t, logs, "retrying request") == nil {
				t.Errorf("no %q log in %s", "retrying request", logs)
			}
		})
	}
}

func TestHandleError_LogLevels(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name      string
		err       error
		ctx       context.Context
		wantCode  int
		wantLevel string
	}{
		{"not found", storage.ErrObjectNotExist, context.Background(), 404, "DEBUG"},
		{"server error", errors.New("boom"), context.Background(), 500, "ERROR"},
		{"client canceled", context.Canceled, canceled, 500, "DEBUG"},
		{"generation changed twice", errGenerationChanged, context.Background(), 503, "WARN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t, slog.LevelDebug)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, filePath, nil).WithContext(tc.ctx)
			(&Server{}).handleError(rec, req, tc.err)

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			e := findLog(t, logs, "proxy error")
			if e == nil {
				t.Fatalf("no proxy error log in %s", logs)
			}
			if e["level"] != tc.wantLevel || e["status"] != float64(tc.wantCode) || e["url"] != filePath {
				t.Errorf("log = %v, want level %s, status %d, url %s", e, tc.wantLevel, tc.wantCode, filePath)
			}
		})
	}
}

// Client cancellations and missing objects are routine and must stay out of
// the default (info) log.
func TestProxy_RoutineErrorsQuietAtInfo(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		path string
		ctx  context.Context
	}{
		{"missing object", "/" + testBucket + "/missing.txt", context.Background()},
		{"client canceled", filePath, canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, []fakestorage.Object{testFile()})
			logs := captureLogs(t, slog.LevelInfo)
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(tc.ctx))
			if rec.Code < 400 {
				t.Fatalf("status = %d, want an error", rec.Code)
			}
			if logs.Len() != 0 {
				t.Errorf("unexpected log output at info: %s", logs)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Header() http.Header       { return http.Header{} }
func (failingWriter) WriteHeader(int)           {}
func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestCopyBody_LogsFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancel    bool
		wantLevel string
	}{
		{"server-side failure", false, "WARN"},
		{"client went away", true, "DEBUG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t, slog.LevelDebug)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			copyBody(failingWriter{}, httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx), strings.NewReader("body"))

			e := findLog(t, logs, "response body copy failed")
			if e == nil || e["level"] != tc.wantLevel {
				t.Errorf("want a %s %q log, got %s", tc.wantLevel, "response body copy failed", logs)
			}
		})
	}
}

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
		{"DEBUG", slog.LevelDebug, false},
		{"Info", slog.LevelInfo, false},
		{"", 0, true},
		{"trace", 0, true},
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

func TestAccessLog(t *testing.T) {
	cases := []struct {
		name       string
		verbose    bool
		level      slog.Level
		path       string
		h          http.Header
		wantLogged bool
		wantStatus int
		wantRemote string
	}{
		{"emitted with -v", true, slog.LevelInfo, filePath, nil, true, 200, "192.0.2.1:1234"},
		{"records the handler's status", true, slog.LevelInfo, "/" + testBucket + "/missing", nil, true, 404, "192.0.2.1:1234"},
		{"prefers X-Forwarded-For", true, slog.LevelInfo, filePath, hdr("X-Forwarded-For", "203.0.113.7"), true, 200, "203.0.113.7"},
		{"off without -v", false, slog.LevelInfo, filePath, nil, false, 0, ""},
		{"filtered below the log level", true, slog.LevelWarn, filePath, nil, false, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, []fakestorage.Object{testFile()})
			s.verbose = tc.verbose
			logs := captureLogs(t, tc.level)

			do(t, s, http.MethodGet, tc.path, tc.h)

			e := findLog(t, logs, "access")
			if !tc.wantLogged {
				if e != nil {
					t.Errorf("unexpected access log: %v", e)
				}
				return
			}
			if e == nil {
				t.Fatalf("no access log in %s", logs)
			}
			if e["method"] != "GET" || e["url"] != tc.path || e["status"] != float64(tc.wantStatus) || e["remote"] != tc.wantRemote {
				t.Errorf("access log = %v, want GET %s %d from %s", e, tc.path, tc.wantStatus, tc.wantRemote)
			}
			if _, ok := e["elapsed"].(float64); !ok {
				t.Errorf("elapsed = %v, want a number", e["elapsed"])
			}
		})
	}
}

func h2cClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

func TestServe_H2C(t *testing.T) {
	s := newTestServer(t, []fakestorage.Object{{
		ObjectAttrs: fakestorage.ObjectAttrs{
			BucketName:  testBucket,
			Name:        testObject,
			ContentType: testCType,
		},
		Content: []byte(testContent),
	}})
	s.h2c = true
	addr, cancel, done := startShutdownServer(t, s, s.handler())
	url := "http://" + addr + "/" + testBucket + "/" + testObject

	for _, tc := range []struct {
		name      string
		client    *http.Client
		wantMajor int
	}{
		{"h2c", h2cClient(t), 2},
		{"http1", &http.Client{Transport: &http.Transport{}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.client.Get(url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("io.ReadAll: %v", err)
			}
			if res.ProtoMajor != tc.wantMajor {
				t.Errorf("proto = %s, want HTTP/%d", res.Proto, tc.wantMajor)
			}
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d (body=%q)", res.StatusCode, http.StatusOK, body)
			}
			if got := string(body); got != testContent {
				t.Errorf("body = %q, want %q", got, testContent)
			}
		})
	}

	cancel()
	waitServeDone(t, done)
}

func TestServe_H2CDisabledByDefault(t *testing.T) {
	s := newTestServer(t, nil)
	addr, cancel, done := startShutdownServer(t, s, s.handler())

	if res, err := h2cClient(t).Get("http://" + addr + "/_health"); err == nil {
		res.Body.Close()
		t.Fatalf("h2c request succeeded with proto %s, want error", res.Proto)
	}
	getHealth(t, addr)

	cancel()
	waitServeDone(t, done)
}

func TestShutdown_H2CIdleConnClosedByGOAWAY(t *testing.T) {
	s := newTestServer(t, nil)
	s.h2c = true
	// Far longer than the test runs: the h2c connection must be closed by the
	// GOAWAY that a drain-window response triggers, never by elapsed time.
	s.idleGracePeriod = 10 * time.Minute
	addr, cancel, done := startShutdownServer(t, s, s.handler())
	client := h2cClient(t)

	get := func() {
		t.Helper()
		res, err := client.Get("http://" + addr + "/_health")
		if err != nil {
			t.Fatalf("GET /_health: %v", err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.ProtoMajor != 2 || res.StatusCode != http.StatusOK {
			t.Fatalf("got %s %d, want HTTP/2.0 200", res.Proto, res.StatusCode)
		}
	}
	get()

	cancel()
	waitListenerClosed(t, addr)

	get()
	waitServeDone(t, done)
}

func startShutdownServer(t *testing.T, s *Server, h http.Handler) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, ln, h) }()
	t.Cleanup(cancel)
	return ln.Addr().String(), cancel, done
}

func waitServeDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func waitListenerClosed(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener never closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func getHealth(t *testing.T, addr string) *http.Response {
	t.Helper()
	res, err := http.Get("http://" + addr + "/_health")
	if err != nil {
		t.Fatalf("GET /_health: %v", err)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatalf("read /_health body: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	return res
}

func TestShutdown_DrainWindowSendsConnectionClose(t *testing.T) {
	s := newTestServer(t, nil)
	// Far longer than the test runs, so the drain window is still open when
	// the assertions execute no matter how slow the runner is. The serve
	// goroutine is deliberately abandoned mid-drain; the test binary's exit
	// reaps it, and shutdown completion is covered by the other tests.
	s.shutdownDelay = 10 * time.Minute
	addr, cancel, _ := startShutdownServer(t, s, s.handler())

	if res := getHealth(t, addr); res.Close {
		t.Fatal("response before shutdown carries Connection: close")
	}

	cancel()
	deadline := time.Now().Add(30 * time.Second)
	for !getHealth(t, addr).Close {
		if time.Now().After(deadline) {
			t.Fatal("drain-window responses never gained Connection: close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestShutdown_IdleConnServedAfterListenerClose(t *testing.T) {
	s := newTestServer(t, nil)
	// Far longer than the test runs: the idle connection must only be closed
	// by the early exit after it closes, never by elapsed time. The test
	// still finishes immediately because serving the idle connection closes
	// it, which ends the idle-grace wait.
	s.idleGracePeriod = 10 * time.Minute
	addr, cancel, done := startShutdownServer(t, s, s.handler())

	conn, br := dialIdleConn(t, addr)

	cancel()
	waitListenerClosed(t, addr) // also asserts brand-new conns are refused

	if _, err := io.WriteString(conn, "GET /_health HTTP/1.1\r\nHost: gcsproxy.test\r\n\r\n"); err != nil {
		t.Fatalf("write request on idle conn: %v", err)
	}
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response on idle conn: %v", err)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatalf("read response body: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if !res.Close {
		t.Fatal("response after listener close lacks Connection: close")
	}

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("read after final response = %v, want io.EOF", err)
	}
	waitServeDone(t, done)
}

func TestShutdown_IdleGraceSkippedWhenNoConnections(t *testing.T) {
	s := newTestServer(t, nil)
	s.idleGracePeriod = 10 * time.Minute // must be skipped: no connections exist
	_, cancel, done := startShutdownServer(t, s, s.handler())

	cancel()
	waitServeDone(t, done) // fails if the wait is not skipped
}

func dialIdleConn(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	br := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, "GET /_health HTTP/1.1\r\nHost: gcsproxy.test\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatalf("read response body: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if res.Close {
		t.Fatal("response before shutdown carries Connection: close")
	}
	return conn, br
}

func TestShutdown_IdleGraceEndsWhenIdleConnsClose(t *testing.T) {
	s := newTestServer(t, nil)
	s.idleGracePeriod = 10 * time.Minute // must end early: client closes its conn
	addr, cancel, done := startShutdownServer(t, s, s.handler())

	conn, _ := dialIdleConn(t, addr)

	cancel()
	waitListenerClosed(t, addr)

	select {
	case err := <-done:
		t.Fatalf("serve returned %v while an idle connection was open", err)
	case <-time.After(100 * time.Millisecond):
	}

	conn.Close()
	waitServeDone(t, done) // fails if the wait doesn't end early
}

func TestShutdown_IdleGraceWaitsForAllIdleConns(t *testing.T) {
	s := newTestServer(t, nil)
	s.idleGracePeriod = 10 * time.Minute
	addr, cancel, done := startShutdownServer(t, s, s.handler())

	conn1, _ := dialIdleConn(t, addr)
	conn2, _ := dialIdleConn(t, addr)

	cancel()
	waitListenerClosed(t, addr)

	conn1.Close()
	select {
	case err := <-done:
		t.Fatalf("serve returned %v while an idle connection was open", err)
	case <-time.After(100 * time.Millisecond):
	}

	conn2.Close()
	waitServeDone(t, done)
}

func TestShutdown_IdleConnClosedAfterIdleGraceElapses(t *testing.T) {
	s := newTestServer(t, nil)
	s.idleGracePeriod = 100 * time.Millisecond
	addr, cancel, done := startShutdownServer(t, s, s.handler())

	conn, br := dialIdleConn(t, addr)

	cancel()

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("read on abandoned idle conn = %v, want io.EOF", err)
	}
	waitServeDone(t, done)
}

func TestShutdown_CompletesAndRefusesNewConnections(t *testing.T) {
	run := func(t *testing.T, delay, idleGrace time.Duration) {
		s := newTestServer(t, nil)
		s.shutdownDelay = delay
		s.idleGracePeriod = idleGrace
		addr, cancel, done := startShutdownServer(t, s, s.handler())

		getHealth(t, addr)
		cancel()
		waitServeDone(t, done)

		if conn, err := net.Dial("tcp", addr); err == nil {
			conn.Close()
			t.Fatal("dial succeeded after shutdown completed")
		}
	}
	t.Run("short delays", func(t *testing.T) { run(t, 20*time.Millisecond, 20*time.Millisecond) })
	t.Run("zero delays", func(t *testing.T) { run(t, 0, 0) })
}

func TestShutdown_WaitsForInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "done")
	})
	s := &Server{shutdownDelay: 20 * time.Millisecond, idleGracePeriod: 20 * time.Millisecond}
	addr, cancel, done := startShutdownServer(t, s, h)

	type result struct {
		status int
		body   string
		err    error
	}
	resc := make(chan result, 1)
	go func() {
		res, err := http.Get("http://" + addr + "/")
		if err != nil {
			resc <- result{err: err}
			return
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		resc <- result{status: res.StatusCode, body: string(body), err: err}
	}()

	<-started
	cancel()
	waitListenerClosed(t, addr)

	select {
	case err := <-done:
		t.Fatalf("serve returned %v before the in-flight request completed", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	r := <-resc
	if r.err != nil {
		t.Fatalf("in-flight request failed: %v", r.err)
	}
	if r.status != http.StatusOK || r.body != "done" {
		t.Fatalf("in-flight request = %d %q, want %d %q", r.status, r.body, http.StatusOK, "done")
	}
	waitServeDone(t, done)
}

func TestShutdown_TimeoutCompletesInTime(t *testing.T) {
	s := newTestServer(t, nil)
	s.shutdownTimeout = 10 * time.Minute // generous: must still exit cleanly
	addr, cancel, done := startShutdownServer(t, s, s.handler())

	getHealth(t, addr)
	cancel()
	waitServeDone(t, done)
}

func TestShutdown_TimeoutForcesClose(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // unblock the handler goroutine
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	s := &Server{shutdownTimeout: 100 * time.Millisecond}
	addr, cancel, done := startShutdownServer(t, s, h)

	errc := make(chan error, 1)
	go func() {
		res, err := http.Get("http://" + addr + "/")
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		errc <- err
	}()

	<-started
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "graceful shutdown incomplete") {
			t.Fatalf("serve returned %v, want graceful shutdown incomplete error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("server did not shut down after the timeout")
	}
	if err := <-errc; err == nil {
		t.Fatal("stuck in-flight request succeeded, want connection error")
	}
}

func TestListenAndServe_CleanShutdown(t *testing.T) {
	s := &Server{addr: "127.0.0.1:0"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown begins immediately; all phases have zero durations
	if err := s.ListenAndServe(ctx); err != nil {
		t.Fatalf("ListenAndServe = %v, want nil", err)
	}
}

func TestListenAndServe_BindError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	s := &Server{addr: ln.Addr().String()}
	if err := s.ListenAndServe(context.Background()); err == nil {
		t.Fatal("ListenAndServe on an occupied port succeeded, want error")
	}
}

func TestServe_ListenerErrorBeforeShutdown(t *testing.T) {
	s := &Server{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.serve(context.Background(), ln, s.handler()) }()

	// Killing the listener without canceling ctx makes srv.Serve fail; serve
	// must return that error instead of waiting for shutdown.
	ln.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("serve returned %v, want net.ErrClosed", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not return after listener close")
	}
}

// TestMain lets the test binary impersonate the gcsproxy binary: with
// GCSPROXY_RUN_MAIN=1 it runs main() on the test binary's arguments, so
// subprocess tests can exercise main's flag validation and startup errors
// for real, including exit codes.
func TestMain(m *testing.M) {
	if os.Getenv("GCSPROXY_RUN_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// runMain re-executes the test binary as gcsproxy and returns its stderr and
// exit code. STORAGE_EMULATOR_HOST lets storage.NewClient succeed without
// credentials; nothing ever connects to it.
func runMain(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "GCSPROXY_RUN_MAIN=1", "STORAGE_EMULATOR_HOST=127.0.0.1:1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stderr.String(), 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("running %v: %v", args, err)
	}
	return stderr.String(), ee.ExitCode()
}

func TestMain_NegativeDurationValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"negative shutdown delay", []string{"-shutdown-delay", "-1s"}},
		{"negative idle grace period", []string{"-shutdown-idle-grace-period", "-1s"}},
		{"negative shutdown timeout", []string{"-shutdown-timeout", "-1s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stderr, code := runMain(t, tc.args...)
			if code != 1 {
				t.Errorf("exit code = %d, want 1 (stderr=%q)", code, stderr)
			}
			if !strings.Contains(stderr, "must not be negative") {
				t.Errorf("stderr %q does not contain %q", stderr, "must not be negative")
			}
		})
	}
}

func TestMain_BindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	stderr, code := runMain(t, "-b", ln.Addr().String())
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "server exited") {
		t.Errorf("stderr %q does not contain %q", stderr, "server exited")
	}
}

func TestMain_ServesAndShutsDownOnSIGTERM(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-b", "127.0.0.1:0", "-log-format", "text")
	// STORAGE_EMULATOR_HOST makes storage.NewClient skip credential lookup;
	// nothing ever connects to it because only /_health is requested.
	cmd.Env = append(os.Environ(), "GCSPROXY_RUN_MAIN=1", "STORAGE_EMULATOR_HOST=127.0.0.1:1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting gcsproxy: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// The bound port is only known from the "listening" log line.
	var addr string
	scanner := bufio.NewScanner(stderr)
	// addr must be checked before Scan: with the operands swapped, finding the
	// address would still block on one more line the child never writes.
	for addr == "" && scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "msg=listening") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "addr="); ok {
				addr = v
			}
		}
	}
	if addr == "" {
		t.Fatalf("no listening line on stderr (scanner err: %v)", scanner.Err())
	}
	go io.Copy(io.Discard, stderr) // keep draining shutdown logs

	getHealth(t, addr)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("gcsproxy exited with %v, want exit code 0", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("gcsproxy did not exit after SIGTERM")
	}
}

func TestMain_Version(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-version")
	cmd.Env = append(os.Environ(), "GCSPROXY_RUN_MAIN=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gcsproxy -version: %v", err)
	}
	if !strings.HasPrefix(string(out), "gcsproxy ") {
		t.Errorf("stdout = %q, want prefix %q", out, "gcsproxy ")
	}
}

func TestServe_SlowHeadersTimeOut(t *testing.T) {
	prev := readHeaderTimeout
	readHeaderTimeout = 100 * time.Millisecond
	t.Cleanup(func() { readHeaderTimeout = prev })

	s := newTestServer(t, nil)
	addr, cancel, done := startShutdownServer(t, s, s.handler())

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /_health HTTP/1.1\r\nHost: x\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("connection was not closed by the server: %v", err)
	}

	cancel()
	waitServeDone(t, done)
}
