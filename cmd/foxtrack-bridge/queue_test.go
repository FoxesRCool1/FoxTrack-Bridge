//go:build headless

package main

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"foxtrack-bridge/config"
)

func withQueue(t *testing.T) {
	t.Helper()
	configMutex.Lock()
	saved := configStore
	configStore = &config.Config{Printers: []config.Printer{{ID: "p1", Name: "Monsieur"}, {ID: "p2", Name: "Sherlock"}}}
	configMutex.Unlock()
	printQueue.Lock()
	printQueue.loaded = false
	printQueue.Unlock()
	os.Remove(queuePath())
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = saved
		configMutex.Unlock()
		printQueue.Lock()
		printQueue.loaded = false
		printQueue.Unlock()
		os.Remove(queuePath())
	})
}

func queueCall(t *testing.T, method, path, body string) (int, map[string][]queueItem) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handleQueue(rec, req)
	var out map[string][]queueItem
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func names(items []queueItem) string {
	var s []string
	for _, it := range items {
		s = append(s, it.FileName)
	}
	return strings.Join(s, ",")
}

func TestQueue_AddMoveRemovePersist(t *testing.T) {
	withQueue(t)
	for _, f := range []string{"a.3mf", "b.3mf", "c.3mf"} {
		if code, _ := queueCall(t, "POST", "/api/queue/p1", `{"file_name":"`+f+`"}`); code != 200 {
			t.Fatalf("add %s: %d", f, code)
		}
	}
	if code, _ := queueCall(t, "POST", "/api/queue/p1", `{"file_name":"  "}`); code != 400 {
		t.Fatalf("blank name: %d", code)
	}
	if code, _ := queueCall(t, "POST", "/api/queue/zz", `{"file_name":"x"}`); code != 404 {
		t.Fatalf("unknown printer: %d", code)
	}
	_, q := queueCall(t, "GET", "/api/queue", "")
	if names(q["p1"]) != "a.3mf,b.3mf,c.3mf" {
		t.Fatalf("queue = %s", names(q["p1"]))
	}
	c := q["p1"][2].ID
	_, q = queueCall(t, "POST", "/api/queue/p1/move", `{"item_id":"`+c+`","to":0}`)
	if names(q["p1"]) != "c.3mf,a.3mf,b.3mf" {
		t.Fatalf("after move = %s", names(q["p1"]))
	}
	_, q = queueCall(t, "POST", "/api/queue/p1/move", `{"item_id":"`+c+`","to":99}`)
	if names(q["p1"]) != "a.3mf,b.3mf,c.3mf" {
		t.Fatalf("move past end = %s", names(q["p1"]))
	}
	_, q = queueCall(t, "DELETE", "/api/queue/p1/"+q["p1"][1].ID, "")
	if names(q["p1"]) != "a.3mf,c.3mf" {
		t.Fatalf("after delete = %s", names(q["p1"]))
	}

	// Survives a restart (a fresh load from queue.json).
	printQueue.Lock()
	printQueue.loaded = false
	printQueue.Unlock()
	_, q = queueCall(t, "GET", "/api/queue", "")
	if names(q["p1"]) != "a.3mf,c.3mf" {
		t.Fatalf("after reload = %s", names(q["p1"]))
	}

	// A removed printer's queue is not shown.
	configMutex.Lock()
	configStore.Printers = configStore.Printers[1:]
	configMutex.Unlock()
	if _, q = queueCall(t, "GET", "/api/queue", ""); len(q["p1"]) != 0 {
		t.Fatalf("removed printer's queue still listed")
	}
}

func TestQueue_UnreadableFileIsNeverOverwritten(t *testing.T) {
	withQueue(t)
	// A directory where queue.json should be: reading fails, but not as "missing".
	if err := os.Mkdir(queuePath(), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(queuePath()) })
	if code, _ := queueCall(t, "POST", "/api/queue/p1", `{"file_name":"a.3mf"}`); code == 200 {
		t.Fatal("add saved over an unreadable queue.json")
	}
	if fi, err := os.Stat(queuePath()); err != nil || !fi.IsDir() {
		t.Fatalf("queue.json was replaced: %v", err)
	}
}

