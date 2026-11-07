package main

import (
	"foxtrack-bridge/assets"
	"foxtrack-bridge/web"
)

// The embedded files live next to their sources (web/, assets/); these names
// are what the server code uses.
var (
	uiHTML       = web.UI
	loginHTML    = web.Login
	tailwindCSS  = web.TailwindCSS
	iconsCSS     = web.IconsCSS
	fontsCSS     = web.FontsCSS
	fontsFS      = web.Fonts
	featuresFS   = web.Features
	i18nFS       = web.I18n
	logoPNG      = assets.Logo
	logoLightPNG = assets.LogoLight
	logo192PNG   = assets.Logo192
	logoDarkPNG  = assets.LogoDark
)

// manifestJSON is the web app manifest served at /manifest.webmanifest.
// background_color and theme_color follow the Paper theme's background.
var manifestJSON = []byte(`{
	"name": "FoxTrack Bridge",
	"short_name": "Bridge",
	"start_url": "/",
	"scope": "/",
	"display": "standalone",
	"background_color": "#f3ede0",
	"theme_color": "#f3ede0",
	"icons": [
		{"src": "/logo.png", "sizes": "512x512", "type": "image/png", "purpose": "any"},
		{"src": "/logo-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any"}
	]
}`)
