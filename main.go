package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/storage"
	"github.com/gorilla/mux"
	"google.golang.org/api/option"
)

type Server struct {
	addr            string
	client          *storage.Client
	defaultIndex    string
	walkUpIndex     bool
	sourceBucket    string
	spa             bool
	notFoundPath    string
	contentLength   bool
	corsOrigin      string
	verbose         bool
	shutdownDelay   time.Duration
	idleGracePeriod time.Duration
	shutdownTimeout time.Duration
	draining        atomic.Bool
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
		shutdownDelay   = flag.Duration("shutdown-delay", 0, "Delay after SIGTERM/SIGINT during which requests are served normally but responses carry Connection: close.")
		idleGracePeriod = flag.Duration("shutdown-idle-grace-period", 0, "After the listener closes, keep idle connections open for up to this long; ends early once no connections remain.")
		shutdownTimeout = flag.Duration("shutdown-timeout", 30*time.Second, "Max wait for in-flight requests after the idle grace period; 0 waits indefinitely.")
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
	if *shutdownDelay < 0 || *idleGracePeriod < 0 || *shutdownTimeout < 0 {
		fatal("-shutdown-delay, -shutdown-idle-grace-period and -shutdown-timeout must not be negative")
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
	defer client.Close()

	s := &Server{
		addr:            *bind,
		client:          client,
		defaultIndex:    *defaultIndex,
		walkUpIndex:     *walkUpIndex,
		sourceBucket:    *sourceBucket,
		spa:             *spa,
		notFoundPath:    *notFoundPath,
		contentLength:   *contentLength,
		corsOrigin:      *corsOrigin,
		verbose:         *verbose,
		shutdownDelay:   *shutdownDelay,
		idleGracePeriod: *idleGracePeriod,
		shutdownTimeout: *shutdownTimeout,
	}

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Unregister after the first signal so a second SIGTERM/SIGINT kills the
	// process immediately via the default disposition.
	context.AfterFunc(sigCtx, stop)

	if err := s.ListenAndServe(sigCtx); err != nil {
		fatal("server exited", "err", err)
	}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	slog.Info("listening", "addr", ln.Addr().String())
	return s.serve(ctx, ln, s.handler())
}

// serve runs h on ln until ctx is canceled, then executes the phased graceful
// shutdown: drain (Connection: close) -> close listener -> idle grace ->
// Shutdown. Idle connections stay open until the idle grace period elapses so
// clients never race a new request against a server-side close.
func (s *Server) serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	tracker := &connTracker{}
	srv := &http.Server{Handler: s.drainingHandler(h), ConnState: tracker.connState}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Phase 1: keep serving; every new response tells clients to stop reusing.
	s.draining.Store(true)
	slog.Info("shutdown: draining", "delay", s.shutdownDelay)
	time.Sleep(s.shutdownDelay)

	// Phase 2: stop accepting new connections. Existing conns keep working.
	slog.Info("shutdown: closing listener")
	ln.Close()
	// Wait for Serve to return so it untracks its listener; otherwise Shutdown
	// below can report a spurious double-close error.
	if err := <-errc; err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Warn("shutdown: serve loop exited with error", "err", err)
	}

	// Phase 3: leave idle connections open; requests on them are still served.
	// Skip the wait when no connections remain, and end it early once the
	// last one closes.
	slog.Info("shutdown: waiting before closing idle connections", "grace", s.idleGracePeriod, "open", tracker.open())
	select {
	case <-tracker.noneOpen():
		slog.Info("shutdown: no connections remain")
	case <-time.After(s.idleGracePeriod):
	}

	// Phase 4: close idle connections, wait for in-flight requests.
	slog.Info("shutdown: closing idle connections", "timeout", s.shutdownTimeout)
	sctx := context.Background()
	if s.shutdownTimeout > 0 {
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(sctx, s.shutdownTimeout)
		defer cancel()
	}
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
		return fmt.Errorf("graceful shutdown incomplete: %w", err)
	}
	slog.Info("shutdown: complete")
	return nil
}

// connTracker counts open server connections via http.Server.ConnState so
// the shutdown sequence can stop waiting as soon as none remain. Active
// connections count too: a response whose headers predate the drain flag
// carries no Connection: close, so its connection can still go idle and be
// reused by the client.
type connTracker struct {
	mu     sync.Mutex
	count  int
	waiter chan struct{}
}

func (ct *connTracker) connState(_ net.Conn, state http.ConnState) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	switch state {
	case http.StateNew:
		ct.count++
	case http.StateHijacked, http.StateClosed:
		ct.count--
		if ct.count == 0 && ct.waiter != nil {
			close(ct.waiter)
			ct.waiter = nil
		}
	}
}

func (ct *connTracker) open() int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.count
}

// noneOpen returns a channel that is closed once no connections remain open.
// Only valid after the listener stopped accepting new connections, since the
// count never rises again from zero.
func (ct *connTracker) noneOpen() <-chan struct{} {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ch := make(chan struct{})
	if ct.count == 0 {
		close(ch)
	} else {
		ct.waiter = ch
	}
	return ch
}

// drainingHandler marks every response written after shutdown begins with
// Connection: close, so keep-alive clients stop reusing the connection.
// net/http then closes the connection after the response completes.
func (s *Server) drainingHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			w.Header().Set("Connection", "close")
		}
		next.ServeHTTP(w, r)
	})
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
