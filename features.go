package main

import (
	"bytes"
	"io/fs"
	"log"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
)

// Feature wiring. Each standalone feature lives in its own files: it registers
// its routes and its startup work from an init() here, and its dashboard
// section from web/features/<name>.js. Nothing else has to change to add one.

var (
	startupMu    sync.Mutex
	startupHooks []func()
)

// onStartup runs fn once the config is loaded and the printers are
// connecting, before the server starts listening. Call it from init().
func onStartup(fn func()) {
	startupMu.Lock()
	startupHooks = append(startupHooks, fn)
	startupMu.Unlock()
}

func runStartupHooks() {
	startupMu.Lock()
	hooks := append([]func(){}, startupHooks...)
	startupMu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

var (
	featuresOnce sync.Once
	featuresJS   []byte
)

// handleFeaturesJS serves every web/features/*.js in name order, each in its
// own block so one file's top-level names cannot clash with another's, and a
// file that throws cannot stop the next one.
func handleFeaturesJS(w http.ResponseWriter, r *http.Request) {
	featuresOnce.Do(func() {
		var names []string
		_ = fs.WalkDir(featuresFS, "web/features", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".js") {
				names = append(names, p)
			}
			return nil
		})
		sort.Strings(names)
		var b bytes.Buffer
		for _, n := range names {
			data, err := featuresFS.ReadFile(n)
			if err != nil {
				log.Printf("[features] %s: %v", n, err)
				continue
			}
			b.WriteString("// " + n + "\ntry {\n")
			b.Write(data)
			b.WriteString("\n} catch (e) { console.error('" + n + "', e); }\n")
		}
		featuresJS = b.Bytes()
	})
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(featuresJS)
}

// handleI18n serves web/i18n/<lang>.json, the dashboard's translations.
func handleI18n(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path) // strips any traversal
	data, err := i18nFS.ReadFile("web/i18n/" + name)
	if err != nil || !strings.HasSuffix(name, ".json") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func init() {
	http.HandleFunc("/features.js", handleFeaturesJS)
	http.HandleFunc("/i18n/", handleI18n)
}
