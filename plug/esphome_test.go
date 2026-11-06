package plug

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"foxtrack-bridge/config"
)

// esphomeFake is a minimal ESPHome web_server. /events sends ping, log and
// state events, then stays open until the client goes away.
type esphomeFake struct {
	mu     sync.Mutex
	states []string          // state JSON sent on each /events connect
	rest   map[string]string // escaped REST path -> JSON answer; other paths 404
	user   string            // basic auth login, "" for none
	pass   string
	noWeb  bool // /events answers 404
	posts  []string
	events int // /events connects
}

func (f *esphomeFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.user != "" {
		if u, p, ok := r.BasicAuth(); !ok || u != f.user || p != f.pass {
			f.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if r.URL.Path == "/events" {
		f.events++
		noWeb := f.noWeb
		states := append([]string(nil), f.states...)
		f.mu.Unlock()
		if noWeb {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, esphomeSSE("ping", `{"title":"ESPHome","comment":"fake"}`))
		fmt.Fprint(w, esphomeSSE("log", `{"level":"DEBUG","message":"boot"}`))
		for _, s := range states {
			fmt.Fprint(w, esphomeSSE("state", s))
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	defer f.mu.Unlock()
	path := r.URL.EscapedPath()
	if r.Method == http.MethodPost {
		base := path[:strings.LastIndex(path, "/")]
		if _, ok := f.rest[base]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.posts = append(f.posts, path)
		w.WriteHeader(http.StatusOK)
		return
	}
	body, ok := f.rest[path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	fmt.Fprint(w, body)
}

// esphomeSSE formats one event the way ESPHome's sse_chunk.cpp does: CRLF lines.
func esphomeSSE(name, data string) string {
	return "retry: 30000\r\nid: 1\r\nevent: " + name + "\r\ndata: " + data + "\r\n\r\n"
}

// newESPHomeFake starts the fake and returns its host:port. Each server has its
// own port, so each test gets its own cache entry.
func newESPHomeFake(t *testing.T, f *esphomeFake) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func esphomeCachedPaths(t *testing.T, host string, g config.Plug) esphomePath {
	t.Helper()
	v, ok := esphomePaths.Load(esphomeKey(host, g))
	if !ok {
		t.Fatal("no cached ESPHome paths")
	}
	return v.(esphomePath)
}

func TestESPHomeNewFormReadsSwitchAndPower(t *testing.T) {
	f := &esphomeFake{
		states: []string{
			`{"id":"switch/Power","state":"ON","value":true}`,
			`{"id":"sensor/Power","state":"152.4 W","value":152.4}`,
			`{"id":"sensor/Voltage","state":"230.1 V","value":230.1}`,
			`{"id":"switch/Child Lock","state":"OFF","value":false}`,
		},
		rest: map[string]string{
			"/switch/Power": `{"id":"switch/Power","state":"ON","value":true}`,
			"/sensor/Power": `{"id":"sensor/Power","state":"152.4 W","value":152.4}`,
		},
	}
	host := newESPHomeFake(t, f)
	g := config.Plug{Kind: config.PlugESPHome, Address: host}
	r, err := Read(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if !r.On || r.Watts == nil || *r.Watts != 152.4 || r.EnergyWh != nil {
		t.Fatalf("Read = %+v", r)
	}
	p := esphomeCachedPaths(t, host, g)
	if p.switchPath != "/switch/Power" || p.powerPath != "/sensor/Power" {
		t.Fatalf("paths = %+v", p)
	}
}

func TestESPHomeEntityWithSpaceIsEscaped(t *testing.T) {
	f := &esphomeFake{
		states: []string{
			`{"id":"switch/Power","state":"ON","value":true}`,
			`{"id":"sensor/Power","state":"152.4 W","value":152.4}`,
			`{"id":"switch/Child Lock","state":"OFF","value":false}`,
		},
		rest: map[string]string{
			"/switch/Power":        `{"id":"switch/Power","state":"ON","value":true}`,
			"/switch/Child%20Lock": `{"id":"switch/Child Lock","state":"OFF","value":false}`,
			"/sensor/Power":        `{"id":"sensor/Power","state":"152.4 W","value":152.4}`,
		},
	}
	host := newESPHomeFake(t, f)
	g := config.Plug{Kind: config.PlugESPHome, Address: host, Entity: "Child Lock"}
	r, err := Read(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if r.On || r.Watts == nil || *r.Watts != 152.4 {
		t.Fatalf("Read = %+v", r)
	}
	if p := esphomeCachedPaths(t, host, g); p.switchPath != "/switch/Child%20Lock" {
		t.Fatalf("switch path = %q", p.switchPath)
	}
	if err := Switch(context.Background(), g, true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posts) != 1 || f.posts[0] != "/switch/Child%20Lock/turn_on" {
		t.Fatalf("posts = %v", f.posts)
	}
}

func TestESPHomeLegacyIDs(t *testing.T) {
	f := &esphomeFake{
		states: []string{
			`{"id":"switch-relay","state":"OFF","value":false}`,
			`{"id":"sensor-power","state":"12.0 W","value":12}`,
		},
		rest: map[string]string{
			"/switch/relay": `{"id":"switch-relay","state":"OFF","value":false}`,
			"/sensor/power": `{"id":"sensor-power","state":"12.0 W","value":12}`,
		},
	}
	host := newESPHomeFake(t, f)
	r, err := Read(context.Background(), config.Plug{Kind: config.PlugESPHome, Address: host})
	if err != nil {
		t.Fatal(err)
	}
	if r.On || r.Watts == nil || *r.Watts != 12 {
		t.Fatalf("Read = %+v", r)
	}
}

func TestESPHomeNameIDPreferred(t *testing.T) {
	f := &esphomeFake{
		states: []string{
			`{"id":"switch-relay","name_id":"switch/Power","state":"ON","value":true}`,
			`{"id":"sensor-power","name_id":"sensor/Power","state":"5.0 W","value":5}`,
		},
		// Only the new-form paths exist: the legacy /switch/relay would 404.
		rest: map[string]string{
			"/switch/Power": `{"id":"switch/Power","state":"ON","value":true}`,
			"/sensor/Power": `{"id":"sensor/Power","state":"5.0 W","value":5}`,
		},
	}
	host := newESPHomeFake(t, f)
	r, err := Read(context.Background(), config.Plug{Kind: config.PlugESPHome, Address: host})
	if err != nil {
		t.Fatal(err)
	}
	if !r.On || r.Watts == nil || *r.Watts != 5 {
		t.Fatalf("Read = %+v", r)
	}
}

func TestESPHomeSwitchPosts(t *testing.T) {
	f := &esphomeFake{
		states: []string{`{"id":"switch/Power","state":"ON","value":true}`},
		rest: map[string]string{
			"/switch/Power": `{"id":"switch/Power","state":"ON","value":true}`,
		},
	}
	host := newESPHomeFake(t, f)
	g := config.Plug{Kind: config.PlugESPHome, Address: host}
	if err := Switch(context.Background(), g, true); err != nil {
		t.Fatal(err)
	}
	if err := Switch(context.Background(), g, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posts) != 2 || f.posts[0] != "/switch/Power/turn_on" || f.posts[1] != "/switch/Power/turn_off" {
		t.Fatalf("posts = %v", f.posts)
	}
}

func TestESPHomeBasicAuth(t *testing.T) {
	f := &esphomeFake{
		user:   "u",
		pass:   "p",
		states: []string{`{"id":"switch/Power","state":"ON","value":true}`},
		rest: map[string]string{
			"/switch/Power": `{"id":"switch/Power","state":"ON","value":true}`,
		},
	}
	host := newESPHomeFake(t, f)
	_, err := Read(context.Background(), config.Plug{Kind: config.PlugESPHome, Address: host})
	if err != errESPHomeLogin {
		t.Fatalf("without login: err = %v", err)
	}
	r, err := Read(context.Background(), config.Plug{Kind: config.PlugESPHome, Address: host, Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.On {
		t.Fatalf("Read = %+v", r)
	}
}

func TestESPHomeNoWebServer(t *testing.T) {
	f := &esphomeFake{noWeb: true}
	host := newESPHomeFake(t, f)
	_, err := Read(context.Background(), config.Plug{Kind: config.PlugESPHome, Address: host})
	if err != errESPHomeNoWeb {
		t.Fatalf("err = %v", err)
	}
}

func TestESPHomeStaleCacheRediscoversOnce(t *testing.T) {
	f := &esphomeFake{
		states: []string{
			`{"id":"switch-relay","state":"ON","value":true}`,
		},
		rest: map[string]string{
			"/switch/relay": `{"id":"switch-relay","state":"ON","value":true}`,
		},
	}
	host := newESPHomeFake(t, f)
	g := config.Plug{Kind: config.PlugESPHome, Address: host}
	if _, err := Read(context.Background(), g); err != nil {
		t.Fatal(err)
	}

	// The device was renamed: the cached /switch/relay now 404s.
	f.mu.Lock()
	delete(f.rest, "/switch/relay")
	f.rest["/switch/Power"] = `{"id":"switch/Power","state":"OFF","value":false}`
	f.states = []string{`{"id":"switch/Power","state":"OFF","value":false}`}
	f.mu.Unlock()

	r, err := Read(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if r.On {
		t.Fatalf("Read = %+v", r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.events != 2 {
		t.Fatalf("/events connects = %d, want 2 (one discovery, one rediscovery)", f.events)
	}
}
