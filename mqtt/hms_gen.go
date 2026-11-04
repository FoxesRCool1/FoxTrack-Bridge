//go:build ignore

// hms_gen builds mqtt/hms_en.json, the English text of Bambu's HMS and print
// error codes. Run it from the repository root to refresh the table:
//
//	go run mqtt/hms_gen.go
//
// Source 1 is Bambu's own table (the one Bambu Studio reads). It only knows
// the codes for the models Bambu serves by default, so source 2, the table
// that ha-bambulab (MIT) ships, fills the codes source 1 lacks (more AMS
// units, newer models). Where both have a code, Bambu's text wins.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	bambuURL = "https://e.bambulab.com/query.php?lang=en"
	haURL    = "https://raw.githubusercontent.com/greghesp/ha-bambulab/main/custom_components/bambu_lab/pybambu/hms_error_text/hms_en.json.gz"
	outPath  = "mqtt/hms_en.json"
)

func get(url string) []byte {
	c := &http.Client{Timeout: 60 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Fatalf("%s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		log.Fatal(err)
	}
	return b
}

// tidy trims a text and collapses its runs of white space to one space.
func tidy(s string) string { return strings.Join(strings.Fields(s), " ") }

func main() {
	var bambu struct {
		Data map[string]struct {
			En []struct {
				Ecode string `json:"ecode"`
				Intro string `json:"intro"`
			} `json:"en"`
		} `json:"data"`
	}
	if err := json.Unmarshal(get(bambuURL), &bambu); err != nil {
		log.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(get(haURL)))
	if err != nil {
		log.Fatal(err)
	}
	var ha map[string]map[string]map[string][]string // table -> code -> text -> models
	if err := json.NewDecoder(zr).Decode(&ha); err != nil {
		log.Fatal(err)
	}

	out := map[string]map[string]string{}
	for key, name := range map[string]string{"device_hms": "hms", "device_error": "err"} {
		m := map[string]string{}
		for _, e := range bambu.Data[key].En {
			if t := tidy(e.Intro); t != "" {
				m[strings.ToUpper(strings.TrimSpace(e.Ecode))] = t
			}
		}
		fromBambu := len(m)
		for code, texts := range ha[key] {
			code = strings.ToUpper(code)
			if _, ok := m[code]; ok {
				continue
			}
			// The text for every model first, else the first model-specific one.
			var all, byModel []string
			for t, models := range texts {
				if len(models) == 0 {
					all = append(all, t)
				} else {
					byModel = append(byModel, t)
				}
			}
			sort.Strings(all)
			sort.Strings(byModel)
			if c := append(all, byModel...); len(c) > 0 && tidy(c[0]) != "" {
				m[code] = tidy(c[0])
			}
		}
		fmt.Printf("%s: %d from Bambu, %d in all\n", name, fromBambu, len(m))
		out[name] = m
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // map keys come out sorted
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", outPath, buf.Len())
}
