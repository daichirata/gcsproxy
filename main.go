package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/storage"
	"github.com/gorilla/mux"
	"google.golang.org/api/option"
)

type Server struct {
	addr          string
	client        *storage.Client
	defaultIndex  string
	walkUpIndex   bool
	sourceBucket  string
	spa           bool
	notFoundPath  string
	contentLength bool
	corsOrigin    string
	verbose       bool
}

func main() {
	var (
		bind            = flag.String("b", "127.0.0.1:8080", "Bind address.")
		verbose         = flag.Bool("v", false, "Show access log.")
		credentialsFile = flag.String("c", "", "Path to a service-account key file. Defaults to Application Default Credentials.")
		defaultIndex    = flag.String("i", "", "Default index file to serve.")
		walkUpIndex     = flag.Bool("walk-up-index", false, "When -i lookup misses, retry parent directories for index files before not-found handling.")
		sourceBucket    = flag.String("bucket", "", "Fixed bucket name. Disables bucket extraction from the path.")
		spa             = flag.Bool("spa", false, "SPA fallback: serve -i from the bucket root with HTTP 200 for unmatched routes.")
		notFoundPath    = flag.String("not-found", "", "Object served with HTTP 404 for unmatched routes.")
		logFormat       = flag.String("log-format", "json", "Log output format: text or json.")
		logLevel        = flag.String("log-level", "info", "Minimum log level: debug, info, warn, or error.")
		contentLength   = flag.Bool("content-length", false, "Send the Content-Length header (disables chunked transfer).")
		corsOrigin      = flag.String("cors-origin", "", "Value for the Access-Control-Allow-Origin header.")
	)
	flag.Parse()

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	handlerOpts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch *logFormat {
	case "text":
		handler = slog.NewTextHandler(os.Stderr, handlerOpts)
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, handlerOpts)
	default:
		fmt.Fprintf(os.Stderr, "invalid -log-format: %q (want text or json)\n", *logFormat)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(handler))

	if *spa && *defaultIndex == "" {
		fatal("-spa requires -i to be set")
	}
	if *spa && *notFoundPath != "" {
		fatal("-spa and -not-found are mutually exclusive")
	}

	ctx := context.Background()
	var opts []option.ClientOption
	if *credentialsFile != "" {
		creds, err := credentials.DetectDefault(&credentials.DetectOptions{
			CredentialsFile: *credentialsFile,
			Scopes:          []string{storage.ScopeFullControl},
		})
		if err != nil {
			fatal("failed to load credentials", "err", err)
		}
		opts = append(opts, option.WithAuthCredentials(creds))
	}
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		fatal("failed to create client", "err", err)
	}

	s := &Server{
		addr:          *bind,
		client:        client,
		defaultIndex:  *defaultIndex,
		walkUpIndex:   *walkUpIndex,
		sourceBucket:  *sourceBucket,
		spa:           *spa,
		notFoundPath:  *notFoundPath,
		contentLength: *contentLength,
		corsOrigin:    *corsOrigin,
		verbose:       *verbose,
	}

	if err := s.ListenAndServe(); err != nil {
		fatal("server exited", "err", err)
	}
}

func (s *Server) ListenAndServe() error {
	slog.Info("listening", "addr", s.addr)
	return http.ListenAndServe(s.addr, s.handler())
}

func (s *Server) handler() http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/_health", s.wrap(healthCheck)).Methods("GET", "HEAD")

	if s.sourceBucket != "" {
		r.HandleFunc("/{object:.*}", s.wrap(func(w http.ResponseWriter, r *http.Request) {
			s.proxy(w, r, s.sourceBucket, mux.Vars(r)["object"])
		})).Methods("GET", "HEAD")
	} else {
		r.HandleFunc("/{bucket:[0-9a-zA-Z-_.]+}/{object:.*}", s.wrap(func(w http.ResponseWriter, r *http.Request) {
			params := mux.Vars(r)
			s.proxy(w, r, params["bucket"], params["object"])
		})).Methods("GET", "HEAD")
	}
	return r
}

