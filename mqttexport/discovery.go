package mqttexport

import (
	"bytes"
	"encoding/json"
	"strings"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// The Home Assistant MQTT discovery configs: topic -> JSON payload. Maps (not
// structs) so the JSON keys come out sorted and the same every time, which is
// what lets sync tell a changed config from an unchanged one.

type obj = map[string]any

func manufacturer(p config.Printer) string {
	switch p.Kind() {
	case config.KindBambu:
		return "Bambu Lab"
	case config.KindPrusaLink:
		return "Prusa Research"
	}
	return p.KindLabel()
}

func discoveryTopic(c config.MQTTExport, component, object string) string {
	return c.DiscoveryPrefix + "/" + component + "/" + nodeID(c) + "/" + object + "/config"
}

// bridgeDiscovery is the Bridge's own device, so each printer's via_device
// points at something real.
func bridgeDiscovery(c config.MQTTExport, version string) map[string]string {
	cfg := obj{
		"name": "Connected", "unique_id": nodeID(c) + "_bridge", "has_entity_name": true,
		"device_class": "connectivity", "state_topic": availabilityTopic(c),
		"payload_on": "online", "payload_off": "offline",
		"device": obj{"identifiers": []string{nodeID(c)}, "name": "FoxTrack Bridge", "manufacturer": "FoxTrack", "model": "Bridge", "sw_version": version},
	}
	return map[string]string{discoveryTopic(c, "binary_sensor", "bridge"): marshal(cfg)}
}

// Discovery returns the configs for one printer: sensors, binary sensors,
// pause/resume/stop buttons and, where the printer has one, the light. Control
// follows the dashboard: no buttons for Bambu Cloud or the read-only Home
// Assistant kind, and a light only on Bambu and Klipper.
func Discovery(c config.MQTTExport, p config.Printer, t *mqttpkg.TelemetryData) map[string]string {
	out := map[string]string{}
	device := obj{
		"identifiers": []string{nodeID(c) + "_" + p.ID}, "name": p.Name,
		"manufacturer": manufacturer(p), "model": modelOf(p, t), "via_device": nodeID(c),
	}
	st, avail := stateTopic(c, p.ID), availabilityTopic(c)
	add := func(component, object, name string, extra obj) {
		cfg := obj{
			"name": name, "unique_id": nodeID(c) + "_" + p.ID + "_" + object, "has_entity_name": true,
			"device": device, "availability_topic": avail,
			"payload_available": "online", "payload_not_available": "offline",
		}
		for k, v := range extra {
			cfg[k] = v
		}
		out[discoveryTopic(c, component, p.ID+"_"+object)] = marshal(cfg)
	}
	sensor := func(object, name, tpl string, extra obj) {
		e := obj{"state_topic": st, "value_template": "{{ " + tpl + " }}"}
		for k, v := range extra {
			e[k] = v
		}
		add("sensor", object, name, e)
	}
	flag := func(object, name, cond, class string) {
		add("binary_sensor", object, name, obj{
			"state_topic": st, "value_template": "{{ 'ON' if " + cond + " else 'OFF' }}", "device_class": class,
		})
	}
	temp := obj{"device_class": "temperature", "unit_of_measurement": "°C", "state_class": "measurement", "suggested_display_precision": 1}

	sensor("status", "Status", "value_json.status", obj{"icon": "mdi:printer-3d"})
	sensor("progress", "Progress", "value_json.progress", obj{"unit_of_measurement": "%", "state_class": "measurement", "icon": "mdi:progress-clock"})
	sensor("nozzle_temp", "Nozzle temperature", "value_json.nozzle_temp", temp)
	sensor("bed_temp", "Bed temperature", "value_json.bed_temp", temp)
	sensor("time_remaining", "Time remaining", "value_json.time_remaining", obj{"device_class": "duration", "unit_of_measurement": "min", "state_class": "measurement"})
	sensor("file", "Current file", "value_json.file[:250]", obj{"icon": "mdi:file-outline"})
	flag("online", "Online", "value_json.online", "connectivity")
	flag("printing", "Printing", "value_json.status == 'printing'", "running")
	flag("problem", "Problem", "value_json.alarms > 0", "problem")

	kind := p.Kind()
	if kind != config.KindHomeAssistant && !p.IsCloud() {
		for _, b := range []struct{ cmd, name, icon string }{
			{"pause", "Pause", "mdi:pause"}, {"resume", "Resume", "mdi:play"}, {"stop", "Stop", "mdi:stop"},
		} {
			add("button", b.cmd, b.name, obj{"command_topic": commandTopic(c, p.ID), "payload_press": b.cmd, "icon": b.icon})
		}
	}
	if kind == config.KindBambu || kind == config.KindKlipper {
		add("light", "light", "Light", obj{
			"command_topic": commandTopic(c, p.ID), "payload_on": "light_on", "payload_off": "light_off",
			"state_topic": st, "state_value_template": "{{ 'ON' if value_json.light else 'OFF' }}",
			"state_on": "ON", "state_off": "OFF",
		})
	}
	return out
}

// marshal writes compact JSON without escaping < > &, which templates use.
func marshal(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSpace(b.String())
}
