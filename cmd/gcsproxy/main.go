package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gorilla/mux"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gorilla/mux/otelmux"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"go.opentelemetry.io/otel/sdk/resource"
)

var (
	bind          = flag.String("b", "127.0.0.1:8080", "Bind address.")
	creds         = flag.String("c", "", "The path to the keyfile. If not present, client will use your default application credentials.")
	redirect404   = flag.Bool("r", false, "Redirect to index.html if 404 not found.")
	indexPage     = flag.String("i", "", "Index page file name.")
	useDomainName = flag.Bool("dn", false, "Use hostname as a bucket name.")
	useSecret     = flag.String("s", "", "Use SA key from secretManager. E.G. 'projects/937192795301/secrets/gcs-proxy/versions/1'")
	verbose       = flag.Bool("v", false, "Show access log.")

	enableOtel = flag.Bool("otel", false, "Enable opentelemetry.")
)

var (
	client *storage.Client
	ctx    = context.Background()
)

func handleErrorRW(w http.ResponseWriter, err error) {
	if err != nil {
		if err == storage.ErrObjectNotExist {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
}

func handleErrStr(err error, message string) {
	if err != nil {
		log.Fatalf("%s: %v", message, err)
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
				time.Since(proc).Seconds(),
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
	if *indexPage != "" && params["object"] == "" {
		params["object"] = *indexPage
	}

	obj := client.Bucket(params["bucket"]).Object(params["object"])
	attr, err := obj.Attrs(ctx)

	if err == storage.ErrObjectNotExist && *redirect404 {
		// Remove first slash, otherwise it won't find an object. Add tailing slash if missing.
		u := r.URL.RequestURI()[1:]
		if l := len(u) - 1; u[l:] != "/" {
			u = u + "/"
		}

		obj = client.Bucket(params["bucket"]).Object(u + *indexPage)
		attr, err = obj.Attrs(ctx)

		if err == storage.ErrObjectNotExist {
			obj = client.Bucket(params["bucket"]).Object(*indexPage)
			attr, err = obj.Attrs(ctx)
		}
	}

	if err != nil {
		handleErrorRW(w, err)
		return
	}

	setStrHeader(w, "Content-Type", attr.ContentType)
	setStrHeader(w, "Content-Language", attr.ContentLanguage)
	setStrHeader(w, "Cache-Control", attr.CacheControl)
	setStrHeader(w, "Content-Encoding", attr.ContentEncoding)
	setStrHeader(w, "Content-Disposition", attr.ContentDisposition)
	setStrHeader(w, "X-Goog-Authenticated-User-Id", r.Header.Get("X-Goog-Authenticated-User-Id"))
	setStrHeader(w, "X-Goog-Authenticated-User-Email", r.Header.Get("X-Goog-Authenticated-User-Email"))
	setIntHeader(w, "Content-Length", attr.Size)

	objr, err := obj.NewReader(ctx)
	if err != nil {
		handleErrorRW(w, err)
		return
	}
	_, err = io.Copy(w, objr)
	if err != nil {
		handleErrorRW(w, err)
		return
	}
}

func initTracer() *sdktrace.TracerProvider {
	ctx := context.Background()
	var connType otlptracegrpc.Option

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "127.0.0.1:4317" // setting default endpoint for exporter
	}

	insecure := os.Getenv("OTEL_EXPORTER_OTLP_SECURE")
	if insecure == "" || insecure == "false" {
		connType = otlptracegrpc.WithInsecure()
	} else {
		connType = otlptracegrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, ""))
	}

	// Create and start new OTLP trace exporter
	traceExporter, err := otlptracegrpc.New(ctx, connType, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithDialOption(grpc.WithBlock()))
	handleErrStr(err, "failed to create new OTLP trace exporter")

	service := os.Getenv("GO_GORILLA_SERVICE_NAME")
	if service == "" {
		service = "gcs-proxy"
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		// the service name used to display traces in backends
		semconv.ServiceNameKey.String(service),
	)
	handleErrStr(err, "failed to create resource")

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	return tp
}

func main() {
	flag.Parse()

	var err error
	var path string

	if *creds != "" {
		client, err = storage.NewClient(ctx, option.WithCredentialsFile(*creds))
	} else if *useSecret != "" {
		client, err = storage.NewClient(ctx, option.WithCredentialsFile(GetSecret(*useSecret)))
	} else {
		client, err = storage.NewClient(ctx)
	}

	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	if !*useDomainName {
		path = "/{bucket:[0-9a-zA-Z-_.]+}"
	}

	r := mux.NewRouter()

	if *enableOtel {
		tp := initTracer()
		defer func() {
			if err := tp.Shutdown(context.Background()); err != nil {
				log.Printf("Error shutting down tracer provider: %v", err)
			}
		}()
		r.Use(otelmux.Middleware("gcs-proxy"))
	}

	r.HandleFunc(path+"/{object:.*}", wrapper(proxy)).Methods("GET", "HEAD", "POST")

	log.Printf("[service] listening on %s", *bind)
	if err := http.ListenAndServe(*bind, r); err != nil {
		log.Fatal(err)
	}
}
