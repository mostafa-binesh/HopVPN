package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func startTestListener(t *testing.T, handle func(net.Conn)) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	return listener
}

func startTestUpstream(t *testing.T) net.Listener {
	return startTestListener(t, func(conn net.Conn) {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(conn)
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		_ = req.Body.Close()
		if req.Method == http.MethodConnect {
			_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
			var message [4]byte
			if _, err := io.ReadFull(reader, message[:]); err == nil && string(message[:]) == "PING" {
				_, _ = io.WriteString(conn, "PONG")
			}
			return
		}
		body := "proxy-ok"
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	})
}

func startTestHTTPRelay(t *testing.T, targets chan<- string) net.Listener {
	return startTestListener(t, func(conn net.Conn) {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(conn)
		req, err := http.ReadRequest(reader)
		if err != nil || req.Method != http.MethodConnect {
			return
		}
		_ = req.Body.Close()
		targets <- req.Host
		next, err := net.DialTimeout("tcp", req.Host, 3*time.Second)
		if err != nil {
			return
		}
		defer next.Close()
		_ = conn.SetDeadline(time.Time{})
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		bridge(&bufferedConn{Conn: conn, reader: reader}, next)
	})
}

func startTestSOCKSRelay(t *testing.T, targets chan<- string) net.Listener {
	return startTestListener(t, func(conn net.Conn) {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var hello [2]byte
		if _, err := io.ReadFull(conn, hello[:]); err != nil || hello[0] != 5 {
			return
		}
		methods := make([]byte, hello[1])
		if _, err := io.ReadFull(conn, methods); err != nil {
			return
		}
		_, _ = conn.Write([]byte{5, 0})
		var request [4]byte
		if _, err := io.ReadFull(conn, request[:]); err != nil || request[0] != 5 || request[1] != 1 {
			return
		}
		var host string
		switch request[3] {
		case 1:
			address := make([]byte, 4)
			if _, err := io.ReadFull(conn, address); err != nil {
				return
			}
			host = net.IP(address).String()
		case 3:
			var length [1]byte
			if _, err := io.ReadFull(conn, length[:]); err != nil {
				return
			}
			address := make([]byte, length[0])
			if _, err := io.ReadFull(conn, address); err != nil {
				return
			}
			host = string(address)
		default:
			return
		}
		var portBytes [2]byte
		if _, err := io.ReadFull(conn, portBytes[:]); err != nil {
			return
		}
		port := int(portBytes[0])<<8 | int(portBytes[1])
		target := net.JoinHostPort(host, strconv.Itoa(port))
		targets <- target
		next, err := net.DialTimeout("tcp", target, 3*time.Second)
		if err != nil {
			return
		}
		defer next.Close()
		_ = conn.SetDeadline(time.Time{})
		_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
		bridge(conn, next)
	})
}

func TestRelayChainAndNoFallback(t *testing.T) {
	for _, kind := range []string{"socks5", "http"} {
		t.Run(kind, func(t *testing.T) {
			upstream := startTestUpstream(t)
			targets := make(chan string, 3)
			var relay net.Listener
			if kind == "socks5" {
				relay = startTestSOCKSRelay(t, targets)
			} else {
				relay = startTestHTTPRelay(t, targets)
			}
			proxy := Proxy{URL: "http://" + upstream.Addr().String(), Relay: kind + "://" + relay.Addr().String()}
			conn, err := dialTunnel(context.Background(), proxy, "destination.example:443")
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = io.WriteString(conn, "PING")
			var reply [4]byte
			if _, err := io.ReadFull(conn, reply[:]); err != nil || string(reply[:]) != "PONG" {
				t.Fatalf("tunnel reply: %q, %v", reply, err)
			}
			conn.Close()
			if target := <-targets; target != upstream.Addr().String() {
				t.Fatalf("relay connected to %q, want upstream %q", target, upstream.Addr())
			}

			upstreamURL, _ := url.Parse(proxy.URL)
			relayURL, _ := parseRelayURL(proxy.Relay)
			transport := newTransport(upstreamURL, relayURL)
			resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Get("http://destination.example/path")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			transport.CloseIdleConnections()
			if string(body) != "proxy-ok" {
				t.Fatalf("HTTP body = %q", body)
			}
			if target := <-targets; target != upstream.Addr().String() {
				t.Fatalf("HTTP relay connected to %q, want upstream %q", target, upstream.Addr())
			}

			relay.Close()
			if conn, err := dialTunnel(context.Background(), proxy, "destination.example:443"); err == nil {
				conn.Close()
				t.Fatal("connected directly after relay was closed")
			}
			transport = newTransport(upstreamURL, relayURL)
			if resp, err := (&http.Client{Transport: transport, Timeout: 3 * time.Second}).Get("http://destination.example/path"); err == nil {
				resp.Body.Close()
				t.Fatal("HTTP request connected directly after relay was closed")
			}
			transport.CloseIdleConnections()
		})
	}
}

func TestParseRelayURL(t *testing.T) {
	for _, input := range []string{"localhost:10808", "socks5://127.0.0.1:10808", "http://user:pass@127.0.0.1:10808"} {
		if _, err := parseRelayURL(input); err != nil {
			t.Errorf("%q: %v", input, err)
		}
	}
	if u, _ := parseRelayURL("localhost:10808"); u.Scheme != "socks5" {
		t.Fatalf("bare address scheme = %q", u.Scheme)
	}
	if !strings.Contains(relayDisplay("http://user:pass@127.0.0.1:10808"), "auth configured") || strings.Contains(relayDisplay("http://user:pass@127.0.0.1:10808"), "pass") {
		t.Fatal("relay display exposed credentials")
	}
}
