package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeJWT(t *testing.T, claims map[string]interface{}) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

// newTestClient points a client at h and freezes the clock at base so the
// pacing rules can be checked without sleeping.
func newTestClient(t *testing.T, h http.Handler, base time.Time) (*Client, *time.Time) {
	t.Helper()
	ResetPacing()
	clock := base
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now; ResetPacing() })
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(RegionGlobal)
	c.BaseURL = srv.URL
	return c, &clock
}

func decodeBody(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m
}

var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func TestLogin_SecondStepSendsEmailCode(t *testing.T) {
	var sent int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/user-service/user/login", func(w http.ResponseWriter, r *http.Request) {
		b := decodeBody(t, r)
		if b["account"] != "a@b.c" || b["password"] != "pw" || r.Header.Get("User-Agent") == "" {
			t.Errorf("unexpected login body/headers: %v", b)
		}
		w.Write([]byte(`{"loginType":"verifyCode"}`))
	})
	mux.HandleFunc("/v1/user-service/user/sendemail/code", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&sent, 1)
		b := decodeBody(t, r)
		if b["email"] != "a@b.c" || b["type"] != "codeLogin" {
			t.Errorf("unexpected send-code body: %v", b)
		}
		w.Write([]byte(`{"message":"success"}`))
	})
	c, _ := newTestClient(t, mux, base)
	if _, err := c.Login(context.Background(), "a@b.c", "pw"); !errors.Is(err, ErrCodeRequired) {
		t.Fatalf("err = %v, want ErrCodeRequired", err)
	}
	if sent != 1 {
		t.Errorf("send-code calls = %d, want 1", sent)
	}
}

func TestLogin_PacedToOncePerMinute(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/user-service/user/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"loginType":"tfa","tfaKey":"k"}`)) // TOTP accounts fall back to the emailed code too
	})
	mux.HandleFunc("/v1/user-service/user/sendemail/code", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	c, clock := newTestClient(t, mux, base)
	ctx := context.Background()
	if _, err := c.Login(ctx, "a@b.c", "pw"); !errors.Is(err, ErrCodeRequired) {
		t.Fatalf("first: err = %v", err)
	}
	if _, err := c.Login(ctx, "a@b.c", "pw"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second within a minute: err = %v, want ErrRateLimited", err)
	}
	if calls != 1 {
		t.Fatalf("login calls = %d, want 1 (the second must not reach Bambu)", calls)
	}
	*clock = clock.Add(61 * time.Second)
	if _, err := c.Login(ctx, "a@b.c", "pw"); !errors.Is(err, ErrCodeRequired) {
		t.Fatalf("after a minute: err = %v", err)
	}
	if calls != 2 {
		t.Errorf("login calls = %d, want 2", calls)
	}
}

func TestLoginWithCode_JWTGivesUsernameAndExpiry(t *testing.T) {
	tok := fakeJWT(t, map[string]interface{}{"username": "u_123", "exp": float64(1893456000)})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/user-service/user/login", func(w http.ResponseWriter, r *http.Request) {
		b := decodeBody(t, r)
		if b["code"] != "123456" || b["account"] != "a@b.c" {
			t.Errorf("unexpected code body: %v", b)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"accessToken": tok, "expiresIn": 7776000})
	})
	mux.HandleFunc("/v1/design-user-service/my/preference", func(w http.ResponseWriter, r *http.Request) {
		t.Error("preference must not be called when the JWT carries the username")
	})
	c, _ := newTestClient(t, mux, base)
	sess, err := c.LoginWithCode(context.Background(), "a@b.c", "123456")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Username != "u_123" || sess.Token != tok || sess.ExpiresAt != 1893456000 || sess.Region != RegionGlobal || sess.Email != "a@b.c" {
		t.Errorf("session = %+v", sess)
	}
	if sess.IssuedAt != base.Unix() {
		t.Errorf("issued = %d, want %d", sess.IssuedAt, base.Unix())
	}
}

func TestLoginWithCode_WrongAndExpiredCodes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/user-service/user/login", func(w http.ResponseWriter, r *http.Request) {
		b := decodeBody(t, r)
		w.WriteHeader(http.StatusBadRequest)
		if b["code"] == "111111" {
			w.Write([]byte(`{"code":2,"error":"wrong"}`))
		} else {
			w.Write([]byte(`{"code":1,"error":"expired"}`))
		}
	})
	c, clock := newTestClient(t, mux, base)
	if _, err := c.LoginWithCode(context.Background(), "a@b.c", "111111"); !errors.Is(err, ErrWrongCode) {
		t.Errorf("err = %v, want ErrWrongCode", err)
	}
	*clock = clock.Add(11 * time.Second)
	if _, err := c.LoginWithCode(context.Background(), "a@b.c", "222222"); !errors.Is(err, ErrCodeExpired) {
		t.Errorf("err = %v, want ErrCodeExpired", err)
	}
}

func TestSessionFromToken_OpaqueTokenUsesPreference(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/design-user-service/my/preference", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer opaque-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"uid":42,"name":"x"}`))
	})
	c, _ := newTestClient(t, mux, base)
	sess, err := c.SessionFromToken(context.Background(), "a@b.c", " Bearer opaque-token ")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Username != "u_42" || sess.Token != "opaque-token" {
		t.Errorf("session = %+v", sess)
	}
	if want := base.Add(tokenLifetime).Unix(); sess.ExpiresAt != want {
		t.Errorf("expires = %d, want %d (issued + 89 days)", sess.ExpiresAt, want)
	}
}

