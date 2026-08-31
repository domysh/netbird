//go:build darwin && !ios

package main

// The callbacks AppKit makes into Go. They live apart from the rest because
// cgo forbids a file that uses //export from carrying definitions in its
// preamble, and tray_native_menu_darwin.go's preamble is nothing but
// definitions.

import "C"

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
