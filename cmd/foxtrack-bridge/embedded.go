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
	logoDarkPNG  = assets.LogoDark
)
