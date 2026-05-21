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
		sourceBucket    = flag.String("bucket", "", "Fixed bucket name. Disables bucket extraction from the path.")
		spa             = flag.Bool("spa", false, "SPA fallback: serve -i from the bucket root with HTTP 200 for unmatched routes.")
		notFoundPath    = flag.String("not-found", "", "Object served with HTTP 404 for unmatched routes.")
		logFormat       = flag.String("log-format", "json", "Log output format: text or json.")
		contentLength   = flag.Bool("content-length", false, "Send the Content-Length header (disables chunked transfer).")
		corsOrigin      = flag.String("cors-origin", "", "Value for the Access-Control-Allow-Origin header.")
	)
	flag.Parse()

	var handler slog.Handler
	switch *logFormat {
	case "text":
		handler = slog.NewTextHandler(os.Stderr, nil)
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, nil)
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
			object, err = url.JoinPath(object, s.defaultIndex)
			if err != nil {
				return nil, err
			}
			return s.client.Bucket(bucket).Object(object).Attrs(ctx)
		}
		return nil, err
	}
	return attrs, nil
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
	objr, err := s.client.Bucket(attrs.Bucket).Object(attrs.Name).ReadCompressed(gzipAcceptable).NewReader(r.Context())
	if err != nil {
		return err
	}
	setTimeHeader(w, "Last-Modified", attrs.Updated)
	setStrHeader(w, "Content-Type", attrs.ContentType)
	setStrHeader(w, "Content-Language", attrs.ContentLanguage)
	setStrHeader(w, "Cache-Control", attrs.CacheControl)
	setStrHeader(w, "Content-Encoding", objr.Attrs.ContentEncoding)
	setStrHeader(w, "Content-Disposition", attrs.ContentDisposition)
	if s.contentLength {
		setIntHeader(w, "Content-Length", objr.Attrs.Size)
	}
	w.WriteHeader(status)
	io.Copy(w, objr)
	return nil
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
