package main

import (
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html assets/hopport-logo.png
var webFiles embed.FS

var version = "0.1.1"

type Proxy struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Relay    string `json:"relay,omitempty"`
	ExitPort int    `json:"exit_port,omitempty"`
}

type Config struct {
	Proxies []Proxy `json:"proxies"`
}

type App struct {
	mu           sync.RWMutex
	config       Config
	path         string
	uiPort       int
	listeners    map[string]net.Listener
	listenErrors map[string]string
}

func main() {
	defaultPath := "proxies.json"
	if dir, err := os.UserConfigDir(); err == nil {
		defaultPath = filepath.Join(dir, "hopport", "proxies.json")
	}
	configPath := flag.String("config", defaultPath, "path to saved proxies")
	uiPort := flag.Int("ui-port", 18080, "local web interface port")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("HopPort " + version)
		return
	}
	if *uiPort < 1 || *uiPort > 65535 {
		log.Fatal("ui-port must be a valid TCP port")
	}

	app := &App{
		path: *configPath, uiPort: *uiPort,
		listeners: make(map[string]net.Listener), listenErrors: make(map[string]string),
	}
	if err := app.load(); err != nil {
		log.Fatalf("read configuration: %v", err)
	}

	uiListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*uiPort)))
	if err != nil {
		log.Fatalf("start web interface: %v", err)
	}

	app.startSavedListeners()
	log.Printf("HopPort %s: http://127.0.0.1:%d", version, *uiPort)
	server := &http.Server{
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	if err := server.Serve(uiListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		data, _ := webFiles.ReadFile("index.html")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/logo.png", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		data, _ := webFiles.ReadFile("assets/hopport-logo.png")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/api/state", a.apiState)
	mux.HandleFunc("/api/proxies", a.apiProxies)
	mux.HandleFunc("/api/proxies/", a.apiDelete)
	mux.HandleFunc("/api/port", a.apiPort)
	mux.HandleFunc("/api/relay", a.apiRelay)
	mux.HandleFunc("/api/test", a.apiTest)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The UI is local, but reject cross-origin writes and DNS rebinding.
		if r.Host != fmt.Sprintf("127.0.0.1:%d", a.uiPort) && r.Host != fmt.Sprintf("localhost:%d", a.uiPort) {
			http.Error(w, "invalid host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != "http://"+r.Host {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func apiError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decodeJSON(r *http.Request, dst any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(nil, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("only one JSON object is allowed")
	}
	return nil
}

func (a *App) apiState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiError(w, 405, "method not allowed")
		return
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	views := make([]map[string]any, 0, len(a.config.Proxies))
	for _, p := range a.config.Proxies {
		u, err := parseProxyURL(p.URL)
		if err != nil {
			continue
		}
		status := "listening"
		if p.ExitPort == 0 {
			status = "Choose an exit port"
		} else if errText, failed := a.listenErrors[p.ID]; failed {
			status = errText
		} else if _, running := a.listeners[p.ID]; !running {
			status = "Not listening"
		}
		views = append(views, map[string]any{
			"id": p.ID, "name": p.Name, "address": u.Host,
			"exitPort": p.ExitPort, "status": status,
			"relay": relayDisplay(p.Relay),
		})
	}
	writeJSON(w, 200, map[string]any{
		"proxies": views, "uiAddress": fmt.Sprintf("127.0.0.1:%d", a.uiPort), "version": version,
	})
}

func (a *App) apiProxies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, 405, "method not allowed")
		return
	}
	var body struct {
		Name, URL, Relay string
		ExitPort         int
	}
	if err := decodeJSON(r, &body); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	u, err := parseProxyURL(body.URL)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	if err := a.validatePort(body.ExitPort); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	relay, err := parseRelayURL(body.Relay)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	if isLocalHost(u.Hostname()) && u.Port() == strconv.Itoa(body.ExitPort) {
		apiError(w, 400, "upstream cannot point to its own exit port")
		return
	}
	if relay != nil && isLocalHost(relay.Hostname()) && relay.Port() == strconv.Itoa(body.ExitPort) {
		apiError(w, 400, "relay cannot point to this proxy's exit port")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = u.Host
	}
	if len(name) > 80 {
		apiError(w, 400, "name is too long")
		return
	}
	id, err := newID()
	if err != nil {
		apiError(w, 500, "could not generate proxy ID")
		return
	}
	p := Proxy{ID: id, Name: name, URL: u.String(), ExitPort: body.ExitPort}
	if relay != nil {
		p.Relay = relay.String()
	}
	a.mu.Lock()
	if a.portInUseLocked(body.ExitPort, "") {
		a.mu.Unlock()
		apiError(w, 409, "exit port is already assigned to another proxy")
		return
	}
	listener, err := listenPort(body.ExitPort)
	if err != nil {
		a.mu.Unlock()
		apiError(w, 409, "exit port is occupied or unavailable")
		return
	}
	before := a.config.copy()
	a.config.Proxies = append(a.config.Proxies, p)
	err = a.saveLocked()
	if err != nil {
		a.config = before
		listener.Close()
	} else {
		a.listeners[id] = listener
	}
	a.mu.Unlock()
	if err != nil {
		apiError(w, 500, "could not save proxy")
		return
	}
	go a.serveProxy(listener, id)
	writeJSON(w, 201, map[string]string{"id": id})
}

