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

var (
	bind            = flag.String("b", "127.0.0.1:8080", "Bind address")
	verbose         = flag.Bool("v", false, "Show access log")
	credentialsFile = flag.String("c", "", "The path to the keyfile. If not present, client will use your default application credentials.")
	defaultIndex    = flag.String("i", "", "The default index file to serve.")
	sourceBucket    = flag.String("bucket", "", "Fixed bucket name. If unset, the bucket is taken from the first path segment.")
	spa             = flag.Bool("spa", false, "Single-page application fallback. When a request does not match an object, serve the -i index file from the bucket root with HTTP 200. Requires -i; mutually exclusive with -not-found.")
	notFoundPath    = flag.String("not-found", "", "Object path served with HTTP 404 when no object matches the request. Mutually exclusive with -spa.")
	logFormat       = flag.String("log-format", "text", "Log output format: text or json.")
)

var client *storage.Client

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

func wrapper(fn func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		proc := time.Now()
		writer := &wrapResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
		}
		fn(writer, r)
		addr := r.RemoteAddr
		if ip, found := header(r, "X-Forwarded-For"); found {
			addr = ip
		}
		if *verbose {
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

func fetchObjectAttrs(ctx context.Context, bucket, object string) (*storage.ObjectAttrs, error) {
	var err error
	var indexAppended bool
	if object == "" && *defaultIndex != "" {
		object, err = url.JoinPath(object, *defaultIndex)
		if err != nil {
			return nil, err
		}
		indexAppended = true
	}

	attrs, err := client.Bucket(bucket).Object(strings.TrimSuffix(object, "/")).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			if *defaultIndex == "" || indexAppended {
				return nil, err
			}
			object, err = url.JoinPath(object, *defaultIndex)
			if err != nil {
				return nil, err
			}
			return client.Bucket(bucket).Object(object).Attrs(ctx)
		}
		return nil, err
	}
	return attrs, nil
}

func proxy(w http.ResponseWriter, r *http.Request, bucket, object string) {
	attrs, err := fetchObjectAttrs(r.Context(), bucket, object)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			if *spa && *defaultIndex != "" {
				if serveErr := serveObject(w, r, bucket, *defaultIndex, http.StatusOK); serveErr == nil {
					return
				}
			} else if *notFoundPath != "" {
				if serveErr := serveObject(w, r, bucket, *notFoundPath, http.StatusNotFound); serveErr == nil {
					return
				}
			}
		}
		handleError(w, err)
		return
	}
	if lastStrs, ok := r.Header["If-Modified-Since"]; ok && len(lastStrs) > 0 {
		last, err := http.ParseTime(lastStrs[0])
		if *verbose && err != nil {
			slog.Warn("could not parse If-Modified-Since", "err", err)
		}
		if !attrs.Updated.Truncate(time.Second).After(last) {
			w.WriteHeader(304)
			return
		}
	}

	gzipAcceptable := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	objr, err := client.Bucket(attrs.Bucket).Object(attrs.Name).ReadCompressed(gzipAcceptable).NewReader(r.Context())
	if err != nil {
		handleError(w, err)
		return
	}
	setTimeHeader(w, "Last-Modified", attrs.Updated)
	setStrHeader(w, "Content-Type", attrs.ContentType)
	setStrHeader(w, "Content-Language", attrs.ContentLanguage)
	setStrHeader(w, "Cache-Control", attrs.CacheControl)
	setStrHeader(w, "Content-Encoding", objr.Attrs.ContentEncoding)
	setStrHeader(w, "Content-Disposition", attrs.ContentDisposition)
	setIntHeader(w, "Content-Length", objr.Attrs.Size)
	io.Copy(w, objr)
}

func serveObject(w http.ResponseWriter, r *http.Request, bucket, object string, status int) error {
	attrs, err := client.Bucket(bucket).Object(object).Attrs(r.Context())
	if err != nil {
		return err
	}
	gzipAcceptable := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	objr, err := client.Bucket(attrs.Bucket).Object(attrs.Name).ReadCompressed(gzipAcceptable).NewReader(r.Context())
	if err != nil {
		return err
	}
	setTimeHeader(w, "Last-Modified", attrs.Updated)
	setStrHeader(w, "Content-Type", attrs.ContentType)
	setStrHeader(w, "Content-Language", attrs.ContentLanguage)
	setStrHeader(w, "Cache-Control", attrs.CacheControl)
	setStrHeader(w, "Content-Encoding", objr.Attrs.ContentEncoding)
	setStrHeader(w, "Content-Disposition", attrs.ContentDisposition)
	setIntHeader(w, "Content-Length", objr.Attrs.Size)
	w.WriteHeader(status)
	io.Copy(w, objr)
	return nil
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	setStrHeader(w, "Content-Type", "text/plain")
	io.WriteString(w, "OK\n")
}

func newRouter(sourceBucket string) http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/_health", wrapper(healthCheck)).Methods("GET", "HEAD")

	if sourceBucket != "" {
		r.HandleFunc("/{object:.*}", wrapper(func(w http.ResponseWriter, r *http.Request) {
			proxy(w, r, sourceBucket, mux.Vars(r)["object"])
		})).Methods("GET", "HEAD")
	} else {
		r.HandleFunc("/{bucket:[0-9a-zA-Z-_.]+}/{object:.*}", wrapper(func(w http.ResponseWriter, r *http.Request) {
			params := mux.Vars(r)
			proxy(w, r, params["bucket"], params["object"])
		})).Methods("GET", "HEAD")
	}
	return r
}

func main() {
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
	var err error
	client, err = storage.NewClient(ctx, opts...)
	if err != nil {
		fatal("failed to create client", "err", err)
	}

	slog.Info("listening", "bind", *bind)
	if err := http.ListenAndServe(*bind, newRouter(*sourceBucket)); err != nil {
		fatal("server exited", "err", err)
	}
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
