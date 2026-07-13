package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	redactErrors  bool
	logErrors     bool
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
		redactErrors    = flag.Bool("redact-errors", false, "Suppress error response bodies.")
		logErrors       = flag.Bool("log-errors", false, "Log proxy error details at error level.")
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
		redactErrors:  *redactErrors,
		logErrors:     *logErrors,
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
		s.handleError(w, err)
		return
	}
	if lastStrs, ok := r.Header["If-Modified-Since"]; ok && len(lastStrs) > 0 {
		last, err := http.ParseTime(lastStrs[0])
		if s.verbose && err != nil {
			slog.Warn("could not parse If-Modified-Since", "err", err)
		}
		if !attrs.Updated.Truncate(time.Second).After(last) {
			w.WriteHeader(304)
			return
		}
	}

	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		s.streamRange(w, r, attrs, rangeHeader)
		return
	}

	if err := s.streamObject(w, r, attrs, http.StatusOK); err != nil {
		s.handleError(w, err)
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
	return s.streamObject(w, r, attrs, status)
}

func (s *Server) streamObject(w http.ResponseWriter, r *http.Request, attrs *storage.ObjectAttrs, status int) error {
	gzipAcceptable := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")

	setTimeHeader(w, "Last-Modified", attrs.Updated)
	setStrHeader(w, "Content-Type", attrs.ContentType)
	setStrHeader(w, "Content-Language", attrs.ContentLanguage)
	setStrHeader(w, "Cache-Control", attrs.CacheControl)
	setStrHeader(w, "Content-Disposition", attrs.ContentDisposition)
	if !strings.EqualFold(attrs.ContentEncoding, "gzip") {
		setStrHeader(w, "Accept-Ranges", "bytes")
	}

	if r.Method == http.MethodHead {
		// Mirror what a matching GET would emit; gzip-stored served without
		// Accept-Encoding: gzip is transcoded by GCS, so neither field is set.
		if strings.EqualFold(attrs.ContentEncoding, "gzip") {
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

	objr, err := s.client.Bucket(attrs.Bucket).Object(attrs.Name).ReadCompressed(gzipAcceptable).NewReader(r.Context())
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
	if strings.EqualFold(attrs.ContentEncoding, "gzip") {
		if err := s.streamObject(w, r, attrs, http.StatusOK); err != nil {
			s.handleError(w, err)
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
				s.handleError(w, err)
			}
			return
		case errors.Is(err, errRangeUnsatisfiable):
			setStrHeader(w, "Content-Range", fmt.Sprintf("bytes */%d", attrs.Size))
			http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}

	setTimeHeader(w, "Last-Modified", attrs.Updated)
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

	objr, err := s.client.Bucket(attrs.Bucket).Object(attrs.Name).NewRangeReader(r.Context(), start, length)
	if err != nil {
		s.handleError(w, err)
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

func healthCheck(w http.ResponseWriter, r *http.Request) {
	setStrHeader(w, "Content-Type", "text/plain")
	io.WriteString(w, "OK\n")
}

func (s *Server) handleError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, storage.ErrObjectNotExist) {
		status = http.StatusNotFound
	}
	if s.logErrors {
		slog.Error("proxy error", "status", status, "err", err)
	}
	if s.redactErrors {
		w.WriteHeader(status)
		return
	}
	http.Error(w, err.Error(), status)
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
