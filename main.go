package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gorilla/mux"
	"google.golang.org/api/option"
)

var (
	bind         = flag.String("b", "127.0.0.1:8080", "Bind address")
	verbose      = flag.Bool("v", false, "Show access log")
	credentials  = flag.String("c", "", "The path to the keyfile. If not present, client will use your default application credentials.")
	defaultIndex = flag.String("i", "", "The default index file to serve.")
)

var client *storage.Client

func handleError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrObjectNotExist) {
		log.Printf("object not found: %v", err)
		http.Error(w, "not found", http.StatusNotFound)
	} else {
		log.Printf("internal error: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
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

var logSanitizer = strings.NewReplacer("\n", "", "\r", "")

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
			addr = logSanitizer.Replace(ip)
		}
		if *verbose {
			log.Printf("[%s] %.3f %d %s %s",
				addr,
				time.Now().Sub(proc).Seconds(),
				writer.status,
				r.Method,
				logSanitizer.Replace(r.URL.String()),
			)
		}
	}
}

func hasDotDotSegment(s string) bool {
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
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

func proxy(w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)

	object := params["object"]
	if hasDotDotSegment(object) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	attrs, err := fetchObjectAttrs(r.Context(), params["bucket"], object)
	if err != nil {
		handleError(w, err)
		return
	}
	if lastStrs, ok := r.Header["If-Modified-Since"]; ok && len(lastStrs) > 0 {
		last, err := http.ParseTime(lastStrs[0])
		if *verbose && err != nil {
			log.Printf("could not parse If-Modified-Since: %v", err)
		}
		if !attrs.Updated.Truncate(time.Second).After(last) {
			w.WriteHeader(304)
			return
		}
	}

	setTimeHeader(w, "Last-Modified", attrs.Updated)
	setStrHeader(w, "Content-Type", attrs.ContentType)
	setStrHeader(w, "Content-Language", attrs.ContentLanguage)
	setStrHeader(w, "Cache-Control", attrs.CacheControl)
	setStrHeader(w, "Content-Encoding", attrs.ContentEncoding)
	setStrHeader(w, "Content-Disposition", attrs.ContentDisposition)
	setIntHeader(w, "Content-Length", attrs.Size)

	if r.Method == http.MethodHead {
		return
	}

	gzipAcceptable := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	objr, err := client.Bucket(attrs.Bucket).Object(attrs.Name).ReadCompressed(gzipAcceptable).NewReader(r.Context())
	if err != nil {
		handleError(w, err)
		return
	}
	_, err = io.Copy(w, objr)
	if err != nil {
		return
	}
}

func healthCheck(w http.ResponseWriter, _ *http.Request) {
	setStrHeader(w, "Content-Type", "text/plain")
	_, err := io.WriteString(w, "OK\n")
	if err != nil {
		return
	}
}

func main() {
	flag.Parse()

	var err error
	if *credentials != "" {
		//goland:noinspection GoDeprecation
		client, err = storage.NewClient(context.Background(), option.WithCredentialsFile(*credentials))
	} else {
		client, err = storage.NewClient(context.Background())
	}
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	r := mux.NewRouter()
	r.HandleFunc("/_health", wrapper(healthCheck)).Methods("GET", "HEAD")
	r.HandleFunc("/{bucket:[0-9a-zA-Z-_.]+}/{object:.*}", wrapper(proxy)).Methods("GET", "HEAD")

	log.Printf("[service] listening on %s", *bind)
	if err := http.ListenAndServe(*bind, r); err != nil {
		log.Fatal(err)
	}
}
