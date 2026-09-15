package main

import "embed"

//go:embed web/ui.html
var uiHTML []byte

//go:embed assets/logo.png
var logoPNG []byte

// Theme-specific rail logos (the FoxTrack web app's header marks): dark ink
// lines for the light theme, light lines for the dark theme.
//
//go:embed assets/logo-light.png
var logoLightPNG []byte

//go:embed assets/logo-dark.png
var logoDarkPNG []byte

//go:embed web/tailwind.css
var tailwindCSS []byte

//go:embed web/icons.css
var iconsCSS []byte

//go:embed web/fonts.css
var fontsCSS []byte

//go:embed web/fonts
var fontsFS embed.FS
