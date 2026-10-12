package main

import (
	"bufio"
	"context"
	"encoding/base64"
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

// A bare host:port is a SOCKS5 relay. The relay only connects to the
// configured upstream HTTP proxy; the upstream then connects to destinations.
func parseRelayURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "socks5://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Hostname() == "" || (u.Scheme != "socks5" && u.Scheme != "socks5h" && u.Scheme != "http") {
		return nil, errors.New("relay must be host:port, socks5://host:port, or http://host:port")
	}
	if u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("relay URL must not contain a path, query, or fragment")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("relay port must be between 1 and 65535")
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		return nil, errors.New("invalid relay host and port")
	}
	if u.Scheme == "socks5h" {
		u.Scheme = "socks5"
	}
	u.Path = ""
	return u, nil
}

func relayDisplay(raw string) string {
	u, err := parseRelayURL(raw)
	if err != nil || u == nil {
		return ""
	}
	hasAuth := u.User != nil
	u.User = nil
	if hasAuth {
		return u.String() + " (auth configured)"
	}
	return u.String()
}

func dialViaRelay(ctx context.Context, relay *url.URL, target string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	if relay == nil {
		return dialer.DialContext(ctx, "tcp", target)
	}
	conn, err := dialer.DialContext(ctx, "tcp", relay.Host)
	if err != nil {
		return nil, fmt.Errorf("reach relay: %w", err)
	}
	switch relay.Scheme {
	case "socks5":
		return socks5RelayConnect(ctx, conn, relay, target)
	case "http":
		return httpConnect(ctx, conn, relay, target, "relay")
	default:
		conn.Close()
		return nil, errors.New("unsupported relay protocol")
	}
}

func httpConnect(ctx context.Context, conn net.Conn, proxyURL *url.URL, target, label string) (net.Conn, error) {
	setHandshakeDeadline(conn, ctx)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	var auth string
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		pair := proxyURL.User.Username() + ":" + password
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(pair)) + "\r\n"
	}
	request := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, auth)
	if _, err := io.WriteString(conn, request); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != 200 {
		conn.Close()
		return nil, fmt.Errorf("%s returned HTTP %d", label, resp.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func socks5RelayConnect(ctx context.Context, conn net.Conn, relay *url.URL, target string) (net.Conn, error) {
	setHandshakeDeadline(conn, ctx)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	methods := []byte{5, 1, 0}
	if relay.User != nil {
		methods = []byte{5, 2, 0, 2}
	}
	if _, err := conn.Write(methods); err != nil {
		conn.Close()
		return nil, err
	}
	var selection [2]byte
	if _, err := io.ReadFull(conn, selection[:]); err != nil {
		conn.Close()
		return nil, err
	}
	if selection[0] != 5 || selection[1] == 0xff {
		conn.Close()
		return nil, errors.New("SOCKS5 relay rejected authentication methods")
	}
	if selection[1] == 2 {
		if relay.User == nil {
			conn.Close()
			return nil, errors.New("SOCKS5 relay requires credentials")
		}
		password, _ := relay.User.Password()
		username := relay.User.Username()
		if len(username) > 255 || len(password) > 255 {
			conn.Close()
			return nil, errors.New("SOCKS5 relay credentials are too long")
		}
		packet := append([]byte{1, byte(len(username))}, []byte(username)...)
		packet = append(packet, byte(len(password)))
		packet = append(packet, []byte(password)...)
		if _, err := conn.Write(packet); err != nil {
			conn.Close()
			return nil, err
		}
		var result [2]byte
		if _, err := io.ReadFull(conn, result[:]); err != nil || result != [2]byte{1, 0} {
			conn.Close()
			return nil, errors.New("SOCKS5 relay authentication failed")
		}
	} else if selection[1] != 0 {
		conn.Close()
		return nil, errors.New("SOCKS5 relay selected an unsupported authentication method")
	}

	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		conn.Close()
		return nil, errors.New("invalid SOCKS5 target port")
	}
	request := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			request = append(request, 1)
			request = append(request, v4...)
		} else {
			request = append(request, 4)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) == 0 || len(host) > 255 {
			conn.Close()
			return nil, errors.New("invalid SOCKS5 target host")
		}
		request = append(request, 3, byte(len(host)))
		request = append(request, host...)
	}
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		conn.Close()
		return nil, err
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		conn.Close()
		return nil, err
	}
	if reply[0] != 5 || reply[1] != 0 {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 relay rejected connection (code %d)", reply[1])
	}
	var addressBytes int64
	switch reply[3] {
	case 1:
		addressBytes = 4
	case 4:
		addressBytes = 16
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			conn.Close()
			return nil, err
		}
		addressBytes = int64(length[0])
	default:
		conn.Close()
		return nil, errors.New("SOCKS5 relay returned an invalid address")
	}
	if _, err := io.CopyN(io.Discard, conn, addressBytes+2); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func setHandshakeDeadline(conn net.Conn, ctx context.Context) {
	deadline := time.Now().Add(tunnelHandshakeTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetDeadline(deadline)
}