func TestQueue_StartNext(t *testing.T) {
	withQueue(t)
	queueCall(t, "POST", "/api/queue/p1", `{"file_name":"a.3mf"}`)
	queueCall(t, "POST", "/api/queue/p1", `{"file_name":"b.3mf"}`)

	var started []string
	ok := func(string) bool { return true }
	start := func(name, file string) error { started = append(started, name+":"+file); return nil }

	if _, err := queueStartNext("p1", func(string) bool { return false }, start); !errors.Is(err, errQueueNotReady) {
		t.Fatalf("busy printer: err = %v", err)
	}
	if _, err := queueStartNext("p1", ok, func(string, string) error { return errors.New("refused") }); err == nil {
		t.Fatal("a refused start must be an error")
	}
	if q := queueSnapshot(); names(q["p1"]) != "a.3mf,b.3mf" {
		t.Fatalf("a failed start must keep the file queued: %s", names(q["p1"]))
	}
	it, err := queueStartNext("p1", ok, start)
	if err != nil || it.FileName != "a.3mf" || started[0] != "Monsieur:a.3mf" {
		t.Fatalf("start: %v %+v %v", err, it, started)
	}
	if q := queueSnapshot(); names(q["p1"]) != "b.3mf" {
		t.Fatalf("after start = %s", names(q["p1"]))
	}
	queueStartNext("p1", ok, start)
	if _, err := queueStartNext("p1", ok, start); !errors.Is(err, errQueueEmpty) {
		t.Fatalf("empty: err = %v", err)
	}

	// A second start while one is in flight is refused.
	queueCall(t, "POST", "/api/queue/p2", `{"file_name":"x.3mf"}`)
	var inner error
	queueStartNext("p2", ok, func(string, string) error {
		_, inner = queueStartNext("p2", ok, start)
		return nil
	})
	if !errors.Is(inner, errQueueBusy) {
		t.Fatalf("concurrent start: err = %v", inner)
	}
}

func TestQueue_NullFileLoadsEmpty(t *testing.T) {
	withQueue(t)
	os.MkdirAll(config.ConfigDir(), 0700)
	os.WriteFile(queuePath(), []byte("null"), 0600)
	code, q := queueCall(t, "POST", "/api/queue/p1", `{"file_name":"a.3mf"}`)
	if code != 200 {
		t.Fatalf("add to a null queue.json: %d", code)
	}
	if names(q["p1"]) != "a.3mf" {
		t.Fatalf("queue = %s", names(q["p1"]))
	}
}

func readSavedQueues(t *testing.T) (string, map[string][]queueItem) {
	t.Helper()
	data, err := os.ReadFile(queuePath())
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string][]queueItem
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	return string(data), saved
}

func TestQueue_SaveDropsRemovedPrinters(t *testing.T) {
	withQueue(t)
	configMutex.Lock()
	configStore.Printers = []config.Printer{{ID: "p1", Name: "Monsieur"}}
	configMutex.Unlock()
	printQueue.Lock()
	printQueue.loaded = true
	printQueue.byPrinter = map[string][]queueItem{
		"p1":   {{ID: "1", FileName: "a.3mf"}},
		"gone": {{ID: "2", FileName: "b.3mf"}},
	}
	err := queueSaveLocked()
	printQueue.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	raw, saved := readSavedQueues(t)
	if len(saved) != 1 || names(saved["p1"]) != "a.3mf" {
		t.Fatalf("saved queues = %s", raw)
	}
}

func TestQueue_SaveKeepsAllWithoutPrinters(t *testing.T) {
	withQueue(t)
	configMutex.Lock()
	configStore.Printers = nil
	configMutex.Unlock()
	printQueue.Lock()
	printQueue.loaded = true
	printQueue.byPrinter = map[string][]queueItem{
		"p1":   {{ID: "1", FileName: "a.3mf"}},
		"gone": {{ID: "2", FileName: "b.3mf"}},
	}
	err := queueSaveLocked()
	printQueue.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	raw, saved := readSavedQueues(t)
	if len(saved) != 2 {
		t.Fatalf("saved queues = %s", raw)
	}
}

func TestQueue_CorruptFileMovedAside(t *testing.T) {
	withQueue(t)
	os.MkdirAll(config.ConfigDir(), 0700)
	os.WriteFile(queuePath(), []byte("{not json"), 0600)
	if q := queueSnapshot(); len(q) != 0 {
		t.Fatalf("corrupt queue should load empty")
	}
	matches, _ := os.ReadDir(config.ConfigDir())
	found := false
	for _, m := range matches {
		if strings.HasPrefix(m.Name(), "queue.json.corrupt-") {
			found = true
			os.Remove(config.ConfigDir() + "/" + m.Name())
		}
	}
	if !found {
		t.Fatal("corrupt queue.json must be kept as queue.json.corrupt-*")
	}
}
