//go:build !windows && !android && !ios && !freebsd && !js

package main

// Only Windows both freezes when its menu is rebuilt underneath it and can be
// asked whether a menu is up (see tray_menu_open_windows.go). Elsewhere the
// tray rebuilds as it always has.

func trayMenuIsOpen() bool { return false }

func dismissTrayMenu() {}

func watchMenuClose(func()) {}

func reopenAfterDismiss() bool { return false }
