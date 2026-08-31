//go:build !darwin && !android && !ios && !freebsd && !js

package main

// Off macOS there is no AppKit layer (see tray_native_menu_darwin.go): the
// hosts draw their own rows, take no custom view and report nothing about a
// menu opening or closing, so every state change goes through a full relayout
// and nothing is ever painted behind Wails' back.

func nativeMenu() bool { return false }

func livePaintMenu() bool { return false }

func rowMarker(menuRow) string { return "" }

func startNativeMenu(func(bool), func(int), func(), func()) {}

func trayMenuIsOpen() bool { return false }

// nativeMenuPainter exists so the painter selection stays platform-free. With
// trayMenuIsOpen always false here, it is never the one chosen.
type nativeMenuPainter struct{}

func (nativeMenuPainter) connection(string, bool, bool) {}

func (nativeMenuPainter) row(menuRow, string, bool, bool, []byte) {}

func (nativeMenuPainter) exitNodes([]string) {}

func (nativeMenuPainter) trayIcon([]byte, []byte) {}

func (nativeMenuPainter) submenuRows(menuState) {}
