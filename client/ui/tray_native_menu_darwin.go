//go:build darwin && !ios

package main

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
// by an invisible marker suffix on their title (see rowMarker below) and the
// menu itself is caught as it opens, via NSMenuDidBeginTracking.
//
// Every function is safe to call from any thread. Calls are no-ops until the
// tray menu has been opened once, which is also the only moment it can be
// found — until then the Wails setters work fine, because the menu is closed.

/*
#cgo CFLAGS: -mmacosx-version-min=10.15 -x objective-c
#cgo LDFLAGS: -framework Cocoa

#import <Cocoa/Cocoa.h>

// Implemented in Go (see tray_native_menu_callbacks_darwin.go). All four are
// called on the main thread, from inside the menu's tracking loop, and hand the
// work to a goroutine — anything slower would freeze the menu they exist to
// keep alive.
extern void nbSwitchToggled(int on);
extern void nbExitNodeClicked(int index);
extern void nbTrayMenuOpened(void);
extern void nbTrayMenuClosed(void);

// Row geometry for the switch. The leading inset lines the label up with the
// text of the ordinary rows, and the switch keeps the trailing margin AppKit
// leaves for a submenu arrow. 14pt is where AppKit starts a title in a menu
// that reserves neither a state nor an image column — which is why the tray
// menu no longer has a checkbox row or a status dot in it.
static const CGFloat kLeadingInset = 14.0;
static const CGFloat kTrailingInset = 14.0;
static const CGFloat kMinGap = 24.0;
static const CGFloat kVerticalPad = 3.0;

// NBSwitchRow exists so an already-installed row can be recognised on a later
// open: a menu that was not relaid out hands back the same item and switch, and
// rebuilding them would drop the state mid-tracking.
@interface NBSwitchRow : NSView
@end

@implementation NBSwitchRow
@end

@interface NBMenuWatcher : NSObject
- (void)menuBeganTracking:(NSNotification *)note;
- (void)menuEndedTracking:(NSNotification *)note;
- (void)switchToggled:(id)sender;
- (void)exitNodeClicked:(id)sender;
@end

// Main-thread state. gTrayMenu, gSwitch and gLabel are retained: Wails destroys
// the whole item tree on a relayout, and a stale unowned pointer would be a
// dangling read on the next paint.
static NBMenuWatcher *gWatcher = nil;
static NSString *gConnectionMarker = nil;
static NSArray *gModes = nil;
static NSMenu *gTrayMenu = nil;
static NSSwitch *gSwitch = nil;
static NSTextField *gLabel = nil;
static BOOL gOn = NO;
static BOOL gEnabled = YES;
// gActivated remembers that the menu, not the user, made this app the active
// one, so only that activation is handed back when the menu closes.
static BOOL gActivated = NO;

// gStatusButton is the menu bar button itself, looked up once and retained: it
// outlives the menu, and the lookup walks every window in the app.
static NSStatusBarButton *gStatusButton = nil;

// gStaging collects exit-node rows between begin and commit. Touched only by
// the (serialised) Go caller until commit hands it to the main thread.
static NSMutableArray *gStaging = nil;

// gOpen is written on the main thread and read from Go goroutines, hence
// volatile: it carries a hint (paint natively, hold the relayout back), and a
// synchronous read would deadlock a caller already on the main thread.
static volatile int gOpen = 0;

// nbOnMain runs block on the main thread in a way that survives menu tracking.
// dispatch_get_main_queue() is deliberately not used: its drain never happens
// while a menu is up, which is precisely when this file has work to do.
//
// gModes must already be set, and nbmenu_start fills it synchronously: a state
// push can arrive before the app runs, and CFRunLoopPerformBlock with no mode
// drops the block.
static void nbOnMain(void (^block)(void)) {
    if ([NSThread isMainThread]) {
        block();
        return;
    }
    CFRunLoopRef runLoop = CFRunLoopGetMain();
    CFRunLoopPerformBlock(runLoop, (__bridge CFTypeRef)gModes, block);
    CFRunLoopWakeUp(runLoop);
}

// nbFindItem returns the top-level row whose title carries marker. Markers are
// all the same length and mutually distinct, so a suffix match cannot pick the
// wrong row. Main thread.
static NSMenuItem *nbFindItem(NSString *marker) {
    if (gTrayMenu == nil || marker == nil) {
        return nil;
    }
    for (NSMenuItem *item in [gTrayMenu itemArray]) {
        NSString *title = [item title];
        if (title != nil && [title hasSuffix:marker]) {
            return item;
        }
    }
    return nil;
}

// nbSearchStatusButton walks a view tree looking for the status item's button.
// It sits a couple of levels down inside the status bar window's content view,
// so the search has to recurse.
static NSStatusBarButton *nbSearchStatusButton(NSView *view) {
    if (view == nil) {
        return nil;
    }
    if ([view isKindOfClass:[NSStatusBarButton class]]) {
        return (NSStatusBarButton *)view;
    }
    for (NSView *subview in [view subviews]) {
        NSStatusBarButton *found = nbSearchStatusButton(subview);
        if (found != nil) {
            return found;
        }
    }
    return nil;
}

// nbStatusButton finds the status item's button. Wails keeps the NSStatusItem
// to itself, but the button lives in a window of the app's own — an
// NSStatusBarWindow — so the window list reaches it. This app has exactly one
// status item, so the first hit is the right one. Main thread.
static NSStatusBarButton *nbStatusButton(void) {
    if (gStatusButton != nil) {
        return gStatusButton;
    }
    for (NSWindow *window in [NSApp windows]) {
        NSStatusBarButton *found = nbSearchStatusButton([window contentView]);
        if (found != nil) {
            gStatusButton = [found retain];
            return gStatusButton;
        }
    }
    return nil;
}

// nbApplyState paints the remembered state onto the live switch. Main thread.
static void nbApplyState(void) {
    if (gSwitch == nil) {
        return;
    }
    [gSwitch setState:(gOn ? NSControlStateValueOn : NSControlStateValueOff)];
    [gSwitch setEnabled:gEnabled];
    [gSwitch setNeedsDisplay:YES];
    [gLabel setTextColor:(gEnabled ? [NSColor labelColor] : [NSColor disabledControlTextColor])];
}

// nbLayoutRow places the label and the switch inside the row.
//
// resize is NO once the menu is on screen: AppKit sized the view when it laid
// the menu out — item views are stretched to the width of the widest row — and
// growing it under an open menu would push the switch past the edge. The label
// changes with the connection state, so this runs on every state change; the
// autoresizing masks keep the switch pinned to the trailing edge either way.
static void nbLayoutRow(NBSwitchRow *row, BOOL resize) {
    [gLabel sizeToFit];
    NSSize switchSize = [gSwitch frame].size;
    NSSize labelSize = [gLabel frame].size;

    CGFloat width = [row frame].size.width;
    CGFloat height = [row frame].size.height;
    if (resize || width <= 0) {
        width = kLeadingInset + labelSize.width + kMinGap + switchSize.width + kTrailingInset;
        height = MAX(switchSize.height, labelSize.height) + kVerticalPad * 2;
        [row setFrame:NSMakeRect(0, 0, width, height)];
    }

    [gLabel setFrame:NSMakeRect(kLeadingInset,
                                (height - labelSize.height) / 2.0,
                                labelSize.width,
                                labelSize.height)];
    [gSwitch setFrame:NSMakeRect(width - kTrailingInset - switchSize.width,
                                 (height - switchSize.height) / 2.0,
                                 switchSize.width,
                                 switchSize.height)];
}

// nbBuildRow returns the view for the connection row: the item's own title on
// the left, the switch on the right.
static NBSwitchRow *nbBuildRow(NSString *title) {
    NSSwitch *sw = [[NSSwitch alloc] initWithFrame:NSZeroRect];
    [sw sizeToFit];
    [sw setState:(gOn ? NSControlStateValueOn : NSControlStateValueOff)];
    [sw setEnabled:gEnabled];
    [sw setTarget:gWatcher];
    [sw setAction:@selector(switchToggled:)];

    NSTextField *label = [NSTextField labelWithString:title];
    [label setFont:[NSFont menuFontOfSize:0]];

    NBSwitchRow *row = [[NBSwitchRow alloc] initWithFrame:NSZeroRect];
    [row setAutoresizingMask:NSViewWidthSizable];
    [label setAutoresizingMask:NSViewMaxXMargin];
    [sw setAutoresizingMask:NSViewMinXMargin];
    [row addSubview:label];
    [row addSubview:sw];

    [gSwitch release];
    [gLabel release];
    gSwitch = [sw retain];
    gLabel = [label retain];
    [sw release];

    nbLayoutRow(row, YES);
    return row;
}

// nbAdoptMenu looks for the connection row in menu and makes sure it carries
// the switch view, recording menu as the tray menu when it does. Called as the
// menu opens, the only moment the NSMenu Wails built is in reach.
static void nbAdoptMenu(NSMenu *menu) {
    if (gConnectionMarker == nil || menu == nil) {
        return;
    }
    for (NSMenuItem *item in [menu itemArray]) {
        NSString *title = [item title];
        if (title == nil || ![title hasSuffix:gConnectionMarker]) {
            continue;
        }
        if (![[item view] isKindOfClass:[NBSwitchRow class]]) {
            NSString *text = [title substringToIndex:([title length] - [gConnectionMarker length])];
            NBSwitchRow *row = nbBuildRow(text);
            [item setView:row];
            [row release];
        }
        if (gTrayMenu != menu) {
            [gTrayMenu release];
            gTrayMenu = [menu retain];
        }
        nbApplyState();
        return;
    }
}

@implementation NBMenuWatcher

- (void)menuBeganTracking:(NSNotification *)note {
    NSMenu *menu = (NSMenu *)[note object];
    nbAdoptMenu(menu);
    if (menu != gTrayMenu) {
        return;
    }
    gOpen = 1;
    // Become the active app for as long as the menu is up. AppKit paints
    // accent-coloured controls — the switch's on state above all — in the
    // inactive grey while the app is not the active one, and a menu bar app is
    // never active on its own: opening a status item menu does not activate it.
    // The app is an accessory, so activating brings no window and no menu bar
    // of its own forward; menuEndedTracking hands the frontmost slot straight
    // back.
    if (![NSApp isActive]) {
        gActivated = YES;
        [NSApp activateIgnoringOtherApps:YES];
    }
    // The rows Wails just rebuilt carry whatever state the last relayout left
    // on them. Ask Go to repaint from the daemon's own truth, so an open menu
    // always starts out right — and one lost push cannot leave it wrong until
    // the next status change.
    nbTrayMenuOpened();
}

- (void)menuEndedTracking:(NSNotification *)note {
    if ((NSMenu *)[note object] != gTrayMenu) {
        return;
    }
    gOpen = 0;
    if (gActivated) {
        gActivated = NO;
        [NSApp deactivate];
    }
    nbTrayMenuClosed();
}

// switchToggled reports the state the user asked for. The control has already
// flipped itself; the daemon's answer comes back through nbmenu_set_switch,
// which corrects it if the request does not land.
- (void)switchToggled:(id)sender {
    gOn = ([(NSSwitch *)sender state] == NSControlStateValueOn);
    nbSwitchToggled(gOn ? 1 : 0);
}

// exitNodeClicked carries the row's position in the list Go last committed;
// Go owns the mapping back to a network id.
- (void)exitNodeClicked:(id)sender {
    nbExitNodeClicked((int)[(NSMenuItem *)sender tag]);
}

@end

void nbmenu_start(const char *connection_marker) {
    if (connection_marker == NULL) {
        return;
    }
    // Called once from Go, before the app runs and so before any menu exists:
    // the marker and the run-loop modes are set here and now, because every
    // painter below needs them and the state pushes start immediately.
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        gConnectionMarker = [[NSString alloc] initWithUTF8String:connection_marker];
        gModes = [@[NSEventTrackingRunLoopMode, NSDefaultRunLoopMode, NSModalPanelRunLoopMode] retain];
        nbOnMain(^{
            gWatcher = [[NBMenuWatcher alloc] init];
            NSNotificationCenter *center = [NSNotificationCenter defaultCenter];
            [center addObserver:gWatcher
                       selector:@selector(menuBeganTracking:)
                           name:NSMenuDidBeginTrackingNotification
                         object:nil];
            [center addObserver:gWatcher
                       selector:@selector(menuEndedTracking:)
                           name:NSMenuDidEndTrackingNotification
                         object:nil];
        });
    });
}

void nbmenu_set_switch(int on, int enabled) {
    nbOnMain(^{
        gOn = (on != 0);
        gEnabled = (enabled != 0);
        nbApplyState();
    });
}

void nbmenu_set_connection_label(const char *label) {
    if (label == NULL) {
        return;
    }
    NSString *text = [[NSString alloc] initWithUTF8String:label];
    nbOnMain(^{
        if (gLabel != nil && ![[gLabel stringValue] isEqualToString:text]) {
            [gLabel setStringValue:text];
            NSMenuItem *item = nbFindItem(gConnectionMarker);
            NSView *row = (item != nil) ? [item view] : nil;
            if ([row isKindOfClass:[NBSwitchRow class]]) {
                nbLayoutRow((NBSwitchRow *)row, gOpen == 0);
            }
        }
        [text release];
    });
}

void nbmenu_set_row(const char *marker, const char *label, int enabled, int hidden,
                    const void *png, int png_len) {
    if (marker == NULL) {
        return;
    }
    NSString *key = [[NSString alloc] initWithUTF8String:marker];
    NSString *text = (label != NULL) ? [[NSString alloc] initWithUTF8String:label] : nil;
    NSData *image = (png != NULL && png_len > 0) ? [[NSData alloc] initWithBytes:png length:(NSUInteger)png_len] : nil;
    nbOnMain(^{
        NSMenuItem *item = nbFindItem(key);
        if (item != nil) {
            if (text != nil) {
                [item setTitle:[text stringByAppendingString:key]];
            }
            [item setEnabled:(enabled != 0)];
            [item setHidden:(hidden != 0)];
            if (image != nil) {
                NSImage *bitmap = [[NSImage alloc] initWithData:image];
                [item setImage:bitmap];
                [bitmap release];
            }
        }
        [key release];
        [text release];
        [image release];
    });
}

void nbmenu_exit_nodes_begin(void) {
    [gStaging release];
    gStaging = [[NSMutableArray alloc] init];
}

void nbmenu_exit_nodes_add(const char *label) {
    if (gStaging == nil || label == NULL) {
        return;
    }
    [gStaging addObject:[NSString stringWithUTF8String:label]];
}

void nbmenu_exit_nodes_commit(const char *marker) {
    if (marker == NULL) {
        [gStaging release];
        gStaging = nil;
        return;
    }
    NSArray *rows = gStaging;
    gStaging = nil;
    NSString *key = [[NSString alloc] initWithUTF8String:marker];
    nbOnMain(^{
        NSMenuItem *parent = nbFindItem(key);
        NSMenu *submenu = (parent != nil) ? [parent submenu] : nil;
        if (submenu != nil) {
            [submenu removeAllItems];
            NSInteger index = 0;
            for (NSString *label in rows) {
                NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:label
                                                              action:@selector(exitNodeClicked:)
                                                       keyEquivalent:@""];
                [item setTarget:gWatcher];
                [item setTag:index++];
                [submenu addItem:item];
                [item release];
            }
        }
        [key release];
        [rows release];
    });
}

void nbmenu_set_icon(const void *png, int png_len) {
    if (png == NULL || png_len <= 0) {
        return;
    }
    NSData *data = [[NSData alloc] initWithBytes:png length:(NSUInteger)png_len];
    nbOnMain(^{
        NSStatusBarButton *button = nbStatusButton();
        if (button != nil) {
            NSImage *image = [[NSImage alloc] initWithData:data];
            // Same sizing and template flag Wails applies, so the icon painted
            // here and the one it pushes on close are the same picture.
            CGFloat thickness = [[NSStatusBar systemStatusBar] thickness];
            [image setSize:NSMakeSize(thickness, thickness)];
            [image setTemplate:YES];
            [button setImage:image];
            [image release];
        }
        [data release];
    });
}

int nbmenu_is_open(void) {
    return gOpen;
}
*/
import "C"

import (
	"runtime"
	"sync"
	"unsafe"
)

// nativeMenu reports that the tray menu is painted through the AppKit layer in
// tray_native_menu_darwin.go. Two things follow from that single answer: the
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
