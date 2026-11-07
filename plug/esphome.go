package plug

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
)

// --- ESPHome (web_server REST API and /events stream) ---

var (
	errESPHomeLogin        = errors.New("The ESPHome web server needs a login: enter its username and password.")
	errESPHomeNoWeb        = errors.New("This ESPHome device has no web server. Add web_server: to its YAML.")
	errESPHomeNoSwitch     = errors.New("The ESPHome device has no switch. Check that web_server is enabled in its YAML.")
	errESPHomeManySwitches = errors.New("This ESPHome device has several switches. Type the switch name in Entity.")
	errESPHomeRefused      = errors.New("The ESPHome device refused the switch command.")
	errESPHomeFormat       = errors.New("The ESPHome device sent a reply the Bridge does not understand.")
)

const (
	esphomeDiscoverFor = 3 * time.Second
	esphomeQuietFor    = 400 * time.Millisecond
)

// esphomeStream has no Timeout: /events never ends, so each request is bounded
// by its context instead.
var esphomeStream = &http.Client{}

// esphomePaths caches the discovered esphomePath per host and entity, so two
// printers on two relays of one board never share a path.
var esphomePaths sync.Map

type esphomePath struct {
	switchPath string
	powerPath  string // "" when the device has no watt sensor
}

// esphomeEntity is one entity seen on /events.
type esphomeEntity struct {
	domain   string // "switch" or "sensor"
	name     string // entity name (new form) or object_id (legacy)
	objectID string // legacy object_id, "" when the device sent only the new form
	path     string // REST path
	state    string // state text, e.g. "152.4 W"
}

// esphomeState is the JSON of a state event or of a REST GET.
type esphomeState struct {
	ID     string          `json:"id"`
	NameID string          `json:"name_id"`
	State  string          `json:"state"`
	Value  json.RawMessage `json:"value"`
}

// splitESPHomeID splits "switch/Power" (new form) or "switch-relay" (legacy)
// into its domain and name.
func splitESPHomeID(id string) (string, string) {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[:i], id[i+1:]
	}
	if i := strings.Index(id, "-"); i >= 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

// esphomeEntityOf builds an entity from a state event. name_id is used when
// present, else id.
func esphomeEntityOf(s esphomeState) (esphomeEntity, bool) {
	ident := s.ID
	if s.NameID != "" {
		ident = s.NameID
	}
	domain, name := splitESPHomeID(ident)
	if domain == "" || name == "" {
		return esphomeEntity{}, false
	}
	e := esphomeEntity{domain: domain, name: name, state: s.State}
	if strings.Contains(ident, "/") {
		e.path = "/" + domain + "/" + url.PathEscape(name)
	} else {
		e.path = "/" + domain + "/" + name
	}
	if !strings.Contains(s.ID, "/") {
		_, e.objectID = splitESPHomeID(s.ID)
	}
	return e, true
}

// esphomeMatch finds the entity whose name or legacy object_id equals want.
func esphomeMatch(list []esphomeEntity, want string) *esphomeEntity {
	for i := range list {
		e := &list[i]
		if strings.EqualFold(e.name, want) || (e.objectID != "" && strings.EqualFold(e.objectID, want)) {
			return e
		}
	}
	return nil
}

// esphomeKeyword returns the first entity whose name contains any of words.
func esphomeKeyword(list []esphomeEntity, words ...string) *esphomeEntity {
	for i := range list {
		name := strings.ToLower(list[i].name)
		for _, w := range words {
			if strings.Contains(name, w) {
				return &list[i]
			}
		}
	}
	return nil
}

func esphomeAuth(req *http.Request, g config.Plug) {
	if strings.TrimSpace(g.Username) != "" {
		req.SetBasicAuth(g.Username, g.Password)
	}
}

