//go:build !darwin || !cgo

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// webViewURL is "" off macOS cgo builds, so the navigation backstop does nothing there (lint builds only).
func webViewURL(*application.WebviewWindow) string { return "" }
