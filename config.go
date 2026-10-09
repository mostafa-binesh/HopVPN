package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func parseProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Scheme != "http" || u.Hostname() == "" {
		return nil, errors.New("enter an HTTP proxy URL, such as http://user:pass@host:port")
	}
	if u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("proxy URL must not contain a path, query, or fragment")
	}
	port := u.Port()
	if port == "" {
		return nil, errors.New("proxy URL must include a port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("proxy port must be between 1 and 65535")
	}
	// url.Parse already checks bracketed IPv6; this also catches malformed hosts.
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		return nil, errors.New("invalid proxy host and port")
	}
	u.Path = ""
	return u, nil
}

func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (c Config) copy() Config {
	c.Proxies = append([]Proxy(nil), c.Proxies...)
	return c
}

func (a *App) findLocked(id string) (Proxy, bool) {
	for _, p := range a.config.Proxies {
		if p.ID == id {
			return p, true
		}
	}
	return Proxy{}, false
}

func (a *App) load() error {
	data, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		a.config.Proxies = []Proxy{}
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &a.config); err != nil {
		return err
	}
	for _, p := range a.config.Proxies {
		if p.ID == "" {
			return fmt.Errorf("proxy %q has no ID", p.Name)
		}
		if _, err := parseProxyURL(p.URL); err != nil {
			return fmt.Errorf("proxy %q: %w", p.Name, err)
		}
		if _, err := parseRelayURL(p.Relay); err != nil {
			return fmt.Errorf("proxy %q: %w", p.Name, err)
		}
		if p.ExitPort < 0 || p.ExitPort > 65535 {
			return fmt.Errorf("proxy %q has an invalid exit port", p.Name)
		}
	}
	return nil
}

func (a *App) validatePort(port int) error {
	if port < 1 || port > 65535 {
		return errors.New("exit port must be between 1 and 65535")
	}
	if port == a.uiPort {
		return errors.New("exit port must differ from the web interface port")
	}
	return nil
}

func (a *App) portInUseLocked(port int, exceptID string) bool {
	for _, p := range a.config.Proxies {
		if p.ID != exceptID && p.ExitPort == port {
			return true
		}
	}
	return false
}

func isLocalHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func listenPort(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

func (a *App) startSavedListeners() {
	used := make(map[int]bool)
	for _, p := range a.config.Proxies {
		if p.ExitPort == 0 {
			continue // Older configs did not have per-proxy ports.
		}
		if p.ExitPort == a.uiPort || used[p.ExitPort] {
			a.listenErrors[p.ID] = "Exit port conflicts with another app port"
			continue
		}
		used[p.ExitPort] = true
		listener, err := listenPort(p.ExitPort)
		if err != nil {
			a.listenErrors[p.ID] = "Exit port is occupied or unavailable"
			continue
		}
		a.listeners[p.ID] = listener
		go a.serveProxy(listener, p.ID)
	}
}

// saveLocked requires App.mu. The file contains credentials and is mode 0600.
func (a *App) saveLocked() error {
	dir := filepath.Dir(a.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".hopport-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	data, err := json.MarshalIndent(a.config, "", "  ")
	if err != nil {
		file.Close()
		return err
	}
	data = append(data, '\n')
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), a.path)
}
