package node

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gansoi/gansoi/ca"
	"github.com/gansoi/gansoi/cluster"
	"github.com/gansoi/gansoi/logger"
	"github.com/gansoi/gansoi/stats"
)

// HTTPStream implements a raft stream for use with Golang's net/http.
type HTTPStream struct {
	closed       bool
	addr         string
	accepted     chan net.Conn
	dial         net.Dialer
	certificates []tls.Certificate
	ca           *ca.CA
	rootCAs      *x509.CertPool
}

func init() {
	stats.CounterInit("http_dialed")
	stats.CounterInit("http_failed")
	stats.CounterInit("http_served")
	stats.CounterInit("http_accepted")
}

// NewHTTPStream will instantiate a new HTTPStream.
func NewHTTPStream(addr string, certificates []tls.Certificate, coreCA *ca.CA) (*HTTPStream, error) {
	// Set up our own dialer.
	dial := net.Dialer{
		// Some crappy NAT devices will close a connection after 30 seconds of
		// inactivity. We try to keep alive every 25th second to counter this.
		KeepAlive: time.Second * 25,
	}

	h := &HTTPStream{
		addr:         addr,
		accepted:     make(chan net.Conn),
		dial:         dial,
		certificates: certificates,
		ca:           coreCA,
		rootCAs:      coreCA.CertPool(),
	}

	return h, nil
}

// Dial will dial a remote http endpoint (and implement raft.StreamLayer).
func (h *HTTPStream) Dial(address string, timeout time.Duration) (net.Conn, error) {
	var conn net.Conn
	var err error

	// Make a copy of our dialer to allow custom timeout.
	dial := h.dial
	dial.Timeout = timeout

	if strings.IndexRune(address, ':') < 0 {
		address += ":4934"
	}

	stats.CounterInc("http_dialed", 1)

	logger.Debug("httpstream", "Dialing %s", address)

	conf := &tls.Config{
		RootCAs:            h.rootCAs,
		Certificates:       h.certificates,
		ServerName:         address,
		InsecureSkipVerify: true,
	}
	conn, err = tls.DialWithDialer(&dial, "tcp", address, conf)

	if err != nil {
		stats.CounterInc("http_failed", 1)
		fmt.Printf("ERRRRRROR %s\n", err.Error())
		return nil, err
	}

	// We use Upgrade, and hope that will make proxies happy.
	open := fmt.Sprintf("GET %s/raft HTTP/1.1\nHost: %s\nUpgrade: raft-0\n\n", cluster.CorePrefix, address)

	_, err = conn.Write([]byte(open))
	if err != nil {
		conn.Close()

		stats.CounterInc("http_failed", 1)
		return nil, err
	}

	return conn, nil
}

// Accept waits for and returns the next connection to the listener.
func (h *HTTPStream) Accept() (net.Conn, error) {
	if h.closed {
		return nil, errors.New("Server is shutting down")
	}

	stats.CounterInc("http_accepted", 1)

	return <-h.accepted, nil
}

// Close closes the listener.
// Any blocked Accept operations will be unblocked and return errors.
func (h *HTTPStream) Close() error {
	h.closed = true

	return nil
}

// Addr returns the listener's network address.
func (h *HTTPStream) Addr() net.Addr {
	return h
}

// String implements net.Addr.
func (h *HTTPStream) String() string {
	return h.addr
}

// Network implements net.Addr.
func (h *HTTPStream) Network() string {
	return "tcp"
}

// ServeHTTP implements the http.Handler interface.
func (h *HTTPStream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	stats.CounterInc("http_served", 1)

	if h.closed {
		http.Error(w, "Server is shutting down", http.StatusServiceUnavailable)
		return
	}

	upgrade := r.Header.Get("Upgrade")
	if upgrade != "raft-0" {
		http.Error(w, "This endpoint is for streaming raft", http.StatusBadRequest)
		return
	}

	_, err := h.ca.VerifyHTTPRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "webserver doesn't support hijacking", http.StatusInternalServerError)
		return
	}

	// We hijack the connection.
	conn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Undo deadlines set by webserver.
	conn.SetDeadline(time.Time{})
	conn.SetWriteDeadline(time.Time{})

	h.accepted <- conn
}
