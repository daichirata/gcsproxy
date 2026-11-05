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
	"net/textproto"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
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

// version is set by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

// Keeps slow-header (Slowloris) clients from pinning connections; response
// streaming is not limited.
const readHeaderTimeout = 10 * time.Second

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
	redactErrors    bool
	h2c             bool
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
		redactErrors    = flag.Bool("redact-errors", true, "Replace error response bodies with the generic HTTP status text.")
		h2c             = flag.Bool("h2c", false, "Enable cleartext HTTP/2 (h2c) with prior knowledge alongside HTTP/1.1.")
		showVersion     = flag.Bool("version", false, "Print the version and exit.")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("gcsproxy", buildVersion())
		return
	}

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
		redactErrors:    *redactErrors,
		h2c:             *h2c,
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
	slog.Info("listening", "addr", ln.Addr().String(), "version", buildVersion())
	return s.serve(ctx, ln, s.handler())
}

// serve runs h on ln until ctx is canceled, then executes the phased graceful
// shutdown: drain (Connection: close) -> close listener -> idle grace ->
// Shutdown. Idle connections stay open until the idle grace period elapses so
// clients never race a new request against a server-side close.
func (s *Server) serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	tracker := &connTracker{}
	srv := &http.Server{
		Handler:           s.drainingHandler(h),
		ConnState:         tracker.connState,
		Protocols:         s.protocols(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

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

func (s *Server) protocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(s.h2c)
	return p
}

// connTracker lets the shutdown sequence stop waiting as soon as no
// connections remain. Active connections count too: a response whose headers
// predate the drain flag
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

// net/http closes the connection after a Connection: close response. Over
// HTTP/2 the header is stripped and the server sends GOAWAY instead.
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
		if s.verbose {
			addr := r.RemoteAddr
			if ip, found := header(r, "X-Forwarded-For"); found {
				addr = ip
			}
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
	err := s.proxyOnce(w, r, bucket, object)
	if errors.Is(err, errGenerationChanged) {
		// Nothing has been written yet: start over from fresh attrs so the
		// preconditions and range are evaluated against the new generation.
		slog.DebugContext(r.Context(), "retrying request", "url", r.URL.String(), "err", err)
		err = s.proxyOnce(w, r, bucket, object)
	}
	if err != nil {
		s.handleError(w, r, err)
	}
}

func (s *Server) proxyOnce(w http.ResponseWriter, r *http.Request, bucket, object string) error {
	attrs, err := s.fetchObjectAttrs(r.Context(), bucket, object)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			var serveErr error
			if s.spa && s.defaultIndex != "" {
				serveErr = s.serveObject(w, r, bucket, s.defaultIndex, http.StatusOK)
			} else if s.notFoundPath != "" {
				serveErr = s.serveObject(w, r, bucket, s.notFoundPath, http.StatusNotFound)
			} else {
				return err
			}
			if serveErr == nil || errors.Is(serveErr, errGenerationChanged) {
				return serveErr
			}
		}
		return err
	}
	if checkPreconditions(w, r, attrs) {
		return nil
	}

	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" && ifRangeAllowsPartial(r, attrs) {
		return s.streamRange(w, r, attrs, rangeHeader)
	}
	return s.streamObject(w, r, attrs, http.StatusOK)
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
	if status == http.StatusOK && checkPreconditions(w, r, attrs) {
		return nil
	}
	return s.streamObject(w, r, attrs, status)
}

func (s *Server) streamObject(w http.ResponseWriter, r *http.Request, attrs *storage.ObjectAttrs, status int) error {
	gzipAcceptable := acceptsGzip(r.Header.Values("Accept-Encoding"))

	// Open the reader before setting any header so that a failure leaves the
	// response clean for handleError.
	var objr *storage.Reader
	if r.Method != http.MethodHead {
		var err error
		objr, err = s.objectHandle(attrs).ReadCompressed(gzipAcceptable).NewReader(r.Context())
		if err != nil {
			return pinnedReadErr(attrs, err)
		}
		defer objr.Close()
	}

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

	setStrHeader(w, "Content-Encoding", objr.Attrs.ContentEncoding)
	if s.contentLength {
		setIntHeader(w, "Content-Length", objr.Attrs.Size)
	}

	w.WriteHeader(status)
	copyBody(w, r, objr)
	return nil
}

func (s *Server) streamRange(w http.ResponseWriter, r *http.Request, attrs *storage.ObjectAttrs, rangeHeader string) error {
	// GCS transcodes gzip-stored objects on download (verified empirically),
	// so Range is silently ignored — fall back to a full 200. Other encodings
	// (br, deflate, ...) are not transcoded and Range works normally.
	if gzipStored(attrs) {
		return s.streamObject(w, r, attrs, http.StatusOK)
	}

	start, length, err := parseSingleRange(rangeHeader, attrs.Size)
	if err != nil {
		switch {
		case errors.Is(err, errRangeIgnore):
			// Per RFC 9110 §14.2 non-"bytes" units must be ignored; we extend
			// that to unparseable byte-ranges so a malformed header doesn't
			// downgrade a previously-working download to 416.
			return s.streamObject(w, r, attrs, http.StatusOK)
		case errors.Is(err, errRangeUnsatisfiable):
			setStrHeader(w, "Content-Range", fmt.Sprintf("bytes */%d", attrs.Size))
			http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return nil
		}
	}

	var objr *storage.Reader
	if r.Method != http.MethodHead {
		objr, err = s.objectHandle(attrs).NewRangeReader(r.Context(), start, length)
		if err != nil {
			return pinnedReadErr(attrs, err)
		}
		defer objr.Close()
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
		return nil
	}

	setStrHeader(w, "Content-Encoding", objr.Attrs.ContentEncoding)

	w.WriteHeader(http.StatusPartialContent)
	copyBody(w, r, objr)
	return nil
}

