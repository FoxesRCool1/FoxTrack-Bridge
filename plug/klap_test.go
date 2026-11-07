package plug

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// Golden values from python-kasa's AesTransport.hash_credentials and
// AesEncyptionSession (key 00..0f, IV 10..1f): the pre-KLAP Tapo login.
func TestTapoAESAgainstPythonKasa(t *testing.T) {
	user := "NzE5N2Y3NWI5MjEzODczYzQ2NzliZmNiOTgyYTAwOTQzZmMwZmZiMw=="
	v1 := tapoAESLoginParams("me@example.com", "S3cret!", false)
	v2 := tapoAESLoginParams("me@example.com", "S3cret!", true)
	if v1["username"] != user || v1["password"] != "UzNjcmV0IQ==" ||
		v2["username"] != user || v2["password2"] != "OWZjMmFjZDQ2YjMwNDY5NzMwYzAzOTFhOGUxNGMxMDlkMzQyNjYzOA==" {
		t.Fatalf("login params %v %v", v1, v2)
	}
	key, iv := make([]byte, 16), make([]byte, 16)
	for i := range key {
		key[i], iv[i] = byte(i), byte(16+i)
	}
	s := &tapoAESSession{key: key, iv: iv}
	const want = "AoTU/alSJog6QIUAiFg+poqOeIoym5tr6MEsEcHy66o="
	if got := s.encrypt([]byte(`{"method":"get_device_info"}`)); got != want {
		t.Fatalf("encrypt %s", got)
	}
	if plain, err := s.decrypt(want); err != nil || string(plain) != `{"method":"get_device_info"}` {
		t.Fatalf("decrypt %q %v", plain, err)
	}
}

// Golden vectors made with python-kasa (KlapTransport, KlapTransportV2 and
// KlapEncryptionSession): login me@example.com / S3cret!, local seed 00..0f,
// remote seed 10..1f, first request {"method":"get_device_info"}.
func TestKlapAgainstPythonKasa(t *testing.T) {
	local, remote := make([]byte, 16), make([]byte, 16)
	for i := range local {
		local[i], remote[i] = byte(i), byte(16+i)
	}
	hx := func(s string) []byte { b, _ := hex.DecodeString(s); return b }
	for _, c := range []struct {
		v                 int
		auth, h1, h2, enc string
		seq               int32
	}{
		{1, "b9ab87ac638dbc9db854d0850f13cb25",
			"50024993c46c0d2a2235f3f810c18c27b9b662d0ab7944a3d7c41258fcb784b1",
			"0ab2d9505d5559b1aa127abe7fae0a44f67514bf0779ff785a725557990b50ab",
			"3524af8c8e40a2c1fd66dbea75e125a989f7f180b311058be0071f6d7b6122650ade2e41df1c4eaee2619131bc8a4584b93e61879e4b835509140953c178e6ac",
			1195773214},
		{2, "1a3a343a9d597e99241d78d6a22e3a6d5de81ccb019a7101d2debd0fc8d6575c",
			"4ea22be79447019ea61eea598303bc56797a32d77fc7391f2e6bcbfd43de36df",
			"d9cc72f96444f0f4dd7382a1d862bdf0b3b9ddcb72980b0123344ebda8cc865a",
			"ec011d65c89967e705498453b59a20b395edeec74475e19e53f2fc59623ab1ae772d728e2b43fe8bcb70635a1e750fccc87a881fe46ce47a70e223563fc4dec8",
			1755866369},
	} {
		auth := klapAuthHash(c.v, "me@example.com", "S3cret!")
		if !bytes.Equal(auth, hx(c.auth)) {
			t.Fatalf("v%d auth %x", c.v, auth)
		}
		h1, h2 := klapSeedHashes(c.v, local, remote, auth)
		if !bytes.Equal(h1, hx(c.h1)) || !bytes.Equal(h2, hx(c.h2)) {
			t.Fatalf("v%d seed hashes %x %x", c.v, h1, h2)
		}
		s := newKlapSession("x", local, remote, auth, c.v == 2)
		enc, seq := s.encrypt([]byte(`{"method":"get_device_info"}`))
		if seq != c.seq || !bytes.Equal(enc, hx(c.enc)) {
			t.Fatalf("v%d request seq %d %x", c.v, seq, enc)
		}
		plain, err := s.decrypt(enc, seq)
		if err != nil || string(plain) != `{"method":"get_device_info"}` {
			t.Fatalf("v%d decrypt %q %v", c.v, plain, err)
		}
	}
}
