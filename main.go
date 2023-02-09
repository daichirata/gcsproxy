package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gorilla/mux"
	"google.golang.org/api/option"
)

var (
	bind        = flag.String("b", "127.0.0.1:8080", "Bind address")
	verbose     = flag.Bool("v", true, "Show access log")
	credentials = flag.String("c", "", "The path to the keyfile. If not present, client will use your default application credentials.")
)

var (
	client *storage.Client
	ctx    = context.Background()
)

func handleError(w http.ResponseWriter, err error) {
	if err != nil {
		if err == storage.ErrObjectNotExist {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
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
			log.Printf("[%s] %.3f %d %s %s",
				addr,
				time.Now().Sub(proc).Seconds(),
				writer.status,
				r.Method,
				r.URL,
			)
		}
	}
}

func proxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	params := mux.Vars(r)
	gzipAcceptable := clientAcceptsGzip(r)
	obj := client.Bucket(params["bucket"]).Object(params["object"]).ReadCompressed(gzipAcceptable)
	attr, err := obj.Attrs(ctx)
	if err != nil {
		handleError(w, err)
		return
	}
	if lastStrs, ok := r.Header["If-Modified-Since"]; ok && len(lastStrs) > 0 {
		last, err := http.ParseTime(lastStrs[0])
		if *verbose && err != nil {
			log.Printf("could not parse If-Modified-Since: %v", err)
		}
		if !attr.Updated.Truncate(time.Second).After(last) {
			w.WriteHeader(304)
			return
		}
	}
	objr, err := obj.NewReader(ctx)
	if err != nil {
		handleError(w, err)
		return
	}

	defer objr.Close()

	setTimeHeader(w, "Last-Modified", attr.Updated)
	setStrHeader(w, "Content-Type", attr.ContentType)
	setStrHeader(w, "Content-Language", attr.ContentLanguage)
	setStrHeader(w, "Cache-Control", attr.CacheControl)
	setStrHeader(w, "Content-Encoding", objr.Attrs.ContentEncoding)
	setStrHeader(w, "Content-Disposition", attr.ContentDisposition)
	setIntHeader(w, "Content-Length", objr.Attrs.Size)

	_, err = io.Copy(w, objr)
	if err != nil {
		log.Printf("error copying: %v", err)
	}
}

func clientAcceptsGzip(r *http.Request) bool {
	acceptHeader := r.Header.Get("Accept-Encoding")
	return strings.Contains(acceptHeader, "gzip")
}

func main() {
	flag.Parse()

	var err error
	if *credentials != "" {
		client, err = storage.NewClient(ctx, option.WithCredentialsFile(*credentials))
	} else {
		client, err = storage.NewClient(ctx)
	}
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	r := mux.NewRouter()
	r.HandleFunc("/{bucket:[0-9a-zA-Z-_.]+}/{object:.*}", wrapper(proxy)).Methods("GET", "HEAD")

	log.Printf("[service] listening on %s", *bind)
	if err := http.ListenAndServe(*bind, r); err != nil {
		log.Fatal(err)
	}
}
