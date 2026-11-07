package plug

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A secured plug gets the token in a "Token" header; without it, a clear
// message instead of "not a myStrom plug".
func TestMyStromToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Token") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"power":5.5,"relay":true}`))
	}))
	defer srv.Close()
	host, _ := hostOf(srv.URL)
	if r, err := mystromRead(context.Background(), host, "secret"); err != nil || !r.On || *r.Watts != 5.5 {
		t.Fatalf("with token: %+v %v", r, err)
	}
	if _, err := mystromRead(context.Background(), host, ""); err != errMyStromToken {
		t.Fatalf("without token: %v", err)
	}
}
