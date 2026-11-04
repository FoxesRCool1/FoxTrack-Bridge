// Package mqttexport publishes the Bridge's printer state to the user's own
// MQTT broker, with Home Assistant discovery so each printer shows up as a
// device. It sends nothing anywhere else. It knows nothing about the web
// server: the Bridge hands it a Host to read printers and run commands.
package mqttexport

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"

	"foxtrack-bridge/config"
)

const (
	DefaultPrefix          = "foxtrack_bridge"
	DefaultDiscoveryPrefix = "homeassistant"
)

// Resolve checks c and fills in its defaults: a clean broker address and both
// topic prefixes. A block that is off and has no broker is left as it is.
func Resolve(c config.MQTTExport) (config.MQTTExport, error) {
	c.Username = strings.TrimSpace(c.Username)
	if c.Enabled || strings.TrimSpace(c.Broker) != "" {
		b, err := normalizeBroker(c.Broker)
		if err != nil {
			return c, err
		}
		c.Broker = b
	}
	var err error
	if c.TopicPrefix, err = cleanPrefix(c.TopicPrefix, DefaultPrefix, "The topic prefix"); err != nil {
		return c, err
	}
	if c.DiscoveryPrefix, err = cleanPrefix(c.DiscoveryPrefix, DefaultDiscoveryPrefix, "The discovery prefix"); err != nil {
		return c, err
	}
	return c, nil
}

// normalizeBroker accepts "host", "host:port", tcp://, mqtt://, ssl://, tls://
// and mqtts:// and returns "tcp://host:port" or "ssl://host:port".
func normalizeBroker(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("Enter the broker address, for example tcp://192.168.1.10:1883.")
	}
	if !strings.Contains(s, "://") {
		s = "tcp://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("That broker address does not look right. Use tcp://host:1883 or ssl://host:8883.")
	}
	scheme, port := "tcp", "1883"
	switch strings.ToLower(u.Scheme) {
	case "tcp", "mqtt":
	case "ssl", "tls", "mqtts":
		scheme, port = "ssl", "8883"
	default:
		return "", errors.New("The broker address must start with tcp:// or ssl://.")
	}
	if u.Port() != "" {
		port = u.Port()
	}
	return scheme + "://" + net.JoinHostPort(u.Hostname(), port), nil
}

// cleanPrefix trims s to a usable topic prefix: no wildcards, no empty levels.
func cleanPrefix(s, def, what string) (string, error) {
	s = strings.Trim(strings.TrimSpace(s), "/")
	if s == "" {
		return def, nil
	}
	if len(s) > 100 || strings.ContainsAny(s, "+#\x00 ") || strings.Contains(s, "//") {
		return "", errors.New(what + " cannot hold spaces, + or #, or be empty between slashes.")
	}
	return s, nil
}

// ClientID is "foxtrack-bridge-" and a short hash of this machine's host name.
func ClientID() string {
	h, _ := os.Hostname()
	sum := sha256.Sum256([]byte(h))
	return "foxtrack-bridge-" + hex.EncodeToString(sum[:3])
}

func options(c config.MQTTExport, clientID string) *paho.ClientOptions {
	o := paho.NewClientOptions().AddBroker(c.Broker).SetClientID(clientID)
	if c.Username != "" {
		o.SetUsername(c.Username).SetPassword(c.Password)
	}
	// ssl:// verifies the broker's certificate unless the user asked to skip it
	// (a self-signed broker on their own network).
	o.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.SkipCertCheck})
	o.SetConnectTimeout(10 * time.Second).SetWriteTimeout(10 * time.Second).SetKeepAlive(30 * time.Second)
	return o
}

// Test connects once with c (already Resolved) and hangs up. The error is a
// plain sentence.
func Test(c config.MQTTExport) error {
	// A different client ID: the same one would knock the live connection off.
	cl := paho.NewClient(options(c, ClientID()+"-test").SetAutoReconnect(false).SetConnectRetry(false))
	tok := cl.Connect()
	if !tok.WaitTimeout(12 * time.Second) {
		return errors.New("The broker did not answer in time. Check the address and the port.")
	}
	if err := tok.Error(); err != nil {
		return Friendly(err)
	}
	cl.Disconnect(100)
	return nil
}

// Friendly turns a paho connect error into a sentence a person can act on.
func Friendly(err error) error {
	m := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, packets.ErrorRefusedBadUsernameOrPassword), errors.Is(err, packets.ErrorRefusedNotAuthorised):
		return errors.New("The broker refused the username or password.")
	case strings.Contains(m, "x509"), strings.Contains(m, "certificate"), strings.Contains(m, "tls:"):
		return errors.New("The broker's certificate could not be verified. For a self-signed broker, turn on skip certificate check.")
	case strings.Contains(m, "connection refused"):
		return errors.New("Nothing is listening at that address and port. Check the broker address.")
	case strings.Contains(m, "no such host"):
		return errors.New("The broker's address could not be found.")
	case strings.Contains(m, "timeout"), strings.Contains(m, "deadline"), strings.Contains(m, "timed out"):
		return errors.New("The broker did not answer in time. Check the address and the port.")
	case strings.Contains(m, "eof"), strings.Contains(m, "protocol"):
		return errors.New("The broker closed the connection. Check that the port is for MQTT and that tcp:// or ssl:// matches it.")
	}
	return errors.New("Could not connect to the broker: " + err.Error())
}