func (s *Server) wrap(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		proc := time.Now()
		if s.corsOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
		}
		writer := &wrapResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
		}
		fn(writer, r)
		addr := r.RemoteAddr
		if ip, found := header(r, "X-Forwarded-For"); found {
			addr = ip
		}
		if s.verbose {
			slog.Info("access",
				"remote", addr,
				"elapsed", time.Since(proc).Seconds(),
				"status", writer.status,
				"method", r.Method,
				"url", r.URL.String(),
			)
		}
	}
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, bucket, object string) {
	attrs, err := s.fetchObjectAttrs(r.Context(), bucket, object)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			if s.spa && s.defaultIndex != "" {
				if serveErr := s.serveObject(w, r, bucket, s.defaultIndex, http.StatusOK); serveErr == nil {
					return
				}
			} else if s.notFoundPath != "" {
				if serveErr := s.serveObject(w, r, bucket, s.notFoundPath, http.StatusNotFound); serveErr == nil {
					return
				}
			}
		}
		handleError(w, err)
		return
	}
	if s.checkPreconditions(w, r, attrs) {
		return
	}

	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" && ifRangeAllowsPartial(r, attrs) {
		s.streamRange(w, r, attrs, rangeHeader)
		return
	}

	if err := s.streamObject(w, r, attrs, http.StatusOK); err != nil {
		handleError(w, err)
	}
}

func (s *Server) fetchObjectAttrs(ctx context.Context, bucket, object string) (*storage.ObjectAttrs, error) {
	var err error
	var indexAppended bool
	if object == "" && s.defaultIndex != "" {
		object, err = url.JoinPath(object, s.defaultIndex)
		if err != nil {
			return nil, err
		}
		indexAppended = true
	}

	attrs, err := s.client.Bucket(bucket).Object(strings.TrimSuffix(object, "/")).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			if s.defaultIndex == "" || indexAppended {
				return nil, err
			}
			candidates, candidateErr := indexCandidates(object, s.defaultIndex, s.walkUpIndex)
			if candidateErr != nil {
				return nil, candidateErr
			}
			for _, candidate := range candidates {
				attrs, candidateErr := s.client.Bucket(bucket).Object(candidate).Attrs(ctx)
				if candidateErr == nil {
					return attrs, nil
				}
				if !errors.Is(candidateErr, storage.ErrObjectNotExist) {
					return nil, candidateErr
				}
				err = candidateErr
			}
			return nil, err
		}
		return nil, err
	}
	return attrs, nil
}

func indexCandidates(object, indexName string, walkUp bool) ([]string, error) {
	trimmed := strings.TrimSuffix(object, "/")
	first, err := url.JoinPath(trimmed, indexName)
	if err != nil {
		return nil, err
	}
	candidates := []string{first}
	if !walkUp {
		return candidates, nil
	}

	ancestor := trimmed
	for {
		slash := strings.LastIndex(ancestor, "/")
		if slash < 0 {
			break
		}
		ancestor = ancestor[:slash]
		candidate := indexName
		if ancestor != "" {
			candidate, err = url.JoinPath(ancestor, indexName)
			if err != nil {
				return nil, err
			}
		}
		if candidate != candidates[len(candidates)-1] {
			candidates = append(candidates, candidate)
		}
	}

	return candidates, nil
}

func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, bucket, object string, status int) error {
	attrs, err := s.client.Bucket(bucket).Object(object).Attrs(r.Context())
	if err != nil {
		return err
	}
	// Preconditions apply only to would-be-2xx responses (RFC 9110 §13.2.1):
	// the SPA fallback revalidates, the -not-found 404 page never does.
	if status == http.StatusOK && s.checkPreconditions(w, r, attrs) {
		return nil
	}
	return s.streamObject(w, r, attrs, status)
}

