//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include <stdlib.h>
#include <string.h>

static WKWebView* findWebView(NSView* v) {
	if ([v isKindOfClass:[WKWebView class]]) return (WKWebView*)v;
	for (NSView* s in v.subviews) {
		WKWebView* w = findWebView(s);
		if (w) return w;
	}
	return nil;
}

// webViewURL returns the web view's URL (malloc'd, or NULL) and unregisters it as a drop target.
static char* webViewURL(void* win) {
	WKWebView* wv = findWebView([(NSWindow*)win contentView]);
	if (!wv) return NULL;
	// A link or file dropped on a WKWebView loads it in the window; without drag types AppKit never offers it.
	[wv unregisterDraggedTypes];
	NSString* s = wv.URL.absoluteString;
	return s ? strdup(s.UTF8String) : NULL;
}
*/
import "C"

import (
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// webViewURL returns the URL the window's web view shows ("" if none) and stops the web view from taking drops.
func webViewURL(win *application.WebviewWindow) string {
	var u string
	application.InvokeSync(func() { // AppKit and WebKit are main-thread only
		p := win.NativeWindow()
		if p == nil {
			return
		}
		if c := C.webViewURL(p); c != nil {
			u = C.GoString(c)
			C.free(unsafe.Pointer(c))
		}
	})
	return u
}
