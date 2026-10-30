package ftps

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Entry is one file in a printer directory.
type Entry struct {
	Name     string
	Size     int64
	Modified time.Time
}

// List returns the files (not directories) in dir on the printer, read with
// LIST over the same TLS-resumed data channel as an upload. The printers run
// vsftpd, whose LIST output is Unix "ls -l"; the lines are parsed by
// ParseListLine. Cancelling ctx closes the connections.
func List(ctx context.Context, cfg Config, dir string) (entries []Entry, err error) {
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
		return nil, err
	}
	defer c.quit()

	data, err := openData(ctx, cfg, c, &conns, tlsCfg, "LIST "+dir)
	if err != nil {
		return nil, err
	}
	data.SetReadDeadline(time.Now().Add(cfg.StallTimeout))
	raw, readErr := io.ReadAll(io.LimitReader(data, 4<<20))
	data.Close()
	// Bambu closes the data channel without a TLS close_notify (see ftps.go):
	// that reads as io.ErrUnexpectedEOF. The listing is whole when the control
	// channel then confirms it with 226, so only then is it accepted.
	truncated := errors.Is(readErr, io.ErrUnexpectedEOF)
	if readErr != nil && !truncated {
		return nil, fmt.Errorf("read listing: %w", readErr)
	}
	// The 226 after the listing; a slow one is not an error, the data is here.
	c.step = cfg.StepTimeout
	code, msg, rerr := c.reply()
	if truncated && (rerr != nil || (code != 226 && code != 250)) {
		return nil, fmt.Errorf("read listing: %w", readErr)
	}
	if rerr == nil && code != 226 && code != 250 {
		cfg.Logf("LIST %s ended with %d %q", dir, code, msg)
	}

	now := time.Now()
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		if e, ok := ParseListLine(sc.Text(), now); ok {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

var listMonths = map[string]time.Month{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

// ParseListLine reads one "ls -l" line as vsftpd writes it:
//
//	-rw-rw-rw-    1 0        0        12345678 Oct 29 10:21 my part.gcode.3mf
//	-rw-rw-rw-    1 0        0        12345678 Oct 29  2025 old.3mf
//
// Directories, links and lines it cannot read return false. A time without a
// year is this year, or last year when that month is still ahead of now. The
// zone is UTC (the printer's clock is not known).
func ParseListLine(line string, now time.Time) (Entry, bool) {
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 10 || line[0] != '-' {
		return Entry{}, false
	}
	// Walk eight whitespace-separated fields; the name is everything after
	// the single space that follows the eighth, spaces and all.
	rest := line
	var fields []string
	for i := 0; i < 8; i++ {
		rest = strings.TrimLeft(rest, " \t")
		j := strings.IndexAny(rest, " \t")
		if j < 0 {
			return Entry{}, false
		}
		fields = append(fields, rest[:j])
		rest = rest[j:]
	}
	name := strings.TrimPrefix(rest, " ")
	if name == "" || strings.TrimSpace(name) == "" {
		return Entry{}, false
	}
	size, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil {
		return Entry{}, false
	}
	month, ok := listMonths[strings.ToLower(fields[5])]
	if !ok {
		return Entry{}, false
	}
	day, err := strconv.Atoi(fields[6])
	if err != nil || day < 1 || day > 31 {
		return Entry{}, false
	}
	year, hour, minute := now.Year(), 0, 0
	if hh, mm, found := strings.Cut(fields[7], ":"); found {
		hour, _ = strconv.Atoi(hh)
		minute, _ = strconv.Atoi(mm)
		if month > now.Month() || (month == now.Month() && day > now.Day()) {
			year--
		}
	} else if y, err := strconv.Atoi(fields[7]); err == nil {
		year = y
	} else {
		return Entry{}, false
	}
	return Entry{
		Name:     name,
		Size:     size,
		Modified: time.Date(year, month, day, hour, minute, 0, 0, time.UTC),
	}, true
}
