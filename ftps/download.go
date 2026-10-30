package ftps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ErrTooLarge means the remote file is bigger than the cap given to Download.
var ErrTooLarge = errors.New("ftps file too large")

// Download copies remoteName from the printer to localPath, over the same
// TLS-resumed data channel as an upload, and fails with ErrTooLarge past
// maxBytes. A data channel the printer closes without a TLS close_notify
// counts as complete only when the control channel then says 226 (as List).
// Cancelling ctx closes the connections. localPath is left behind on error;
// the caller removes it.
func Download(ctx context.Context, cfg Config, remoteName, localPath string, maxBytes int64) (err error) {
	cfg.defaults()
	var conns connSet
	stop := context.AfterFunc(ctx, conns.closeAll)
	defer stop()
	defer conns.closeAll()
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
	}()

	c, tlsCfg, err := openControl(ctx, cfg, &conns)
	if err != nil {
		return err
	}
	defer c.quit()

	data, err := openData(ctx, cfg, c, &conns, tlsCfg, "RETR "+remoteName)
	if err != nil {
		return err
	}
	f, err := os.Create(localPath)
	if err != nil {
		data.Close()
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	buf := make([]byte, 32<<10)
	var total int64
	var readErr error
	for {
		data.SetReadDeadline(time.Now().Add(cfg.StallTimeout))
		n, rerr := data.Read(buf)
		if n > 0 {
			if total += int64(n); total > maxBytes {
				data.Close()
				return ErrTooLarge
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				data.Close()
				return fmt.Errorf("write file: %w", werr)
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				readErr = rerr
			}
			break
		}
	}
	data.Close()
	truncated := errors.Is(readErr, io.ErrUnexpectedEOF)
	if readErr != nil && !truncated {
		return fmt.Errorf("read file: %w", readErr)
	}
	c.step = cfg.StepTimeout
	code, msg, rerr := c.reply()
	if truncated && (rerr != nil || (code != 226 && code != 250)) {
		return fmt.Errorf("read file: %w", readErr)
	}
	if rerr == nil && code != 226 && code != 250 {
		return fmt.Errorf("RETR ended with %d %s", code, msg)
	}
	return nil
}
