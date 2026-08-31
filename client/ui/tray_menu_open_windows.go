//go:build windows

package main

import (
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// GUI_INMENUMODE and GUI_POPUPMENUMODE are the flags GetGUIThreadInfo raises
// while a menu is tracked; wmCancelMode tells a window to drop the modal state
// that tracking is.
const (
	guiInMenuMode    = 0x00000004
	guiPopupMenuMode = 0x00000010
	wmCancelMode     = 0x001F

	menuCloseWatchInterval = 200 * time.Millisecond
	// Five minutes at the interval above, so a menu left open cannot keep a
	// goroutine polling for the life of the process.
	menuCloseWatchMaxIterations = 5 * 60 * 5
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
	user32                     = syscall.NewLazyDLL("user32.dll")
	procGetGUIThreadInfo       = user32.NewProc("GetGUIThreadInfo")
	procGetWindowThreadProcess = user32.NewProc("GetWindowThreadProcessId")
	procPostMessage            = user32.NewProc("PostMessageW")

	menuCloseWatcherRunning atomic.Bool
)

// menuOwner returns the window owning the popup menu on screen, or 0 when none
// of ours is up. Thread 0 is the foreground thread, which is the one tracking a
// menu; the owner is checked to be this process so another application's menu
// is not mistaken for the tray's.
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

func trayMenuIsOpen() bool { return menuOwner() != 0 }

// dismissTrayMenu closes a tray popup that is up. Win32 repaints a popup when
// it sees fit, so a menu left standing through a connection change shows the
// old state until the pointer passes over it. Posted rather than called:
// EndMenu ends only the calling thread's menu, and this runs on a goroutine.
func dismissTrayMenu() {
	owner := menuOwner()
	if owner == 0 {
		return
	}
	procPostMessage.Call(owner, wmCancelMode, 0, 0)
}

// watchMenuClose runs fn once the popup is gone. Nothing reports the close, so
// it has to be watched for; one watcher at a time is enough.
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

func reopenAfterDismiss() bool { return true }