func (a *App) apiDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		apiError(w, 405, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/proxies/")
	if id == "" || strings.Contains(id, "/") {
		apiError(w, 404, "proxy not found")
		return
	}
	a.mu.Lock()
	before := a.config.copy()
	index := -1
	for i, p := range a.config.Proxies {
		if p.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		a.mu.Unlock()
		apiError(w, 404, "proxy not found")
		return
	}
	a.config.Proxies = append(a.config.Proxies[:index], a.config.Proxies[index+1:]...)
	err := a.saveLocked()
	if err != nil {
		a.config = before
	} else {
		if listener := a.listeners[id]; listener != nil {
			listener.Close()
		}
		delete(a.listeners, id)
		delete(a.listenErrors, id)
	}
	a.mu.Unlock()
	if err != nil {
		apiError(w, 500, "could not save proxy list")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiPort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, 405, "method not allowed")
		return
	}
	var body struct {
		ID       string
		ExitPort int
	}
	if err := decodeJSON(r, &body); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	if err := a.validatePort(body.ExitPort); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	a.mu.Lock()
	index := -1
	for i, p := range a.config.Proxies {
		if p.ID == body.ID {
			index = i
			break
		}
	}
	if index < 0 {
		a.mu.Unlock()
		apiError(w, 404, "proxy not found")
		return
	}
	if a.portInUseLocked(body.ExitPort, body.ID) {
		a.mu.Unlock()
		apiError(w, 409, "exit port is already assigned to another proxy")
		return
	}
	u, _ := parseProxyURL(a.config.Proxies[index].URL)
	if isLocalHost(u.Hostname()) && u.Port() == strconv.Itoa(body.ExitPort) {
		a.mu.Unlock()
		apiError(w, 400, "upstream cannot point to its own exit port")
		return
	}
	relay, _ := parseRelayURL(a.config.Proxies[index].Relay)
	if relay != nil && isLocalHost(relay.Hostname()) && relay.Port() == strconv.Itoa(body.ExitPort) {
		a.mu.Unlock()
		apiError(w, 400, "relay cannot point to this proxy's exit port")
		return
	}
	if a.config.Proxies[index].ExitPort == body.ExitPort && a.listeners[body.ID] != nil {
		a.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	listener, err := listenPort(body.ExitPort)
	if err != nil {
		a.mu.Unlock()
		apiError(w, 409, "exit port is occupied or unavailable")
		return
	}
	before := a.config.copy()
	a.config.Proxies[index].ExitPort = body.ExitPort
	err = a.saveLocked()
	if err != nil {
		a.config = before
		listener.Close()
	} else {
		if old := a.listeners[body.ID]; old != nil {
			old.Close()
		}
		a.listeners[body.ID] = listener
		delete(a.listenErrors, body.ID)
	}
	a.mu.Unlock()
	if err != nil {
		apiError(w, 500, "could not save exit port")
		return
	}
	go a.serveProxy(listener, body.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiRelay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, 405, "method not allowed")
		return
	}
	var body struct{ ID, Relay string }
	if err := decodeJSON(r, &body); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	relay, err := parseRelayURL(body.Relay)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	a.mu.Lock()
	index := -1
	for i, p := range a.config.Proxies {
		if p.ID == body.ID {
			index = i
			break
		}
	}
	if index < 0 {
		a.mu.Unlock()
		apiError(w, 404, "proxy not found")
		return
	}
	if relay != nil && isLocalHost(relay.Hostname()) && relay.Port() == strconv.Itoa(a.config.Proxies[index].ExitPort) {
		a.mu.Unlock()
		apiError(w, 400, "relay cannot point to this proxy's exit port")
		return
	}
	before := a.config.copy()
	if relay == nil {
		a.config.Proxies[index].Relay = ""
	} else {
		a.config.Proxies[index].Relay = relay.String()
	}
	err = a.saveLocked()
	if err != nil {
		a.config = before
	}
	a.mu.Unlock()
	if err != nil {
		apiError(w, 500, "could not save relay")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, 405, "method not allowed")
		return
	}
	var body struct{ ID string }
	if err := decodeJSON(r, &body); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	a.mu.RLock()
	p, ok := a.findLocked(body.ID)
	listenerReady := a.listeners[body.ID] != nil
	a.mu.RUnlock()
	if !ok {
		apiError(w, 404, "proxy not found")
		return
	}
	if !listenerReady {
		apiError(w, 409, "local exit port is not listening")
		return
	}
	start := time.Now()
	ip, err := testLocalExit(r.Context(), p.ExitPort)
	if err != nil {
		_, upstreamErr := testProxy(r.Context(), p)
		if upstreamErr != nil {
			apiError(w, 502, "local exit failed; upstream route through relay also failed: "+probeError(upstreamErr))
		} else {
			apiError(w, 502, "local exit failed while upstream route through relay works: "+probeError(err))
		}
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "exitIP": ip, "latencyMs": time.Since(start).Milliseconds()})
}