// esphomeFetch reads /events until 3 s pass, or 400 ms pass with no new state
// event after at least one arrived, and returns the entities seen.
func esphomeFetch(ctx context.Context, host string, g config.Plug) ([]esphomeEntity, error) {
	ctx, cancel := context.WithTimeout(ctx, esphomeDiscoverFor)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/events", nil)
	if err != nil {
		return nil, errNoAnswer
	}
	req.Header.Set("Accept", "text/event-stream")
	esphomeAuth(req, g)
	resp, err := esphomeStream.Do(req)
	if err != nil {
		return nil, errNoAnswer
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, errESPHomeLogin
	case http.StatusNotFound:
		return nil, errESPHomeNoWeb
	default:
		return nil, fmt.Errorf("The ESPHome web server answered with status %d.", resp.StatusCode)
	}

	var mu sync.Mutex
	var ents []esphomeEntity
	arrived := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var event, data string
		for sc.Scan() {
			line := strings.TrimSuffix(sc.Text(), "\r")
			switch {
			case line == "":
				if event == "state" {
					var s esphomeState
					if json.Unmarshal([]byte(data), &s) == nil {
						if e, ok := esphomeEntityOf(s); ok {
							mu.Lock()
							ents = append(ents, e)
							mu.Unlock()
							select {
							case arrived <- struct{}{}:
							default:
							}
						}
					}
				}
				event, data = "", ""
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()

	var quiet <-chan time.Time
loop:
	for {
		select {
		case <-arrived:
			quiet = time.After(esphomeQuietFor)
		case <-quiet:
			break loop
		case <-done:
			break loop
		case <-ctx.Done():
			break loop
		}
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return ents, nil
}

// esphomeDiscover picks the switch and the power sensor paths from /events.
func esphomeDiscover(ctx context.Context, host string, g config.Plug) (switchPath, powerPath string, err error) {
	ents, err := esphomeFetch(ctx, host, g)
	if err != nil {
		return "", "", err
	}
	var switches, watts []esphomeEntity
	for _, e := range ents {
		if e.domain == "switch" {
			switches = append(switches, e)
		} else if e.domain == "sensor" && strings.HasSuffix(e.state, " W") {
			watts = append(watts, e)
		}
	}
	var sw *esphomeEntity
	want := strings.TrimSpace(g.Entity)
	switch {
	case want != "":
		sw = esphomeMatch(switches, want)
		if sw == nil {
			return "", "", fmt.Errorf("The ESPHome device has no switch named %s.", g.Entity)
		}
	case len(switches) == 0:
		return "", "", errESPHomeNoSwitch
	case len(switches) == 1:
		sw = &switches[0]
	default:
		// Several switches: take an obvious plug name, never guess (it could be an LED).
		sw = esphomeKeyword(switches, "relay", "power", "plug", "socket", "outlet")
		if sw == nil {
			return "", "", errESPHomeManySwitches
		}
	}
	var pw *esphomeEntity
	if pn := strings.TrimSpace(g.PowerEntity); pn != "" {
		pw = esphomeMatch(watts, pn)
	} else {
		pw = esphomeKeyword(watts, "power")
		if pw == nil && len(watts) > 0 {
			pw = &watts[0]
		}
	}
	if pw != nil {
		powerPath = pw.path
	}
	return sw.path, powerPath, nil
}

// esphomeRediscover discovers the paths for host and caches them.
func esphomeRediscover(ctx context.Context, host string, g config.Plug) (esphomePath, error) {
	sw, pw, err := esphomeDiscover(ctx, host, g)
	if err != nil {
		return esphomePath{}, err
	}
	p := esphomePath{switchPath: sw, powerPath: pw}
	esphomePaths.Store(esphomeKey(host, g), p)
	return p, nil
}

// esphomeLocate returns the cached paths for host, or discovers them. The bool
// reports whether the paths came from the cache.
func esphomeLocate(ctx context.Context, host string, g config.Plug) (esphomePath, bool, error) {
	if v, ok := esphomePaths.Load(esphomeKey(host, g)); ok {
		return v.(esphomePath), true, nil
	}
	p, err := esphomeRediscover(ctx, host, g)
	return p, false, err
}

func esphomeGET(ctx context.Context, host string, g config.Plug, path string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		return 0, nil, errNoAnswer
	}
	esphomeAuth(req, g)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, errNoAnswer
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, body, nil
}

func esphomeStatusErr(status int) error {
	if status == http.StatusUnauthorized {
		return errESPHomeLogin
	}
	return fmt.Errorf("The ESPHome web server answered with status %d.", status)
}

func esphomeReadPath(ctx context.Context, host string, g config.Plug, p esphomePath) (Reading, error) {
	status, body, err := esphomeGET(ctx, host, g, p.switchPath)
	if err != nil {
		return Reading{}, err
	}
	if status != http.StatusOK {
		return Reading{}, esphomeStatusErr(status)
	}
	var s esphomeState
	var on bool
	if json.Unmarshal(body, &s) != nil || json.Unmarshal(s.Value, &on) != nil {
		return Reading{}, errESPHomeFormat
	}
	r := Reading{On: on}
	if p.powerPath != "" {
		if status, body, err := esphomeGET(ctx, host, g, p.powerPath); err == nil && status == http.StatusOK {
			var ps esphomeState
			var w float64
			if json.Unmarshal(body, &ps) == nil && json.Unmarshal(ps.Value, &w) == nil {
				r.Watts = &w
			}
		}
	}
	return r, nil
}

// esphomeRead reads the switch state and, when found, the power sensor.
// A failure with cached paths drops the cache and rediscovers once.
func esphomeRead(ctx context.Context, host string, g config.Plug) (Reading, error) {
	p, cached, err := esphomeLocate(ctx, host, g)
	if err != nil {
		return Reading{}, err
	}
	r, err := esphomeReadPath(ctx, host, g, p)
	if err == nil || !cached {
		return r, err
	}
	esphomePaths.Delete(esphomeKey(host, g))
	if p, err = esphomeRediscover(ctx, host, g); err != nil {
		return Reading{}, err
	}
	return esphomeReadPath(ctx, host, g, p)
}

func esphomePost(ctx context.Context, host string, g config.Plug, path string, on bool) error {
	verb := "/turn_off"
	if on {
		verb = "/turn_on"
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+path+verb, nil)
	if err != nil {
		return errNoAnswer
	}
	esphomeAuth(req, g)
	resp, err := httpClient.Do(req)
	if err != nil {
		return errNoAnswer
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errESPHomeLogin
	}
	return errESPHomeRefused
}

// esphomeSwitch turns the switch on or off. A failure with cached paths drops
// the cache and rediscovers once.
func esphomeSwitch(ctx context.Context, host string, g config.Plug, on bool) error {
	p, cached, err := esphomeLocate(ctx, host, g)
	if err != nil {
		return err
	}
	err = esphomePost(ctx, host, g, p.switchPath, on)
	if err == nil || !cached {
		return err
	}
	esphomePaths.Delete(esphomeKey(host, g))
	if p, err = esphomeRediscover(ctx, host, g); err != nil {
		return err
	}
	return esphomePost(ctx, host, g, p.switchPath, on)
}

func esphomeKey(host string, g config.Plug) string {
	return host + "\x00" + g.Entity + "\x00" + g.PowerEntity
}