// The status line is already out, so a failure can only be logged; client
// disconnects are routine.
func copyBody(w http.ResponseWriter, r *http.Request, objr io.Reader) {
	if _, err := io.Copy(w, objr); err != nil {
		level := slog.LevelWarn
		if r.Context().Err() != nil {
			level = slog.LevelDebug
		}
		slog.Log(r.Context(), level, "response body copy failed", "url", r.URL.String(), "err", err)
	}
}

// RFC 9110 §12.5.3: an explicit gzip/x-gzip entry wins over "*", and q=0
// means "not acceptable".
func acceptsGzip(values []string) bool {
	gzipQ, anyQ := -1.0, -1.0
	for _, v := range values {
		for _, member := range strings.Split(v, ",") {
			coding, params, _ := strings.Cut(member, ";")
			q := 1.0
			for _, param := range strings.Split(params, ";") {
				name, value, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok && strings.EqualFold(strings.TrimSpace(name), "q") {
					if f, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
						q = f
					}
				}
			}
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "gzip", "x-gzip":
				gzipQ = max(gzipQ, q)
			case "*":
				anyQ = max(anyQ, q)
			}
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return anyQ > 0
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

var errGenerationChanged = errors.New("object generation changed during request")

// objectHandle pins reads to the generation the validators were derived from,
// so a concurrent overwrite cannot pair a new body with them.
func (s *Server) objectHandle(attrs *storage.ObjectAttrs) *storage.ObjectHandle {
	obj := s.client.Bucket(attrs.Bucket).Object(attrs.Name)
	if attrs.Generation != 0 {
		obj = obj.Generation(attrs.Generation)
	}
	return obj
}

// pinnedReadErr turns a not-found from a generation-pinned read into
// errGenerationChanged, so the request is retried instead of answering 404.
func pinnedReadErr(attrs *storage.ObjectAttrs, err error) error {
	if attrs.Generation != 0 && errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("%w (generation %d)", errGenerationChanged, attrs.Generation)
	}
	return err
}

func gzipStored(attrs *storage.ObjectAttrs) bool {
	return strings.EqualFold(attrs.ContentEncoding, "gzip")
}

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

// Ported from net/http/fs.go (RFC 9110 §8.8.3).
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

func etagStrongMatch(a, b string) bool {
	return a == b && a != "" && a[0] == '"'
}

func etagWeakMatch(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

// A present-but-empty If-Match/If-None-Match field is treated as absent rather
// than as a failing precondition, matching net/http semantics.
func hasETagValues(values []string) bool {
	for _, v := range values {
		if textproto.TrimString(v) != "" {
			return true
		}
	}
	return false
}

// "*" matches any existing representation. A malformed member ends scanning
// of that value, treating the rest as no-match, like net/http/fs.go.
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

// 304 carries the validator headers the equivalent 200 would (RFC 9110
// §15.4.5); representation headers (Content-Type, Content-Length, ...) are
// deliberately omitted.
func writeNotModified(w http.ResponseWriter, attrs *storage.ObjectAttrs) {
	setStrHeader(w, "ETag", objectETag(attrs))
	setTimeHeader(w, "Last-Modified", attrs.Updated)
	setStrHeader(w, "Cache-Control", attrs.CacheControl)
	if gzipStored(attrs) {
		setStrHeader(w, "Vary", "Accept-Encoding")
	}
	w.WriteHeader(http.StatusNotModified)
}

// checkPreconditions follows the evaluation order of RFC 9110 §13.2.2 and
// reports whether it has written a 412 or 304. Invalid HTTP-dates and unknown
// (zero) modification times disable the date-based checks.
func checkPreconditions(w http.ResponseWriter, r *http.Request, attrs *storage.ObjectAttrs) bool {
	if ifMatch := r.Header.Values("If-Match"); hasETagValues(ifMatch) {
		if !anyETagMatches(ifMatch, objectETag(attrs), true) {
			http.Error(w, "Precondition Failed", http.StatusPreconditionFailed)
			return true
		}
	} else if ius, ok := header(r, "If-Unmodified-Since"); ok && !attrs.Updated.IsZero() {
		t, err := http.ParseTime(ius)
		if err != nil {
			slog.DebugContext(r.Context(), "ignoring invalid If-Unmodified-Since", "err", err)
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
			slog.DebugContext(r.Context(), "ignoring invalid If-Modified-Since", "err", err)
			return false
		}
		if !attrs.Updated.Truncate(time.Second).After(t) {
			writeNotModified(w, attrs)
			return true
		}
	}
	return false
}

// If-Range (RFC 9110 §13.1.5): an entity-tag must strong-match, so weak tags
// never do; an HTTP-date must equal Last-Modified exactly.
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

func (s *Server) handleError(w http.ResponseWriter, r *http.Request, err error) {
	status, level := http.StatusInternalServerError, slog.LevelError
	switch {
	case errors.Is(err, errGenerationChanged):
		status, level = http.StatusServiceUnavailable, slog.LevelWarn
	case errors.Is(err, storage.ErrObjectNotExist):
		status, level = http.StatusNotFound, slog.LevelDebug
	case errors.Is(err, context.Canceled):
		level = slog.LevelDebug
	}
	slog.Log(r.Context(), level, "proxy error", "status", status, "url", r.URL.String(), "err", err)
	msg := err.Error()
	if s.redactErrors {
		msg = http.StatusText(status)
	}
	http.Error(w, msg, status)
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

// buildVersion falls back to the module version recorded by `go install
// ...@vX.Y.Z` when the binary was not built by goreleaser.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
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
