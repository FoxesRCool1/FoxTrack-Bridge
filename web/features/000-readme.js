// Feature scripts live in this folder, one per Settings section. The server
// joins every *.js here, sorted by name, into /features.js, which loads right
// after the dashboard script, so its helpers are in scope: apiFetch, toast,
// escHtml, fill, tip, setSwitch, getSwitch, friendlyError, confirmDialog,
// configuredPrinters, liveStatus and registerSettingsTab. Each file is wrapped
// in its own block scope by the server, so top-level names do not collide.
