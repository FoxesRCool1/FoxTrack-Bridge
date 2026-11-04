package lan

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// skipNoObjects is the reason shown when the printer has no [exclude_object]
// objects to skip.
const skipNoObjects = "Klipper needs [exclude_object] in printer.cfg and objects labelled by the slicer."

// fetchExcludeObjects asks Moonraker for the [exclude_object] state.
func fetchExcludeObjects(p configpkg.Printer) (mqttpkg.SkipList, error) {
	client := &http.Client{Timeout: 6 * time.Second, Transport: insecureTransport()}
	body, err := sendJSONRequest(client, "GET",
		moonrakerURL(p, "/printer/objects/query?exclude_object&toolhead=axis_minimum,axis_maximum"),
		moonrakerAuthHeaders(p), nil)
	if err != nil {
		return mqttpkg.SkipList{}, fmt.Errorf("Bridge could not read the object list from the printer: %v", err)
	}
	return parseExcludeObjects(body)
}

// parseExcludeObjects turns a Moonraker objects/query reply into the skip
// list. Coordinates are normalised to the picture the dashboard draws:
// top-down, origin at the top left. Klipper's Y grows towards the back of the
// machine, a picture's grows downwards, so Y is flipped around the bed's far
// edge.
func parseExcludeObjects(m map[string]interface{}) (mqttpkg.SkipList, error) {
	status := nestedMap(m, "result", "status")
	exclude := nestedMapAny(status, "exclude_object")
	rawObjects, ok := exclude["objects"].([]interface{})
	if !ok || len(rawObjects) == 0 {
		return mqttpkg.SkipList{}, &mqttpkg.SkipUnsupported{Reason: skipNoObjects}
	}

	excluded := map[string]bool{}
	if names, ok := exclude["excluded_objects"].([]interface{}); ok {
		for _, n := range names {
			if s, ok := n.(string); ok {
				excluded[s] = true
			}
		}
	}
	current := stringAny(anyFromMap(exclude, "current_object"))

	toolhead := nestedMapAny(status, "toolhead")
	minA, okMin := toolhead["axis_minimum"].([]interface{})
	maxA, okMax := toolhead["axis_maximum"].([]interface{})
	hasToolhead := okMin && okMax && len(minA) >= 2 && len(maxA) >= 2 &&
		floatAny(maxA[0]) > floatAny(minA[0]) && floatAny(maxA[1]) > floatAny(minA[1])

	// First pass: gather every point so the origin can come from the object
	// points when the toolhead axes are missing.
	type rawPoint struct{ x, y float64 }
	type rawEntry struct {
		name    string
		polygon []rawPoint
		center  *rawPoint
	}
	var entries []rawEntry
	minX, maxY := 0.0, 0.0
	firstPoint := false
	for _, raw := range rawObjects {
		obj, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		name := stringAny(anyFromMap(obj, "name"))
		if name == "" {
			continue
		}
		entry := rawEntry{name: name}
		if pts, ok := obj["polygon"].([]interface{}); ok {
			for _, p := range pts {
				pair, ok := p.([]interface{})
				if !ok || len(pair) < 2 {
					continue
				}
				pt := rawPoint{x: floatAny(pair[0]), y: floatAny(pair[1])}
				entry.polygon = append(entry.polygon, pt)
				if !firstPoint {
					minX, maxY = pt.x, pt.y
					firstPoint = true
				}
				if pt.x < minX {
					minX = pt.x
				}
				if pt.y > maxY {
					maxY = pt.y
				}
			}
		}
		if c, ok := obj["center"].([]interface{}); ok && len(c) >= 2 {
			entry.center = &rawPoint{x: floatAny(c[0]), y: floatAny(c[1])}
			if !firstPoint {
				minX, maxY = entry.center.x, entry.center.y
				firstPoint = true
			}
			if entry.center.x < minX {
				minX = entry.center.x
			}
			if entry.center.y > maxY {
				maxY = entry.center.y
			}
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return mqttpkg.SkipList{}, &mqttpkg.SkipUnsupported{Reason: skipNoObjects}
	}

	var ox, oy float64
	var bed *mqttpkg.SkipBed
	if hasToolhead {
		ox, oy = floatAny(minA[0]), floatAny(maxA[1])
		bed = &mqttpkg.SkipBed{W: floatAny(maxA[0]) - floatAny(minA[0]), H: floatAny(maxA[1]) - floatAny(minA[1])}
	} else {
		ox, oy = minX, maxY
	}

	list := mqttpkg.SkipList{Bed: bed}
	for _, e := range entries {
		obj := mqttpkg.SkipObject{
			ID:      e.name,
			Name:    e.name,
			Skipped: excluded[e.name],
			Current: e.name == current,
		}
		if len(e.polygon) > 0 {
			for _, pt := range e.polygon {
				obj.Polygon = append(obj.Polygon, [2]float64{pt.x - ox, oy - pt.y})
			}
		} else if e.center != nil {
			cx, cy := e.center.x-ox, oy-e.center.y
			obj.BBox = []float64{cx - 5, cy - 5, cx + 5, cy + 5}
		}
		if len(obj.Polygon) > 0 {
			minX, minY, maxX, maxY := obj.Polygon[0][0], obj.Polygon[0][1], obj.Polygon[0][0], obj.Polygon[0][1]
			for _, pt := range obj.Polygon[1:] {
				if pt[0] < minX {
					minX = pt[0]
				}
				if pt[1] < minY {
					minY = pt[1]
				}
				if pt[0] > maxX {
					maxX = pt[0]
				}
				if pt[1] > maxY {
					maxY = pt[1]
				}
			}
			obj.BBox = []float64{minX, minY, maxX, maxY}
		}
		list.Objects = append(list.Objects, obj)
	}
	return list, nil
}

// safeObjectName reports whether a name may be put into an
// EXCLUDE_OBJECT NAME=... script. Klipper takes the rest of the line as the
// name, so anything that could end the line or start a comment must be
// refused.
func safeObjectName(s string) bool {
	if s == "" || len([]rune(s)) > 128 {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		switch r {
		case '_', '-', '.', '+', '(', ')', '[', ']', '~':
		default:
			return false
		}
	}
	return true
}

// excludeStart looks up the printer and its state and checks that skipping
// objects makes sense for it.
func (c *Controller) excludeStart(name string) (configpkg.Printer, error) {
	c.mu.RLock()
	p, ok := c.printers[name]
	state := c.states[name]
	c.mu.RUnlock()
	if !ok {
		return configpkg.Printer{}, fmt.Errorf("printer %q not found", name)
	}
	if p.Kind() != configpkg.KindKlipper {
		return p, &mqttpkg.SkipUnsupported{Reason: "Skipping objects works on Bambu and Klipper printers."}
	}
	if state == nil || (!mqttpkg.InPrint(state.Status)) {
		return p, &mqttpkg.SkipUnsupported{Reason: "Objects can be skipped only while a print is running or paused."}
	}
	return p, nil
}

// ExcludeObjects lists the objects on the plate and which are skipped.
func (c *Controller) ExcludeObjects(ctx context.Context, name string) (mqttpkg.SkipList, error) {
	p, err := c.excludeStart(name)
	if err != nil {
		return mqttpkg.SkipList{}, err
	}
	return fetchExcludeObjects(p)
}

// ExcludeObject marks the given objects as skipped, one EXCLUDE_OBJECT per
// object, and returns the fresh list. Objects sent before a failure stay
// skipped; that is fine.
func (c *Controller) ExcludeObject(ctx context.Context, name string, ids []string) (mqttpkg.SkipList, error) {
	p, err := c.excludeStart(name)
	if err != nil {
		return mqttpkg.SkipList{}, err
	}

	cleaned := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		cleaned = append(cleaned, id)
	}
	if len(cleaned) == 0 {
		return mqttpkg.SkipList{}, errors.New("Pick at least one object to skip.")
	}

	list, err := fetchExcludeObjects(p)
	if err != nil {
		return mqttpkg.SkipList{}, err
	}
	byID := map[string]mqttpkg.SkipObject{}
	for _, o := range list.Objects {
		byID[o.ID] = o
	}

	remaining := []string{}
	for _, id := range cleaned {
		o, ok := byID[id]
		if !ok {
			return mqttpkg.SkipList{}, &mqttpkg.SkipUnsupported{Reason: "One of those objects is not on the printer's plate. Refresh the list and try again."}
		}
		if o.Skipped {
			continue
		}
		remaining = append(remaining, id)
	}
	if len(remaining) == 0 {
		return mqttpkg.SkipList{}, &mqttpkg.SkipUnsupported{Reason: "Those objects are already skipped."}
	}
	for _, id := range remaining {
		if !safeObjectName(id) {
			return mqttpkg.SkipList{}, &mqttpkg.SkipUnsupported{Reason: "Bridge will not send that object's name to the printer because it has unusual characters. Skip it on the printer instead."}
		}
	}

	client := &http.Client{Timeout: 6 * time.Second, Transport: insecureTransport()}
	headers := moonrakerAuthHeaders(p)
	for _, id := range remaining {
		if err := ctx.Err(); err != nil {
			return mqttpkg.SkipList{}, err
		}
		script := "EXCLUDE_OBJECT NAME=" + id
		u := moonrakerURL(p, "/printer/gcode/script?script="+url.QueryEscape(script))
		if _, err := sendJSONRequest(client, "POST", u, headers, nil); err != nil {
			return mqttpkg.SkipList{}, fmt.Errorf("The printer did not take the skip command: %v", err)
		}
		log.Printf("[%s] exclude_object: %s", name, id)
	}

	return fetchExcludeObjects(p)
}
