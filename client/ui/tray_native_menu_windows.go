//go:build windows

package main

import (
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Windows has no AppKit layer, so the connection row is a plain action under a
// status row that keeps the words and the dot (see tray_native_menu_darwin.h
// for what macOS does instead). What it does have is a live menu: Wails' items
// write straight into the HMENU with SetMenuItemInfo, so painting a row reaches
// a popup the user is holding open.
//
// What it must never do is rebuild that menu underneath them.
// windowsSystemTray.updateMenu destroys the old HMENU and builds a new one, and
// Win32 does not survive having a menu it is tracking pulled away: the popup
// freezes. macOS holds the rebuild back because AppKit reports its menus
// opening and closing; Wails reports nothing here, but Windows will answer the
// question directly if asked — GetGUIThreadInfo says whether the foreground
// thread is in menu mode and who owns the menu.

func nativeMenu() bool { return false }

func livePaintMenu() bool { return true }

func rowMarker(menuRow) string { return "" }

func startNativeMenu(func(bool), func(int), func(), func()) {}

// nativeMenuPainter exists so the painter selection stays platform-free; with
// no AppKit layer here it is never the one chosen.
type nativeMenuPainter struct{}

func (nativeMenuPainter) connection(string, bool, bool) {}

func (nativeMenuPainter) row(menuRow, string, bool, bool, []byte) {}

func (nativeMenuPainter) exitNodes([]string) {}

func (nativeMenuPainter) trayIcon([]byte, []byte) {}

func (nativeMenuPainter) submenuRows(menuState) {}

// GUI_INMENUMODE and GUI_POPUPMENUMODE are the two flags GetGUIThreadInfo
// raises while a menu is being tracked; a popup menu sets both.
const (
	guiInMenuMode    = 0x00000004
	guiPopupMenuMode = 0x00000010

	// wmCancelMode tells a window to drop whatever modal state it is in, menu
	// tracking included. It is the documented way to end another thread's menu.
	wmCancelMode = 0x001F
)

type guiThreadInfo struct {
	cbSize        uint32
	flags         uint32
	hwndActive    uintptr
	hwndFocus     uintptr
	hwndCapture   uintptr
	hwndMenuOwner uintptr
	hwndMoveSize  uintptr
	hwndCaret     uintptr
	rcCaret       struct{ left, top, right, bottom int32 }
}

var (
	user32                      = syscall.NewLazyDLL("user32.dll")
	procGetGUIThreadInfo        = user32.NewProc("GetGUIThreadInfo")
	procGetWindowThreadProcess  = user32.NewProc("GetWindowThreadProcessId")
	procPostMessage             = user32.NewProc("PostMessageW")
	menuCloseWatcherRunning     atomic.Bool
	menuCloseWatchInterval      = 200 * time.Millisecond
	menuCloseWatchMaxIterations = 5 * 60 * 5 // five minutes at the interval above
)

// menuOwner returns the window owning the popup menu currently on screen, or 0
// when there is none of ours. Asking for thread 0 asks about the foreground
// thread, which is the one tracking a menu while it is up; the owner is checked
// to belong to this process so another application's menu is never mistaken for
// the tray's.
func menuOwner() uintptr {
	info := guiThreadInfo{}
	info.cbSize = uint32(unsafe.Sizeof(info))

	ret, _, _ := procGetGUIThreadInfo.Call(0, uintptr(unsafe.Pointer(&info)))
	if ret == 0 || info.hwndMenuOwner == 0 {
		return 0
	}
	if info.flags&(guiInMenuMode|guiPopupMenuMode) == 0 {
		return 0
	}

	var pid uint32
	procGetWindowThreadProcess.Call(info.hwndMenuOwner, uintptr(unsafe.Pointer(&pid)))
	if pid != uint32(os.Getpid()) {
		return 0
	}
	return info.hwndMenuOwner
}

// trayMenuIsOpen reports whether this process is showing a popup menu, which is
// what holds a rebuild back: windowsSystemTray.updateMenu destroys the HMENU
// Win32 is tracking, and the popup freezes.
func trayMenuIsOpen() bool { return menuOwner() != 0 }

// dismissTrayMenu closes a tray popup that is up.
//
// Painting a row reaches the live HMENU, but Win32 redraws the popup when it
// sees fit — after a disconnect had finished, the row went on reading
// "Disconnecting" until the pointer passed over it. Rather than fight the
// repaint, the menu is dismissed on a connection change, and the next open
// shows the truth, rebuilt from scratch. Posted rather than called: EndMenu
// ends only the calling thread's menu, and this runs on a goroutine.
func dismissTrayMenu() {
	owner := menuOwner()
	if owner == 0 {
		return
	}
	procPostMessage.Call(owner, wmCancelMode, 0, 0)
}

// watchMenuClose runs fn once the menu is off the screen again. macOS is told
// by AppKit; here it has to be watched for, so a single goroutine polls until
// the popup is gone. The iteration cap keeps a menu left open — or a flag that
// somehow never clears — from spinning for the life of the process.
func watchMenuClose(fn func()) {
	if !menuCloseWatcherRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer menuCloseWatcherRunning.Store(false)
		for i := 0; i < menuCloseWatchMaxIterations; i++ {
			time.Sleep(menuCloseWatchInterval)
			if !trayMenuIsOpen() {
				fn()
				return
			}
		}
	}()
}

// reopenAfterDismiss reports that a menu this tray closed should be offered
// back once the connection settles: here it was dismissed only because Win32
// could not be trusted to redraw it.
func reopenAfterDismiss() bool { return true }
