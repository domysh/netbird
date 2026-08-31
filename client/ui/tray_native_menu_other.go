//go:build !darwin && !windows && !android && !ios && !freebsd && !js

package main

// This is the dbusmenu side: the hosts draw their own rows, take no custom
// view, report nothing about a menu opening or closing, and cache a submenu's
// layout on first open — so every state change goes through a full relayout
// (see relayoutMenu), and nothing is ever painted behind Wails' back. macOS has
// the AppKit layer instead (tray_native_menu_darwin.go) and Windows its own
// arrangement (tray_native_menu_windows.go).

func nativeMenu() bool { return false }

func livePaintMenu() bool { return false }

func rowMarker(menuRow) string { return "" }

func startNativeMenu(func(bool), func(int), func(), func()) {}

func trayMenuIsOpen() bool { return false }

// watchMenuClose has nothing to watch: with trayMenuIsOpen always false, no
// relayout is ever held back here.
func watchMenuClose(func()) {}

// dismissTrayMenu is a no-op here: nothing reports whether a menu is open, and
// the relayout on the next state change rebuilds the tree anyway.
func dismissTrayMenu() {}

// reopenAfterDismiss is false here: nothing is ever dismissed.
func reopenAfterDismiss() bool { return false }

// nativeMenuPainter exists so the painter selection stays platform-free. With
// trayMenuIsOpen always false here, it is never the one chosen.
type nativeMenuPainter struct{}

func (nativeMenuPainter) connection(string, bool, bool) {}

func (nativeMenuPainter) row(menuRow, string, bool, bool, []byte) {}

func (nativeMenuPainter) exitNodes([]string) {}

func (nativeMenuPainter) trayIcon([]byte, []byte) {}

func (nativeMenuPainter) submenuRows(menuState) {}
