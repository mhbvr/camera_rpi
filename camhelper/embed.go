// Package camhelper embeds the Picamera2 helper script so the server binary
// can write it out and run it.
package camhelper

import _ "embed"

// Script is camhelper.py.
//
//go:embed camhelper.py
var Script []byte
