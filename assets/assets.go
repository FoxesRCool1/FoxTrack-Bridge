// Package assets embeds the FoxTrack logos.
package assets

import _ "embed"

// Logo is the app icon and notification image.
//
//go:embed logo.png
var Logo []byte

// Theme-specific rail logos (the FoxTrack web app's header marks): dark ink
// lines for the light theme, light lines for the dark theme.
//
//go:embed logo-light.png
var LogoLight []byte

//go:embed logo-dark.png
var LogoDark []byte
