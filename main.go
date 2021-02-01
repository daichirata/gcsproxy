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
	"google.golang.org/api/option"
)

var (
	bind          = flag.String("b", "127.0.0.1:8080", "Bind address.")
	credentials   = flag.String("c", "", "The path to the keyfile. If not present, client will use your default application credentials.")
	redirect404   = flag.Bool("r", false, "Redirect to index.html if 404 not found.")
	useDomainName = flag.Bool("dn", false, "Use hostname as a bucket name.")
	useSecret     = flag.String("s", "", "Use SA key from secretManager. E.G. 'projects/937192795301/secrets/gcs-proxy/versions/1'")
	verbose       = flag.Bool("v", false, "Show access log.")
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
	params := mux.Vars(r)

	// Redefine bucket name, in case our bucket name in the following format 'site.example.com'.
	if *useDomainName {
		params["bucket"] = r.Host
	}

	// Set index page name
	if params["object"] == "" {
		params["object"] = "index.html"
	}

	obj := client.Bucket(params["bucket"]).Object(params["object"])
	attr, err := obj.Attrs(ctx)

	if err == storage.ErrObjectNotExist && *redirect404 {
		obj = client.Bucket(params["bucket"]).Object("index.html")
		attr, err = obj.Attrs(ctx)
	}

	if err != nil {
		handleError(w, err)
		return
	}
	setStrHeader(w, "Content-Type", attr.ContentType)
	setStrHeader(w, "Content-Language", attr.ContentLanguage)
	setStrHeader(w, "Cache-Control", attr.CacheControl)
	setStrHeader(w, "Content-Encoding", attr.ContentEncoding)
	setStrHeader(w, "Content-Disposition", attr.ContentDisposition)
	setIntHeader(w, "Content-Length", attr.Size)
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
	var path string

	if *credentials != "" {
		client, err = storage.NewClient(ctx, option.WithCredentialsFile(*credentials))
	} else if *useSecret != "" {
		client, err = storage.NewClient(ctx, option.WithCredentialsFile(GetSecret(*useSecret)))
	} else {
		client, err = storage.NewClient(ctx)
	}
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	r := mux.NewRouter()

	if !*useDomainName {
		path = "/{bucket:[0-9a-zA-Z-_.]+}"
	}

	r.HandleFunc(path+"/{object:.*}", wrapper(proxy)).Methods("GET", "HEAD", "POST")

	log.Printf("[service] listening on %s", *bind)
	if err := http.ListenAndServe(*bind, r); err != nil {
		log.Fatal(err)
	}
}
