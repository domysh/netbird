//go:build darwin && !ios

#ifndef TRAY_NATIVE_MENU_DARWIN_H
#define TRAY_NATIVE_MENU_DARWIN_H

// Painting the tray menu while the user has it open.
//
// Two AppKit facts drive this file. First, while an NSMenu is tracking, the
// main thread sits in the menu's own event loop and the main dispatch queue is
// never drained — every Wails menu setter hops through dispatch_sync/async, so
// a repaint issued while the menu is open does not land, and the goroutine
// that asked for it blocks until the menu closes. What does run in that loop
// is work scheduled on the main *run loop* in NSEventTrackingRunLoopMode, so
// every setter here goes through nbOnMain, which schedules exactly that way.
// Second, AppKit dismisses a menu as soon as an ordinary item is picked, which
// is why the connection row is an NSSwitch inside a custom item view: a
// control in an item view handles its own clicks and the menu stays up.
//
// Wails exposes no pointer to the NSMenu or its items, so rows are addressed
// by an invisible marker suffix on their title (see tray_native_menu_darwin.go)
// and the menu itself is caught as it opens, via NSMenuDidBeginTracking.
//
// Every function is safe to call from any thread. Calls are no-ops until the
// tray menu has been opened once, which is also the only moment it can be
// found — until then the Wails setters work fine, because the menu is closed.

// nbmenu_start installs the menu-tracking observers. connection_marker tags the
// row that becomes the switch; the string is copied. Idempotent.
void nbmenu_start(const char *connection_marker);

// nbmenu_set_switch pushes the state the connection row must show. Remembered
// for the next install too — the row is rebuilt whenever Wails relays the menu
// out.
void nbmenu_set_switch(int on, int enabled);

// nbmenu_set_connection_label retitles the switch row.
void nbmenu_set_connection_label(const char *label);

// nbmenu_set_row paints one ordinary row, addressed by its marker. label may be
// NULL to leave the title alone; png may be NULL to leave the image alone.
void nbmenu_set_row(const char *marker, const char *label, int enabled, int hidden,
                    const void *png, int png_len);

// Exit-node rows are staged and then committed in one go, so the submenu is
// replaced atomically on the main thread. The three calls must come from one
// goroutine at a time; the Go side serialises them.
void nbmenu_exit_nodes_begin(void);
void nbmenu_exit_nodes_add(const char *label);
void nbmenu_exit_nodes_commit(const char *marker);

// nbmenu_set_icon paints the status item's own icon. Wails pushes it through
// the main dispatch queue, so under an open menu its call never lands; this one
// takes the same run-loop route as the rows and reaches the menu bar while the
// user is looking at it. The bytes are a PNG, drawn as a template image the way
// the tray's icons are.
void nbmenu_set_icon(const void *png, int png_len);

// nbmenu_is_open reports whether the tray menu is on screen, so the Go side can
// pick the native painters over the Wails ones and hold back a relayout that
// would pull the open menu out from under the user.
int nbmenu_is_open(void);

#endif
