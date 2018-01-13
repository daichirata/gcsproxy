package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
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
	bind         = flag.String("bind", "127.0.0.1:8080", "bind address")
	credentials  = flag.String("credentials", "", "the path to the keyfile. If not present, client will use your default application credentials")
	signatureKey = flag.String("signature_key", "", "HMAC key used in calculating request signatures")
	verbose      = flag.Bool("verbose", false, "print verbose logging messages")
)

var (
	client *storage.Client
	ctx    = context.Background()
)

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
	r.HandleFunc("/{bucket:[0-9a-zA-Z-_]+}/{object:.*}", wrapper(proxy)).Methods("GET", "HEAD")

	log.Printf("[service] listening on %s", *bind)
	log.Fatal(http.ListenAndServe(*bind, r))
}

func validateSignature(key string, r *http.Request) error {
	q := r.URL.Query()

	expires := q.Get("expires")
	signature := q.Get("signature")

	i, err := strconv.Atoi(expires)
	if err != nil {
		return fmt.Errorf("expires format is invalid: %s", err)
	}
	if int64(i) < time.Now().Unix() {
		return errors.New("sigunature has expired")
	}

	got, err := base64.URLEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("error base64 decoding signature %q", signature)
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(expires + ":" + r.URL.Path))
	want := mac.Sum(nil)
	if !hmac.Equal(got, want) {
		return errors.New("signature is invalid")
	}

	return nil
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

		if *verbose {
			addr := r.RemoteAddr
			if ip, found := header(r, "X-Forwarded-For"); found {
				addr = ip
			}
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

func handleError(w http.ResponseWriter, code int, err error) {
	log.Println(err)
	http.Error(w, http.StatusText(code), code)
}

func proxy(w http.ResponseWriter, r *http.Request) {
	if *signatureKey != "" {
		if err := validateSignature(*signatureKey, r); err != nil {
			handleError(w, http.StatusForbidden, err)
			return
		}
	}

	params := mux.Vars(r)
	obj := client.Bucket(params["bucket"]).Object(params["object"])

	attr, err := obj.Attrs(ctx)
	if err != nil {
		var code int
		if err == storage.ErrObjectNotExist {
			code = http.StatusNotFound
		} else {
			code = http.StatusInternalServerError
		}
		handleError(w, code, err)
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
		handleError(w, http.StatusInternalServerError, err)
		return
	}
	io.Copy(w, objr)
}
