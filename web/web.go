// Package web embeds the dashboard: the page, its styles, fonts, feature
// scripts and translations. Nothing here is downloaded at build or run time.
package web

import "embed"

//go:embed ui.html
var UI []byte

//go:embed login.html
var Login []byte

// Share is the read-only page behind a share link (/share/{token}).
//
//go:embed share.html
var Share []byte

//go:embed tailwind.css
var TailwindCSS []byte

//go:embed icons.css
var IconsCSS []byte

//go:embed fonts.css
var FontsCSS []byte

// Fonts holds fonts/<name>.woff2.
//
//go:embed fonts
var Fonts embed.FS

// Features holds every features/*.js, served joined as /features.js.
//
//go:embed features
var Features embed.FS

// I18n holds one i18n/<lang>.json per language.
//
//go:embed i18n
var I18n embed.FS