func (s *Server) streamObject(w http.ResponseWriter, r *http.Request, attrs *storage.ObjectAttrs, status int) error {
	gzipAcceptable := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")

	setTimeHeader(w, "Last-Modified", attrs.Updated)
	setStrHeader(w, "ETag", objectETag(attrs))
	setStrHeader(w, "Content-Type", attrs.ContentType)
	setStrHeader(w, "Content-Language", attrs.ContentLanguage)
	setStrHeader(w, "Cache-Control", attrs.CacheControl)
	setStrHeader(w, "Content-Disposition", attrs.ContentDisposition)
	if gzipStored(attrs) {
		// The gzip and GCS-transcoded variants share this URL, so caches
		// must key on the request's Accept-Encoding.
		setStrHeader(w, "Vary", "Accept-Encoding")
	} else {
		setStrHeader(w, "Accept-Ranges", "bytes")
	}

	if r.Method == http.MethodHead {
		// Mirror what a matching GET would emit; gzip-stored served without
		// Accept-Encoding: gzip is transcoded by GCS, so neither field is set.
		if gzipStored(attrs) {
			if gzipAcceptable {
				setStrHeader(w, "Content-Encoding", attrs.ContentEncoding)
				if s.contentLength {
					setIntHeader(w, "Content-Length", attrs.Size)
				}
			}
		} else if s.contentLength {
			setIntHeader(w, "Content-Length", attrs.Size)
		}
		w.WriteHeader(status)
		return nil
	}

	obj := s.client.Bucket(attrs.Bucket).Object(attrs.Name)
	if attrs.Generation != 0 {
		// Pin the read to the generation the validators above were derived
		// from, so a concurrent overwrite cannot pair a new body with them.
		obj = obj.Generation(attrs.Generation)
	}
	objr, err := obj.ReadCompressed(gzipAcceptable).NewReader(r.Context())
	if err != nil {
		return err
	}
	defer objr.Close()

	setStrHeader(w, "Content-Encoding", objr.Attrs.ContentEncoding)
	if s.contentLength {
		setIntHeader(w, "Content-Length", objr.Attrs.Size)
	}

	w.WriteHeader(status)
	io.Copy(w, objr)
	return nil
}

func (s *Server) streamRange(w http.ResponseWriter, r *http.Request, attrs *storage.ObjectAttrs, rangeHeader string) {
	// GCS transcodes gzip-stored objects on download (verified empirically),
	// so Range is silently ignored — fall back to a full 200. Other encodings
	// (br, deflate, ...) are not transcoded and Range works normally.
	if gzipStored(attrs) {
		if err := s.streamObject(w, r, attrs, http.StatusOK); err != nil {
			handleError(w, err)
		}
		return
	}

	start, length, err := parseSingleRange(rangeHeader, attrs.Size)
	if err != nil {
		switch {
		case errors.Is(err, errRangeIgnore):
			// Per RFC 9110 §14.2 non-"bytes" units must be ignored; we extend
			// that to unparseable byte-ranges so a malformed header doesn't
			// downgrade a previously-working download to 416.
			if err := s.streamObject(w, r, attrs, http.StatusOK); err != nil {
				handleError(w, err)
			}
			return
		case errors.Is(err, errRangeUnsatisfiable):
			setStrHeader(w, "Content-Range", fmt.Sprintf("bytes */%d", attrs.Size))
			http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}

	setTimeHeader(w, "Last-Modified", attrs.Updated)
	setStrHeader(w, "ETag", objectETag(attrs))
	setStrHeader(w, "Content-Type", attrs.ContentType)
	setStrHeader(w, "Content-Language", attrs.ContentLanguage)
	setStrHeader(w, "Cache-Control", attrs.CacheControl)
	setStrHeader(w, "Content-Disposition", attrs.ContentDisposition)
	setStrHeader(w, "Accept-Ranges", "bytes")
	setStrHeader(w, "Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, attrs.Size))
	if s.contentLength {
		setIntHeader(w, "Content-Length", length)
	}

	if r.Method == http.MethodHead {
		setStrHeader(w, "Content-Encoding", attrs.ContentEncoding)
		w.WriteHeader(http.StatusPartialContent)
		return
	}

	obj := s.client.Bucket(attrs.Bucket).Object(attrs.Name)
	if attrs.Generation != 0 {
		obj = obj.Generation(attrs.Generation)
	}
	objr, err := obj.NewRangeReader(r.Context(), start, length)
	if err != nil {
		handleError(w, err)
		return
	}
	defer objr.Close()
	setStrHeader(w, "Content-Encoding", objr.Attrs.ContentEncoding)

	w.WriteHeader(http.StatusPartialContent)
	io.Copy(w, objr)
}

var (
	errRangeIgnore        = errors.New("range header ignored")
	errRangeUnsatisfiable = errors.New("range not satisfiable")
)

func parseSingleRange(header string, size int64) (start, length int64, err error) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, errRangeIgnore
	}
	spec := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(header, prefix), ",", 2)[0])
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, errRangeIgnore
	}
	startStr, endStr := spec[:dash], spec[dash+1:]

	if startStr == "" {
		// Suffix range: -N → last N bytes.
		n, perr := strconv.ParseInt(endStr, 10, 64)
		if perr != nil || n <= 0 {
			return 0, 0, errRangeIgnore
		}
		if n > size {
			n = size
		}
		if n == 0 {
			return 0, 0, errRangeUnsatisfiable
		}
		return size - n, n, nil
	}

	s, perr := strconv.ParseInt(startStr, 10, 64)
	if perr != nil || s < 0 {
		return 0, 0, errRangeIgnore
	}
	if s >= size {
		return 0, 0, errRangeUnsatisfiable
	}
	if endStr == "" {
		return s, size - s, nil
	}
	e, perr := strconv.ParseInt(endStr, 10, 64)
	if perr != nil {
		return 0, 0, errRangeIgnore
	}
	if e < s {
		return 0, 0, errRangeUnsatisfiable
	}
	if e >= size {
		e = size - 1
	}
	return s, e - s + 1, nil
}