func TestDevices_CachedForAMinute(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/iot-service/api/user/bind", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"message":"success","devices":[{"dev_id":"01P00A1","name":"Studio P1S","online":true,"print_status":"IDLE","dev_model_name":"N2S","dev_product_name":"P1S","dev_access_code":"12345678"}]}`))
	})
	c, clock := newTestClient(t, mux, base)
	ctx := context.Background()
	devs, err := c.Devices(ctx, "tok", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].Serial != "01P00A1" || devs[0].Model != "P1S" || !devs[0].Online || devs[0].AccessCode != "12345678" {
		t.Fatalf("devices = %+v", devs)
	}
	if b, _ := json.Marshal(devs[0]); strings.Contains(string(b), "12345678") {
		t.Errorf("access code leaked into JSON: %s", b)
	}
	if _, err := c.Devices(ctx, "tok", true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("bind calls = %d, want 1 (a forced refresh inside a minute is still cached)", calls)
	}
	*clock = clock.Add(61 * time.Second)
	if _, err := c.Devices(ctx, "tok", false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("bind calls = %d, want 1 (no refresh asked)", calls)
	}
	if _, err := c.Devices(ctx, "tok", true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("bind calls = %d, want 2", calls)
	}
}

func TestCloudflareBlockHoldsEveryCall(t *testing.T) {
	var bindCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/user-service/user/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`<html><title>Attention Required! | Cloudflare</title></html>`))
	})
	mux.HandleFunc("/v1/iot-service/api/user/bind", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bindCalls, 1)
		w.Write([]byte(`{"devices":[]}`))
	})
	c, clock := newTestClient(t, mux, base)
	ctx := context.Background()
	if _, err := c.Login(ctx, "a@b.c", "pw"); !errors.Is(err, ErrCloudflare) {
		t.Fatalf("err = %v, want ErrCloudflare", err)
	}
	if HoldUntil().IsZero() {
		t.Fatal("a Cloudflare block must start a hold")
	}
	if _, err := c.Devices(ctx, "tok", true); !errors.Is(err, ErrCloudflare) {
		t.Fatalf("devices during hold: err = %v, want ErrCloudflare", err)
	}
	if bindCalls != 0 {
		t.Fatalf("bind calls during hold = %d, want 0", bindCalls)
	}
	*clock = clock.Add(cloudflareHold + time.Second)
	if _, err := c.Devices(ctx, "tok", true); err != nil {
		t.Fatalf("after hold: %v", err)
	}
	if bindCalls != 1 {
		t.Errorf("bind calls after hold = %d, want 1", bindCalls)
	}
}

func TestTokenExpiry(t *testing.T) {
	jwt := fakeJWT(t, map[string]interface{}{"exp": float64(1800000000)})
	cases := []struct {
		name      string
		token     string
		expiresIn int64
		want      int64
	}{
		{"jwt exp wins", jwt, 10, 1800000000},
		{"expiresIn when opaque", "opaque", 7776000, base.Add(7776000 * time.Second).Unix()},
		{"fallback 89 days", "opaque", 0, base.Add(tokenLifetime).Unix()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TokenExpiry(tc.token, base, tc.expiresIn); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBroker(t *testing.T) {
	if Broker("cn") != "ssl://cn.mqtt.bambulab.com:8883" || Broker("") != "ssl://us.mqtt.bambulab.com:8883" || Broker("global") != "ssl://us.mqtt.bambulab.com:8883" {
		t.Error("unexpected broker mapping")
	}
}
