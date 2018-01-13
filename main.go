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
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

func main() {
	var (
		bind         = flag.String("bind", "127.0.0.1:8080", "bind address")
		credentials  = flag.String("credentials", "", "the path to the keyfile. If not present, client will use your default application credentials")
		signatureKey = flag.String("signature_key", "", "HMAC key used in calculating request signatures")
		accessLog    = flag.Bool("access_log", true, "print access log")
		verbose      = flag.Bool("verbose", false, "print verbose logging messages")
	)
	flag.Parse()

	c := Config{
		credentials:  *credentials,
		signatureKey: []byte(*signatureKey),
		accessLog:    *accessLog,
		verbose:      *verbose,
	}
	proxy, err := NewProxy(c)
	if err != nil {
		log.Fatalf("Failed to create proxy: %v", err)
	}

	mux := http.NewServeMux()
	proxy.RegisterHandlers(mux)

	log.Printf("[service] listening on %s", *bind)
	log.Fatal(http.ListenAndServe(*bind, mux))
}

type Config struct {
	credentials  string
	signatureKey []byte
	accessLog    bool
	verbose      bool
}

type Proxy struct {
	client *storage.Client
	config Config
}

func NewProxy(config Config) (*Proxy, error) {
	ctx := context.Background()

	var client *storage.Client
	var err error

	if config.credentials != "" {
		client, err = storage.NewClient(ctx, option.WithCredentialsFile(config.credentials))
	} else {
		client, err = storage.NewClient(ctx)
	}
	if err != nil {
		return nil, err
	}

	return &Proxy{client: client, config: config}, nil
}

func (p *Proxy) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/favicon.ico", http.NotFound)

	mux.HandleFunc("/health-check", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.Handle("/", p.wrapper(p.proxy))
}

func (p *Proxy) sendResponse(w http.ResponseWriter, code int, err error) {
	if p.config.verbose && err != nil {
		log.Println(err)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	fmt.Fprintln(w, http.StatusText(code))
}

type wrapResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *wrapResponseWriter) WriteHeader(status int) {
	w.ResponseWriter.WriteHeader(status)
	w.status = status
}

func (p *Proxy) wrapper(fn func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		writer := &wrapResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		fn(writer, r)

		if p.config.accessLog {
			addr := r.RemoteAddr
			if ip, found := header(r, "X-Forwarded-For"); found {
				addr = ip
			}
			log.Printf("[%s] %.3f %d %s %s", addr, time.Now().Sub(t).Seconds(), writer.status, r.Method, r.URL)
		}
	}
}

func (p *Proxy) proxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		p.sendResponse(w, http.StatusBadRequest, nil)
		return
	}

	if len(p.config.signatureKey) != 0 {
		if err := validateSignature(p.config.signatureKey, r); err != nil {
			p.sendResponse(w, http.StatusForbidden, err)
			return
		}
	}

	parts := strings.SplitN(r.URL.Path[1:], "/", 2)
	bucket := parts[0]
	object := parts[1]

	o := p.client.Bucket(bucket).Object(object)
	ctx := context.Background()

	attr, err := o.Attrs(ctx)
	if err != nil {
		if e, ok := err.(*googleapi.Error); ok {
			p.sendResponse(w, e.Code, err)
			return
		}
		p.sendResponse(w, http.StatusInternalServerError, err)
		return
	}
	setStrHeader(w, "Cache-Control", attr.CacheControl)
	setStrHeader(w, "Content-Disposition", attr.ContentDisposition)
	setStrHeader(w, "Content-Encoding", attr.ContentEncoding)
	setStrHeader(w, "Content-Language", attr.ContentLanguage)
	setIntHeader(w, "Content-Length", attr.Size)
	setStrHeader(w, "Content-Type", attr.ContentType)

	or, err := o.NewReader(ctx)
	if err != nil {
		p.sendResponse(w, http.StatusInternalServerError, err)
		return
	}
	io.Copy(w, or)
}

func validateSignature(key []byte, r *http.Request) error {
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
	mac := hmac.New(sha256.New, key)
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
