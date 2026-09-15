package capture

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"time"
)

// BambuFrame connects to the printer's camera port (6000), authenticates using the
// proprietary binary protocol, reads the first valid JPEG frame, and closes.
// The auth format mirrors bambuCameraStream in server.go.
func BambuFrame(ip, lanCode, printerName string) ([]byte, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second},
		"tcp",
		net.JoinHostPort(ip, "6000"),
		&tls.Config{InsecureSkipVerify: true},
	)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	auth := make([]byte, 80)
	binary.LittleEndian.PutUint32(auth[0:4], 0x40)
	binary.LittleEndian.PutUint32(auth[4:8], 0x3000)
	copy(auth[16:48], []byte("bblp"))
	copy(auth[48:80], []byte(lanCode))

	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(auth); err != nil {
		return nil, fmt.Errorf("auth write: %w", err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	reader := bufio.NewReader(conn)
	hdr := make([]byte, 16)
	for {
		if _, err := io.ReadFull(reader, hdr); err != nil {
			return nil, fmt.Errorf("header read: %w", err)
		}
		frameSize := binary.LittleEndian.Uint32(hdr[0:4])
		if frameSize == 0 || frameSize > 10<<20 {
			return nil, fmt.Errorf("invalid frame size %d", frameSize)
		}
		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(reader, frame); err != nil {
			return nil, fmt.Errorf("frame read: %w", err)
		}
		if frameSize < 2 || frame[0] != 0xFF || frame[1] != 0xD8 {
			continue // skip non-JPEG metadata frames
		}
		return frame, nil
	}
}

// snapshotTransport is shared by every webcam fetch. A per-call transport owns a
// connection pool nothing ever closes, so each snapshot left an idle socket open
// for the life of the process. See lan.sharedInsecureTransport.
var snapshotTransport = &http.Transport{
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 4,
	IdleConnTimeout:     90 * time.Second,
}

// KlipperFrame fetches a single JPEG snapshot from the given webcam URL.
// Handles both plain JPEG responses and MJPEG multipart streams (returns the first frame).
func KlipperFrame(webcamURL string) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: snapshotTransport}
	resp, err := client.Get(webcamURL)
	if err != nil {
		return nil, fmt.Errorf("GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("webcam returned HTTP %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "multipart") {
		_, params, err := mime.ParseMediaType(ct)
		if err != nil {
			return nil, fmt.Errorf("parse content-type: %w", err)
		}
		mr := multipart.NewReader(resp.Body, params["boundary"])
		part, err := mr.NextPart()
		if err != nil {
			return nil, fmt.Errorf("read multipart frame: %w", err)
		}
		defer part.Close()
		return io.ReadAll(io.LimitReader(part, 10<<20))
	}

	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
}
