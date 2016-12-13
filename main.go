package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gorilla/mux"
)

var (
	bind    = flag.String("b", "127.0.0.1:8080", "bind address")
	verbose = flag.Bool("v", false, "show access log")
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
	w.Header().Add(key, value.UTC().Format(http.TimeFormat))
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
	params := mux.Vars(r)
	obj := client.Bucket(params["bucket"]).Object(params["object"])
	attr, err := obj.Attrs(ctx)
	if err != nil {
		handleError(w, err)
		return
	}
	setStrHeader(w, "Content-Type", attr.ContentType)
	setStrHeader(w, "Content-Language", attr.ContentLanguage)
	setStrHeader(w, "Cache-Control", attr.CacheControl)
	setStrHeader(w, "Content-Encoding", attr.ContentEncoding)
	setStrHeader(w, "Content-Disposition", attr.ContentDisposition)
	// setIntHeader(w, "Content-Length", attr.Size)
	// setTimeHeader(w, "Last-Modified", attr.Updated)
	// setStrHeader(w, "Expires", attr.Expires)
	objr, err := obj.NewReader(ctx)
	if err != nil {
		handleError(w, err)
		return
	}
	io.Copy(w, objr)
}

func main() {
	flag.Parse()

	var err error
	client, err = storage.NewClient(ctx)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	r := mux.NewRouter()
	r.HandleFunc("/{bucket:[0-9a-zA-Z-_]+}/{object:.*}", wrapper(proxy)).Methods("GET", "HEAD")

	log.Printf("[service] listening on %s", *bind)
	if err := http.ListenAndServe(*bind, r); err != nil {
		log.Fatal(err)
	}
}
