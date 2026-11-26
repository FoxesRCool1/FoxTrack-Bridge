package main

import (
	"html/template"
	"net"
	"net/http"
	"strings"
)

// writeHostRefused answers a request whose Host name hostAllowed refused. A
// browser opening the dashboard gets a page that says how to get in; scripts
// and the API get the usual JSON error. Nothing on the page loads from the
// Bridge: the stylesheet would be refused under the same name.
func writeHostRefused(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	name := host
	port := ""
	if h, p, err := net.SplitHostPort(host); err == nil {
		name, port = h, p
	}
	if r.Method != http.MethodGet || !strings.Contains(r.Header.Get("Accept"), "text/html") {
		writeErr(w, http.StatusForbidden, "This Bridge does not answer to the name "+name+
			". Open it by IP address, or add the name to FOXTRACK_BRIDGE_ALLOWED_HOSTS and restart the Bridge.")
		return
	}
	// Inside a container the addresses are the container's own, which the
	// browser cannot reach, so the page names the Docker computer instead.
	docker := hostRefusedInDocker()
	var links []string
	if !docker {
		for _, ip := range lanAddresses() {
			u := "http://" + ip
			if port != "" {
				u += ":" + port
			}
			links = append(links, u)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_ = hostRefusedPage.Execute(w, struct {
		Name   string
		Links  []string
		Docker bool
	}{name, links, docker})
}

// hostRefusedInDocker is inDocker; a test swaps it.
var hostRefusedInDocker = inDocker

// lanAddresses lists this computer's IPv4 addresses on interfaces that are up,
// loopback last. Container and VM bridges (docker0, br-…, veth…, virbr…) are
// left out: nobody opens the dashboard through them.
func lanAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out, loop []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || virtualInterface(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil || n.IP.IsLinkLocalUnicast() {
				continue
			}
			if n.IP.IsLoopback() {
				loop = append(loop, n.IP.String())
				continue
			}
			out = append(out, n.IP.String())
		}
	}
	return append(out, loop...)
}

func virtualInterface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "podman", "lxc"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

var hostRefusedPage = template.Must(template.New("host").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>FoxTrack Bridge: open it by its address</title>
<style>
:root { --bg: #f3ede0; --panel: rgb(60 45 30 / 5%); --border: rgb(60 45 30 / 14%); --text: #2b2118; --text-2: rgb(43 33 24 / 70%); --link: #43647f; }
@media (prefers-color-scheme: dark) {
  :root { --bg: #1a1a1c; --panel: rgb(255 255 255 / 5%); --border: rgb(255 255 255 / 10%); --text: #f2f2f0; --text-2: rgb(242 242 240 / 60%); --link: #9db3c9; }
}
body { margin: 0; background: var(--bg); color: var(--text); font: 15px/1.6 system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 36rem; margin: 0 auto; padding: 3rem 1rem; }
h1 { font-family: "Cormorant Garamond", Georgia, serif; font-weight: 500; font-size: 1.9rem; line-height: 1.2; margin: 0 0 .75rem; }
h2 { font-size: 1rem; font-weight: 600; margin: 0 0 .25rem; }
p { margin: 0 0 .75rem; color: var(--text-2); }
section { border: 1px solid var(--border); background: var(--panel); border-radius: .5rem; padding: 1rem 1.25rem; margin-top: 1rem; }
ul { margin: .25rem 0 0; padding-left: 1.25rem; }
a { color: var(--link); }
code { font: 13px ui-monospace, SFMono-Regular, Menlo, monospace; background: var(--panel); border: 1px solid var(--border); border-radius: .25rem; padding: 0 .3rem; overflow-wrap: anywhere; }
</style>
</head>
<body>
<main>
<h1>Open the Bridge by its address</h1>
<p>You opened this page as <code>{{.Name}}</code>. The Bridge only answers to names it knows, so a website can't pretend to be it.</p>
<section>
<h2>Open it now</h2>
{{if .Docker}}<p>In Docker, use the address of the computer that runs Docker.</p>
{{else if .Links}}<p>Use one of this computer's addresses:</p>
<ul>{{range .Links}}<li><a href="{{.}}">{{.}}</a></li>{{end}}</ul>{{else}}<p>Use this computer's IP address, for example <code>http://192.168.1.20:8080</code>.</p>{{end}}
</section>
<section>
<h2>Allow this name</h2>
<p>Start the Bridge with <code>FOXTRACK_BRIDGE_ALLOWED_HOSTS={{.Name}}</code>, then restart it. Separate several names with commas. In Docker, add the line under <code>environment:</code> in docker-compose.yml.</p>
<p>Names ending in .local, .lan, .home.arpa, .internal or .ts.net (Tailscale) work without this.</p>
</section>
</main>
</body>
</html>
`))