// --- conditional requests (RFC 9110 §13) ---

// gzipStored reports whether the object is stored with Content-Encoding: gzip
// and is therefore subject to GCS decompressive transcoding on download.
func gzipStored(attrs *storage.ObjectAttrs) bool {
	return strings.EqualFold(attrs.ContentEncoding, "gzip")
}

// objectETag returns the ETag header value for attrs, or "" when unknown.
// gzip-stored objects get a weak tag because the same URL serves two
// byte-different representations (raw gzip vs GCS-transcoded) depending on
// the request's Accept-Encoding; the variants are semantically equivalent,
// which is exactly what a weak tag promises.
func objectETag(attrs *storage.ObjectAttrs) string {
	if attrs.Etag == "" {
		return ""
	}
	if gzipStored(attrs) {
		return `W/"` + attrs.Etag + `"`
	}
	return `"` + attrs.Etag + `"`
}

// scanETag determines if a syntactically valid ETag is present at the start
// of s (RFC 9110 §8.8.3). If so, it returns the ETag (including quotes and
// any W/ prefix) and the remaining text after it; otherwise ("", "").
// Ported from net/http/fs.go.
func scanETag(s string) (etag string, remain string) {
	s = textproto.TrimString(s)
	start := 0
	if strings.HasPrefix(s, "W/") {
		start = 2
	}
	if len(s[start:]) < 2 || s[start] != '"' {
		return "", ""
	}
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		switch {
		// Character values allowed in etagc.
		case c == 0x21 || c >= 0x23 && c <= 0x7E || c >= 0x80:
		case c == '"':
			return s[:i+1], s[i+1:]
		default:
			return "", ""
		}
	}
	return "", ""
}

// etagStrongMatch reports whether a and b match using strong ETag comparison:
// both must be non-weak and identical.
func etagStrongMatch(a, b string) bool {
	return a == b && a != "" && a[0] == '"'
}

