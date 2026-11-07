package plug

import (
	"bytes"
	"encoding/hex"
	"net"
	"testing"
)

// Golden vectors made with tinytuya 1.x (pack_message and AESCipher), local key
// "0123456789abcdef", client nonce 00..0f, device nonce 10..1f, and device
// replies carrying {"dps":{"1":true,"19":1234}}.
func TestTuyaAgainstTinytuya(t *testing.T) {
	local := []byte("0123456789abcdef")
	cn, rn := make([]byte, 16), make([]byte, 16)
	for i := range cn {
		cn[i], rn[i] = byte(i), byte(16+i)
	}
	k34, _ := hex.DecodeString("377222e061a924c591cd9c27ea163ed4")
	k35, _ := hex.DecodeString("ed2fd40d0e642b26f836170ad08ba03d")
	if got := tuyaSessionKey("3.4", local, cn, rn); !bytes.Equal(got, k34) {
		t.Fatalf("3.4 session key %x", got)
	}
	if got := tuyaSessionKey("3.5", local, cn, rn); !bytes.Equal(got, k35) {
		t.Fatalf("3.5 session key %x", got)
	}

	for _, c := range []struct {
		name, ver, frame string
		key              []byte
	}{
		{"3.3 query reply", "3.3", "000055aa000000070000000a0000002c00000000c27ee03f8be481e63320de7dc6eb42968a3027832332edf33f8e751dd3fdc213bf405fe60000aa55", local},
		{"3.3 status with header", "3.3", "000055aa00000008000000080000003b00000000332e33000000000000000000000000c27ee03f8be481e63320de7dc6eb42968a3027832332edf33f8e751dd3fdc213858925760000aa55", local},
		{"3.4 status", "3.4", "000055aa000000090000000800000058000000001e5754a8e86f54c7fb3b8d7c147a28cd9b4637edd1308d6a3c648dbfe1635626ba7f716541ecba3b55307e369dacd83d5487a3b0af6c1b0f459e70a3bd18af155e77a134cda87c90033dbd094f949c490000aa55", k34},
		{"3.5 query reply", "3.5", "0000669900000000000a000000100000003c6162636465666768696a6b6cca2be9e34dada751f350468879c678e74cacac2ffc0ed958b3098f22b740a9cc4452ba21a06d474e870a7389d8d3703100009966", k35},
	} {
		frame, _ := hex.DecodeString(c.frame)
		dev, bridge := net.Pipe()
		go func() { dev.Write(frame); dev.Close() }()
		dps, err := (&tuyaConn{c: bridge, ver: c.ver, key: c.key}).awaitDPS()
		bridge.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		r, err := tuyaReading(dps)
		if err != nil || !r.On || r.Watts == nil || *r.Watts != 123.4 {
			t.Fatalf("%s: %+v %v", c.name, r, err)
		}
	}
}

func TestIsControlAck(t *testing.T) {
	if !isControlAck(tuyaControl) || !isControlAck(tuyaControlNew) {
		t.Fatal("control frames must be acks")
	}
	if isControlAck(tuyaStatus) {
		t.Fatal("status push must not be taken as ack")
	}
}
