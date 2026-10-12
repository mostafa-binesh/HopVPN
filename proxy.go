package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

const tunnelHandshakeTimeout = 12 * time.Second

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (a *App) serveProxy(listener net.Listener, proxyID string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		a.mu.RLock()
		proxy, ok := a.findLocked(proxyID)
		a.mu.RUnlock()
		if !ok {
			conn.Close()
			continue
		}
		go a.serveClient(conn, proxy)
	}
}

func (a *App) serveClient(conn net.Conn, proxy Proxy) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if first[0] == 0x05 {
		a.serveSOCKS5(conn, reader, proxy)
		return
	}
	a.serveHTTPProxy(conn, reader, proxy)
}

func (a *App) serveHTTPProxy(conn net.Conn, reader *bufio.Reader, proxy Proxy) {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		if req.Method == http.MethodConnect {
			_ = req.Body.Close()
			a.handleHTTPConnect(conn, reader, req.Host, proxy)
			return
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			writeProxyError(conn, 400, "Use this port as an HTTP proxy")
			_ = req.Body.Close()
			return
		}
		upstream, err := parseProxyURL(proxy.URL)
		if err != nil {
			writeProxyError(conn, 502, "Invalid upstream proxy")
			_ = req.Body.Close()
			return
		}
		relay, err := parseRelayURL(proxy.Relay)
		if err != nil {
			writeProxyError(conn, 502, "Invalid relay")
			_ = req.Body.Close()
			return
		}
		transport := newTransport(upstream, relay)
		req.RequestURI = ""
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Proxy-Connection")
		resp, err := transport.RoundTrip(req)
		_ = req.Body.Close()
		if err != nil {
			transport.CloseIdleConnections()
			writeProxyError(conn, 502, "Upstream request failed: "+err.Error())
			return
		}
		resp.Close = true
		_ = resp.Write(conn)
		_ = resp.Body.Close()
		transport.CloseIdleConnections()
		return
	}
}

func (a *App) handleHTTPConnect(client net.Conn, clientReader *bufio.Reader, target string, proxy Proxy) {
	if _, _, err := net.SplitHostPort(target); err != nil {
		writeProxyError(client, 400, "CONNECT target must be host:port")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), tunnelHandshakeTimeout)
	defer cancel()
	upstream, err := dialTunnel(ctx, proxy, target)
	if err != nil {
		writeProxyError(client, 502, "Upstream CONNECT failed: "+err.Error())
		return
	}
	defer upstream.Close()
	_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	bridge(&bufferedConn{Conn: client, reader: clientReader}, upstream)
}

func writeProxyError(conn net.Conn, code int, message string) {
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " ")
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", code, http.StatusText(code), len(message), message)
}

func (a *App) serveSOCKS5(client net.Conn, reader *bufio.Reader, proxy Proxy) {
	_ = client.SetReadDeadline(time.Now().Add(15 * time.Second))
	var greeting [2]byte
	if _, err := io.ReadFull(reader, greeting[:]); err != nil || greeting[0] != 5 || greeting[1] == 0 {
		return
	}
	methods := make([]byte, greeting[1])
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}
	noAuth := false
	for _, method := range methods {
		if method == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		_, _ = client.Write([]byte{5, 0xff})
		return
	}
	_, _ = client.Write([]byte{5, 0})
	var head [4]byte
	if _, err := io.ReadFull(reader, head[:]); err != nil || head[0] != 5 {
		return
	}
	if head[1] != 1 {
		socksReply(client, 7)
		return
	}
	var host string
	switch head[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(reader, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(reader, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(reader, n[:]); err != nil {
			return
		}
		if n[0] == 0 {
			socksReply(client, 8)
			return
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(reader, b); err != nil {
			return
		}
		host = string(b)
	default:
		socksReply(client, 8)
		return
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(reader, portBytes[:]); err != nil {
		return
	}
	port := int(portBytes[0])<<8 | int(portBytes[1])
	if port == 0 {
		socksReply(client, 8)
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	ctx, cancel := context.WithTimeout(context.Background(), tunnelHandshakeTimeout)
	defer cancel()
	upstream, err := dialTunnel(ctx, proxy, target)
	if err != nil {
		socksReply(client, 1)
		return
	}
	defer upstream.Close()
	_ = client.SetReadDeadline(time.Time{})
	socksReply(client, 0)
	bridge(&bufferedConn{Conn: client, reader: reader}, upstream)
}

func socksReply(conn net.Conn, code byte) {
	_, _ = conn.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
}

func bridge(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

func dialTunnel(ctx context.Context, proxy Proxy, target string) (net.Conn, error) {
	u, err := parseProxyURL(proxy.URL)
	if err != nil {
		return nil, err
	}
	relay, err := parseRelayURL(proxy.Relay)
	if err != nil {
		return nil, err
	}
	conn, err := dialViaRelay(ctx, relay, u.Host)
	if err != nil {
		return nil, err
	}
	return httpConnect(ctx, conn, u, target, "upstream proxy")
}

func newTransport(proxyURL, relayURL *url.URL) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !strings.HasPrefix(network, "tcp") {
				return nil, errors.New("only TCP connections are supported")
			}
			return dialViaRelay(ctx, relayURL, addr)
		},
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 12 * time.Second,
		DisableKeepAlives:     true,
	}
}

func testProxy(parent context.Context, proxy Proxy) (string, error) {
	u, err := parseProxyURL(proxy.URL)
	if err != nil {
		return "", err
	}
	relay, err := parseRelayURL(proxy.Relay)
	if err != nil {
		return "", err
	}
	return testProxyAt(parent, u, relay)
}

func testLocalExit(parent context.Context, port int) (string, error) {
	u, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		return "", err
	}
	return testProxyAt(parent, u, nil)
}

func probeError(err error) string {
	if errors.Is(err, io.EOF) {
		return "connection closed unexpectedly"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "connection timed out"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "connection timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "request canceled"
	}
	return err.Error()
}

func testProxyAt(parent context.Context, u, relay *url.URL) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	transport := newTransport(u, relay)
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("test endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", errors.New("test endpoint did not return a valid IP")
	}
	return ip, nil
}
