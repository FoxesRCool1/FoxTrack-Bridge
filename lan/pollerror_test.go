package lan

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A failed poll reaches the card as a plain sentence, never the Go error.
func TestPollErrorText(t *testing.T) {
	client := &http.Client{Timeout: 150 * time.Millisecond}

	// A server that never answers: the client times out.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(400 * time.Millisecond) }))
	defer slow.Close()
	_, timeoutErr := sendJSONRequest(client, "GET", slow.URL+"/printer/objects/query", nil, nil)

	// A closed port: refused at once.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	closed := "http://" + ln.Addr().String()
	ln.Close()
	_, refusedErr := sendJSONRequest(client, "GET", closed+"/printer/objects/query", nil, nil)

	// An address that answers with a web page, not JSON.
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer page.Close()
	_, pageErr := sendJSONRequest(client, "GET", page.URL, nil, nil)

	host := strings.TrimPrefix(slow.URL, "http://")
	host = host[:strings.LastIndex(host, ":")]

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"timeout", timeoutErr, "Can't reach " + host + "."},
		{"refused", refusedErr, "Can't reach 127.0.0.1."},
		{"web page", pageErr, "not like the printer's software"},
		{"bad key", errors.New("HTTP 401"), errMoonKey.Error()},
		{"forbidden", errors.New("HTTP 403"), errMoonKey.Error()},
		{"not found", errors.New("HTTP 404"), "HTTP 404"},
		{"server error", errors.New("HTTP 503"), "HTTP 503"},
		{"already a sentence", errors.New(`PrusaLink reports the printer as "OFFLINE"`), `PrusaLink reports the printer as "OFFLINE"`},
	}
	for _, c := range cases {
		if c.err == nil {
			t.Errorf("%s: the request did not fail", c.name)
			continue
		}
		got := pollErrorText(c.err)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q does not contain %q (raw: %v)", c.name, got, c.want, c.err)
		}
		if strings.Contains(got, "dial tcp") || strings.Contains(got, "Client.Timeout") || strings.Contains(got, "http://") {
			t.Errorf("%s: raw Go error in %q", c.name, got)
		}
	}
}