// etagWeakMatch reports whether a and b match using weak ETag comparison,
// ignoring any W/ prefix on either side.
func etagWeakMatch(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

// hasETagValues reports whether any of the header values is non-empty, so a
// present-but-empty If-Match/If-None-Match field is treated as absent rather
// than as a failing precondition, matching net/http semantics.
func hasETagValues(values []string) bool {
	for _, v := range values {
		if textproto.TrimString(v) != "" {
			return true
		}
	}
	return false
}

// anyETagMatches reports whether target matches any entity-tag in the given
// header values (each possibly a comma-separated list). "*" matches any
// existing representation. A malformed member ends scanning of that value,
// treating the rest as no-match, like net/http/fs.go.
func anyETagMatches(values []string, target string, strong bool) bool {
	match := etagWeakMatch
	if strong {
		match = etagStrongMatch
	}
	for _, buf := range values {
		for {
			buf = textproto.TrimString(buf)
			if len(buf) == 0 {
				break
			}
			if buf[0] == ',' {
				buf = buf[1:]
				continue
			}
			if buf[0] == '*' {
				return true
			}
			etag, remain := scanETag(buf)
			if etag == "" {
				break
			}
			if match(etag, target) {
				return true
			}
			buf = remain
		}
	}
	return false
}

// writeNotModified sends 304 with the validator headers the equivalent 200
// would carry (RFC 9110 §15.4.5). Representation headers (Content-Type,
// Content-Length, ...) are deliberately omitted.
func writeNotModified(w http.ResponseWriter, attrs *storage.ObjectAttrs) {
	setStrHeader(w, "ETag", objectETag(attrs))
	setTimeHeader(w, "Last-Modified", attrs.Updated)
	setStrHeader(w, "Cache-Control", attrs.CacheControl)
	if gzipStored(attrs) {
		setStrHeader(w, "Vary", "Accept-Encoding")
	}
	w.WriteHeader(http.StatusNotModified)
}

// checkPreconditions evaluates If-Match, If-Unmodified-Since, If-None-Match
// and If-Modified-Since against the resolved object in the order mandated by
// RFC 9110 §13.2.2, writing a 412 or 304 when a precondition fails. It
// reports whether the response has been written. Invalid HTTP-dates and
// unknown (zero) modification times disable the date-based checks; malformed
// entity-tags are treated as no-match.
func (s *Server) checkPreconditions(w http.ResponseWriter, r *http.Request, attrs *storage.ObjectAttrs) bool {
	if ifMatch := r.Header.Values("If-Match"); hasETagValues(ifMatch) {
		if !anyETagMatches(ifMatch, objectETag(attrs), true) {
			http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
			return true
		}
	} else if ius, ok := header(r, "If-Unmodified-Since"); ok && !attrs.Updated.IsZero() {
		t, err := http.ParseTime(ius)
		if err != nil {
			if s.verbose {
				slog.Warn("could not parse If-Unmodified-Since", "err", err)
			}
		} else if attrs.Updated.Truncate(time.Second).After(t) {
			http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
			return true
		}
	}

	if inm := r.Header.Values("If-None-Match"); hasETagValues(inm) {
		if anyETagMatches(inm, objectETag(attrs), false) {
			writeNotModified(w, attrs)
			return true
		}
		// If-None-Match present but unmatched: If-Modified-Since MUST be
		// ignored (RFC 9110 §13.1.3).
		return false
	}

	if ims, ok := header(r, "If-Modified-Since"); ok && !attrs.Updated.IsZero() {
		t, err := http.ParseTime(ims)
		if err != nil {
			if s.verbose {
				slog.Warn("could not parse If-Modified-Since", "err", err)
			}
			return false
		}
		if !attrs.Updated.Truncate(time.Second).After(t) {
			writeNotModified(w, attrs)
			return true
		}
	}
	return false
}

// ifRangeAllowsPartial reports whether a Range header may be honored given
// the request's If-Range field (RFC 9110 §13.1.5): absent → yes; an
// entity-tag must strong-match the current ETag (weak tags never match); an
// HTTP-date must equal Last-Modified exactly, at the header's second
// precision. On mismatch the caller serves the full body instead.
func ifRangeAllowsPartial(r *http.Request, attrs *storage.ObjectAttrs) bool {
	ir, ok := header(r, "If-Range")
	if !ok || ir == "" {
		return true
	}
	if etag, _ := scanETag(ir); etag != "" {
		return etagStrongMatch(etag, objectETag(attrs))
	}
	if attrs.Updated.IsZero() {
		return false
	}
	t, err := http.ParseTime(ir)
	if err != nil {
		return false
	}
	return attrs.Updated.Truncate(time.Second).Equal(t)
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	setStrHeader(w, "Content-Type", "text/plain")
	io.WriteString(w, "OK\n")
}

func handleError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrObjectNotExist) {
		http.Error(w, err.Error(), http.StatusNotFound)
	} else {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func header(r *http.Request, key string) (string, bool) {
	if r.Header == nil {
		return "", false
	}
	if candidate := r.Header[key]; len(candidate) > 0 {
		return candidate[0], true
	}
	return "", false
}

func setStrHeader(w http.ResponseWriter, key string, value string) {
	if value != "" {
		w.Header().Add(key, value)
	}
}

func setIntHeader(w http.ResponseWriter, key string, value int64) {
	if value > 0 {
		w.Header().Add(key, strconv.FormatInt(value, 10))
	}
}

func setTimeHeader(w http.ResponseWriter, key string, value time.Time) {
	if !value.IsZero() {
		w.Header().Add(key, value.UTC().Format(http.TimeFormat))
	}
}

type wrapResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *wrapResponseWriter) WriteHeader(status int) {
	w.ResponseWriter.WriteHeader(status)
	w.status = status
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid -log-level: %q (want debug, info, warn, or error)", s)
}
