//go:build darwin && !ios

package main

/*
#cgo CFLAGS: -mmacosx-version-min=10.15
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
#include "tray_native_menu_darwin.h"
*/
import "C"

import (
	"runtime"
	"sync"
	"unsafe"
)

// nativeMenu reports that the tray menu is painted through the AppKit layer in
// tray_native_menu_darwin.h. Two things follow from that single answer: the
// connection row is a switch rather than a checkbox, and state changes reach a
// menu that is already on screen instead of needing a rebuild.
func nativeMenu() bool { return true }

// livePaintMenu reports that a state change can be written onto the rows of a
// menu that is already up, rather than needing the whole tree rebuilt. True
// here through the AppKit painter above.
func livePaintMenu() bool { return true }

// rowMarker is the invisible suffix a row's label carries so the AppKit painter
// can find its NSMenuItem — Wails hands out no pointer to the items it builds.
// The markers are all two zero-width characters and mutually distinct, so no
// marker is a suffix of another and a lookup cannot land on the wrong row.
func rowMarker(row menuRow) string {
	switch row {
	case rowConnection:
		return "​​"
	case rowStatus:
		return "​‌"
	case rowSession:
		return "​‍"
	case rowExitNode:
		return "​⁠"
	case rowSettings:
		return "‌​"
	case rowProfiles:
		return "‌‌"
	}
	return ""
}

// The C callbacks carry no context pointer, so the handlers live here. Set once
// by startNativeMenu, before the first menu can open.
var (
	nativeMu       sync.Mutex
	switchToggleFn func(bool)
	exitNodeFn     func(int)
	menuOpenedFn   func()
	menuClosedFn   func()
)

// startNativeMenu arms the AppKit layer: the switch row, the run-loop painters,
// and the open/close reporting. onToggle receives the state the user asked the
// switch for, onExitNode the position of the exit-node row they picked.
func startNativeMenu(onToggle func(bool), onExitNode func(int), onMenuOpen, onMenuClose func()) {
	nativeMu.Lock()
	switchToggleFn = onToggle
	exitNodeFn = onExitNode
	menuOpenedFn = onMenuOpen
	menuClosedFn = onMenuClose
	nativeMu.Unlock()

	marker := C.CString(rowMarker(rowConnection))
	defer C.free(unsafe.Pointer(marker))
	C.nbmenu_start(marker)
}

// trayMenuIsOpen reports whether the tray menu is on screen. While it is, the
// Wails setters must not be called: they hop through the main dispatch queue,
// which AppKit does not drain during menu tracking, so a call would neither
// land nor return until the menu closed.
func trayMenuIsOpen() bool { return C.nbmenu_is_open() != 0 }

// watchMenuClose is a no-op here: AppKit announces the close through
// nbTrayMenuClosed, which drives the deferred rebuild already.
func watchMenuClose(func()) {}

// dismissTrayMenu is a no-op here: the AppKit painter reaches the rows of a
// menu that is up, so a state change shows itself without the menu having to
// go away.
func dismissTrayMenu() {}

// reopenAfterDismiss is false here: nothing is ever dismissed, because the
// AppKit painter reaches a menu that is up.
func reopenAfterDismiss() bool { return false }

// nativeMenuPainter writes state straight into AppKit, for a menu the user is
// looking at. It reaches the top-level rows and the status item's icon.
type nativeMenuPainter struct{}

func (nativeMenuPainter) connection(label string, on, enabled bool) {
	C.nbmenu_set_switch(cInt(on), cInt(enabled))

	text := C.CString(label)
	defer C.free(unsafe.Pointer(text))
	C.nbmenu_set_connection_label(text)
}

// row paints one ordinary row. An empty label leaves the row's title alone, and
// a nil bitmap leaves its image alone.
func (nativeMenuPainter) row(row menuRow, label string, enabled, hidden bool, bitmap []byte) {
	marker := C.CString(rowMarker(row))
	defer C.free(unsafe.Pointer(marker))

	var cLabel *C.char
	if label != "" {
		cLabel = C.CString(label)
		defer C.free(unsafe.Pointer(cLabel))
	}

	var png unsafe.Pointer
	var pngLen C.int
	if len(bitmap) > 0 {
		png = unsafe.Pointer(&bitmap[0])
		pngLen = C.int(len(bitmap))
	}

	C.nbmenu_set_row(marker, cLabel, cInt(enabled), cInt(hidden), png, pngLen)
	// The bytes are copied into an NSData before the call returns, so the Go
	// backing array only has to stay put until then.
	runtime.KeepAlive(bitmap)
}

// exitNodes replaces the Exit Node submenu with rows in this order, which is
// the order a click reports back as its index.
func (nativeMenuPainter) exitNodes(labels []string) {
	C.nbmenu_exit_nodes_begin()
	for _, label := range labels {
		cLabel := C.CString(label)
		C.nbmenu_exit_nodes_add(cLabel)
		C.free(unsafe.Pointer(cLabel))
	}
	marker := C.CString(rowMarker(rowExitNode))
	defer C.free(unsafe.Pointer(marker))
	C.nbmenu_exit_nodes_commit(marker)
}

// trayIcon paints the menu bar icon. The dark variant is Wails' business: macOS
// takes a single template image and inverts it itself.
func (nativeMenuPainter) trayIcon(icon, _ []byte) {
	if len(icon) == 0 {
		return
	}
	C.nbmenu_set_icon(unsafe.Pointer(&icon[0]), C.int(len(icon)))
	runtime.KeepAlive(icon)
}

// submenuRows is empty here: this painter finds top-level rows by their marker
// and no others, and nothing inside a submenu moves while the menu is open. The
// relayout on close paints them through Wails.
func (nativeMenuPainter) submenuRows(menuState) {}

func cInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// nbSwitchToggled is called by AppKit on the main thread while the menu is
// still tracking, so the handler runs on a goroutine: a connect blocks on the
// daemon, and blocking here would freeze the menu it is meant to keep alive.
//
//export nbSwitchToggled
func nbSwitchToggled(on C.int) {
	nativeMu.Lock()
	fn := switchToggleFn
	nativeMu.Unlock()
	if fn == nil {
		return
	}
	go fn(on != 0)
}

// nbExitNodeClicked reports the row's position in the list the painter last
// committed through exitNodes.
//
//export nbExitNodeClicked
func nbExitNodeClicked(index C.int) {
	nativeMu.Lock()
	fn := exitNodeFn
	nativeMu.Unlock()
	if fn == nil {
		return
	}
	go fn(int(index))
}

// nbTrayMenuOpened is called on the main thread as the menu opens, before it is
// on screen. The handler paints, which is a run-loop hop, so it runs on a
// goroutine rather than inside the tracking loop.
//
//export nbTrayMenuOpened
func nbTrayMenuOpened() {
	nativeMu.Lock()
	fn := menuOpenedFn
	nativeMu.Unlock()
	if fn == nil {
		return
	}
	go fn()
}

// nbTrayMenuClosed is called on the main thread when the tray menu leaves the
// screen. The handler goes back through Wails, which hops onto the main thread
// itself, so it must not run here.
//
//export nbTrayMenuClosed
func nbTrayMenuClosed() {
	nativeMu.Lock()
	fn := menuClosedFn
	nativeMu.Unlock()
	if fn == nil {
		return
	}
	go fn()
}
