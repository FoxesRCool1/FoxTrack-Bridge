package plug

import (
	"encoding/json"
	"testing"
)

// Golden values from meross_lan's compute_message_signature and
// HttpClient.Cipher (the AES used by models with Appliance.Encrypt.ECDHE).
func TestMerossAgainstMerossLan(t *testing.T) {
	if got := merossSign("0123456789abcdef0123456789abcdef", "pippo", 1700000000); got != "6e5df93a7dfdaaf561ad132464426e45" {
		t.Fatalf("sign %s", got)
	}
	ck := merossCipherKey("2008179288373890809548e1e9123456", "0123456789abcdef0123456789abcdef", "48:e1:e9:12:34:56")
	const want = "w+CD8babzz+ec+1QRNkKmflByXMFXG63pWg7j6yh+hY="
	if got := string(merossEncrypt(ck, []byte(`{"header":{"method":"GET"}}`))); got != want {
		t.Fatalf("encrypt %s", got)
	}
	if plain, err := merossDecrypt(ck, []byte(want+"\n")); err != nil || string(plain) != `{"header":{"method":"GET"}}` {
		t.Fatalf("decrypt %q %v", plain, err)
	}
}

// System.All carries channel 0's state in one of three places.
func TestParseMerossOnOff(t *testing.T) {
	for body, want := range map[string]bool{
		`{"all":{"digest":{"togglex":[{"channel":0,"onoff":1}]}}}`: true,
		`{"all":{"digest":{"togglex":{"channel":0,"onoff":0}}}}`:   false,
		`{"all":{"digest":{"toggle":{"onoff":1}}}}`:                true,
		`{"all":{"control":{"toggle":{"onoff":1}}}}`:               true,
	} {
		on, ok := parseMerossOnOff(json.RawMessage(body))
		if !ok || on != want {
			t.Fatalf("%s: on=%v ok=%v", body, on, ok)
		}
	}
	if _, ok := parseMerossOnOff(json.RawMessage(`{"all":{}}`)); ok {
		t.Fatal("no state should not parse")
	}
}
