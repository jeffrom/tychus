package tychus

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

type proxy struct {
	config   *Configuration
	errorStr string
	requests chan bool
	revproxy *httputil.ReverseProxy
	unpause  chan bool
}

// Returns a newly configured proxy
func newProxy(c *Configuration) *proxy {
	url, err := url.Parse(fmt.Sprintf("%s:%v", "http://localhost", c.AppPort))
	if err != nil {
		c.Logger.Fatal(err)
	}

	revproxy := httputil.NewSingleHostReverseProxy(url)
	if dbg := os.Getenv("DEBUG"); dbg == "" {
		revproxy.ErrorLog = log.New(ioutil.Discard, "proxy", 0)
	}

	p := &proxy{
		config:   c,
		requests: make(chan bool),
		revproxy: revproxy,
		unpause:  make(chan bool),
	}

	return p
}

func (p *proxy) start() error {
	server := &http.Server{Handler: p}

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", "localhost", p.config.ProxyPort))
	if err != nil {
		return err
	}
	defer listener.Close()

	p.config.Logger.Printf("Proxing requests on port %v to %v", p.config.ProxyPort, p.config.AppPort)

	err = server.Serve(listener)
	if err != nil {
		return err
	}

	return nil
}

// Proxy the request to the application server.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.requests <- true

	<-p.unpause

	if ok := p.forward(w, r); ok {
		return
	}

	timeout := time.After(time.Second * time.Duration(p.config.Timeout))
	tick := time.Tick(50 * time.Millisecond)

	ctx := r.Context()

	for {
		select {
		case <-tick:
			if ok := p.forward(w, r); ok {
				return
			}

		case <-timeout:
			p.config.Logger.Print("Timeout reached")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("Connection Refused"))

			return

		case <-ctx.Done():
			return
		}
	}
}

func (p *proxy) forward(w http.ResponseWriter, r *http.Request) bool {
	if len(p.errorStr) > 0 {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(p.errorStr))
		return true
	}

	body := &reqBody{r: r.Body, size: int(r.ContentLength)}
	r.Body = body

	writer := &proxyWriter{res: w}
	p.revproxy.ServeHTTP(writer, r)

	if writer.status < 200 || writer.status >= 300 && r.Body != nil {
		r.Body = body.NextReader()
	}

	// If the request is "successful" - as in the server responded in
	// some way, return the response to the client.
	return writer.status != http.StatusBadGateway
}

func (p *proxy) setError(err error) {
	p.config.Logger.Debug("Proxy: Error Mode")
	p.errorStr = err.Error()
}

func (p *proxy) clearError() {
	p.errorStr = ""
}

// Wrapper around http.ResponseWriter. Since the proxy works rather naively -
// it just retries requests over and over until it gets a response from the app
// server - we can't use the ResponseWriter that is passed to the handler
// because you cannot call WriteHeader multiple times.
type proxyWriter struct {
	res    http.ResponseWriter
	status int
}

func (w *proxyWriter) WriteHeader(status int) {
	if status == 502 {
		w.status = status
		return
	}

	w.res.WriteHeader(status)
}

func (w *proxyWriter) Write(body []byte) (int, error) {
	return w.res.Write(body)
}

func (w *proxyWriter) Header() http.Header {
	return w.res.Header()
}

type reqBody struct {
	r    io.ReadCloser
	b    []byte
	size int
}

func (b *reqBody) Read(p []byte) (int, error) {
	if b.b == nil {
		b.b = make([]byte, b.size)
	}
	n, err := b.r.Read(p)
	// fmt.Printf("reqBody: %q %+v\n", p[:b.size], err)
	copy(b.b, p)
	return n, err
}

func (b *reqBody) Close() error { return nil }

func (b *reqBody) NextReader() io.ReadCloser {
	return &closingBuffer{Buffer: bytes.NewBuffer(b.b)}
}

type closingBuffer struct {
	*bytes.Buffer
}

func (b *closingBuffer) Close() error { return nil }
