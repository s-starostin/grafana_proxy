package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	grafanaUser = "admin"
	grafanaHost = "localhost:3000"
	grafanaHeader = "X-GRAFANA-WEBAUTH-USER"
	)

type middleware func(http.Handler) http.Handler
type middlewares []middleware

func (mws middlewares) apply(hdlr http.Handler) http.Handler {
	if len(mws) == 0 {
		return hdlr
	}
	return mws[1:].apply(mws[0](hdlr))
}

func (c *controller) shutdown(ctx context.Context, server *http.Server) context.Context {
	ctx, done := context.WithCancel(ctx)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer done()

		<-quit
		signal.Stop(quit)
		close(quit)

		atomic.StoreInt64(&c.healthy, 0)
		server.ErrorLog.Printf("Server is shutting down...\n")

		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		server.SetKeepAlivesEnabled(false)
		if err := server.Shutdown(ctx); err != nil {
			server.ErrorLog.Fatalf("Could not gracefully shutdown the server: %s\n", err)
		}
	}()

	return ctx
}

type controller struct {
	logger        *log.Logger
	nextRequestID func() string
	healthy       int64
}

func SendJSONError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `{"error":{"msg":%q}}`, msg)
}

func main() {
	listenAddr := ":4000"
	if len(os.Args) == 2 {
		listenAddr = os.Args[1]
	}

	logger := log.New(os.Stdout, "http: ", log.LstdFlags)
	logger.Printf("Server is starting...")

	c := &controller{logger: logger, nextRequestID: func() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }}
	router := http.NewServeMux()
	router.HandleFunc("/", c.index)
	router.HandleFunc("/healthz", c.healthz)

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      (middlewares{c.tracing, c.logging}).apply(router),
		ErrorLog:     logger,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}
	ctx := c.shutdown(context.Background(), server)

	logger.Printf("Server is ready to handle requests at %q\n", listenAddr)
	atomic.StoreInt64(&c.healthy, time.Now().UnixNano())

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		logger.Fatalf("Could not listen on %q: %s\n", listenAddr, err)
	}
	<-ctx.Done()
	logger.Printf("Server stopped\n")
}

func (c *controller) index(w http.ResponseWriter, req *http.Request) {
	url, err := url.Parse(fmt.Sprintf("http://%s/", grafanaHost))
	if err != nil {
		SendJSONError(w, err.Error())
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(url)

	fmt.Println(req.URL.Host)
//	req.Header.Set(grafanaHeader, grafanaUser)

	proxy.ServeHTTP(w, req)
}

func (c *controller) healthz(w http.ResponseWriter, req *http.Request) {
	if h := atomic.LoadInt64(&c.healthy); h == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		fmt.Fprintf(w, "uptime: %s\n", time.Since(time.Unix(0, h)))
	}
}

func (c *controller) logging(hdlr http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func(start time.Time) {
			requestID := w.Header().Get("X-Request-Id")
			if requestID == "" {
				requestID = "unknown"
			}
			c.logger.Println(requestID, req.Method, req.URL.Path, req.RemoteAddr, req.UserAgent(), time.Since(start))
			for k, v := range req.Header {
                c.logger.Println(requestID, k, v)
            }
            c.logger.Println(req.Body)
		}(time.Now())
		hdlr.ServeHTTP(w, req)
	})
}

func (c *controller) tracing(hdlr http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestID := req.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = c.nextRequestID()
		}
		w.Header().Set("X-Request-Id", requestID)
		hdlr.ServeHTTP(w, req)
	})
}

// main_test.go
var (
	_ http.Handler = http.HandlerFunc((&controller{}).index)
	_ http.Handler = http.HandlerFunc((&controller{}).healthz)
	_ middleware   = (&controller{}).logging
	_ middleware   = (&controller{}).tracing
)