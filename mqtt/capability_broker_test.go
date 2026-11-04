package mqtt

import (
	"net"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// A broker that answers the login with "bad user name or password" (what a
// printer does for a wrong access code) becomes the access code sentence.
func TestConnectProblem_RealRefusedLogin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, err := packets.ReadPacket(conn); err != nil {
					return
				}
				ack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
				ack.ReturnCode = packets.ErrRefusedBadUsernameOrPassword
				_ = ack.Write(conn)
			}()
		}
	}()

	opts := mqtt.NewClientOptions().AddBroker("tcp://" + ln.Addr().String()).SetConnectTimeout(2 * time.Second).SetAutoReconnect(false).SetProtocolVersion(4)
	token := mqtt.NewClient(opts).Connect()
	if !token.WaitTimeout(5*time.Second) || token.Error() == nil {
		t.Fatal("a refused login did not fail the connect")
	}
	if got := connectProblem("127.0.0.1", token.Error()); !strings.Contains(got, "refused the access code") {
		t.Fatalf("refused login (%v) became %q", token.Error(), got)
	}
}
