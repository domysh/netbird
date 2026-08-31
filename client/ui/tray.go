//go:build !android && !ios && !freebsd && !js

package main

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"github.com/netbirdio/netbird/client/ui/authsession"
	"github.com/netbirdio/netbird/client/ui/i18n"
	"github.com/netbirdio/netbird/client/ui/preferences"
	"github.com/netbirdio/netbird/client/ui/services"
	"github.com/netbirdio/netbird/version"
)

// Notification IDs are OS dedup keys that coalesce duplicate toasts;
// statusError is a tray-only sentinel for the error-icon state.
const (
	notifyIDUpdatePrefix = "netbird-update-"
	notifyIDEvent        = "netbird-event-"
	notifyIDTrayError    = "netbird-tray-error"
	notifyIDMDMPolicy    = "netbird-mdm-policy"

	statusError = "Error"

	quitDownTimeout = 5 * time.Second

	urlGitHubRepo = "https://github.com/netbirdio/netbird"
	urlDocs       = "https://docs.netbird.io"

	// brandName labels the connection switch. Not translated: it is the
	// product's name in every locale.
	brandName = "NetBird"
)

// TrayServices bundles the services the tray menu needs, grouped so NewTray
// stays under the linter's parameter-count threshold.
type TrayServices struct {
	Connection      *services.Connection
	Settings        *services.Settings
	Profiles        *services.Profiles
	Networks        *services.Networks
	DaemonFeed      *services.DaemonFeed
	Notifier        *Notifier
	Update          *services.Update
	ProfileSwitcher *services.ProfileSwitcher
	WindowManager   *services.WindowManager
	// Session is bound to authsession directly because the services wrapper
	// only re-exposes the React subset.
	Session     *authsession.Session
	Localizer   *Localizer
	Preferences *preferences.Store
}

type Tray struct {
	app    *application.App
	tray   *application.SystemTray
	window *application.WebviewWindow
	svc    TrayServices
	// panelDark reports whether the desktop panel uses a dark scheme, so
	// iconForState can pick the black vs white mono tray icon on Linux. Set
	// by startTrayTheme (Linux only); nil elsewhere, where panelIsDark falls
	// back to its default.
	panelDark func() bool
	loc       *Localizer

	// menu and the *Item/*Submenu fields below are reassigned by buildMenu
	// on every relayout — touch them only with menuMu held. Exceptions:
	// the connection switch's OnClick closure captures its own item, and
	// refreshSessionExpiresLabel snapshots its item under menuMu.
	menu       *application.Menu
	statusItem *application.MenuItem
	// sessionExpiresItem shows the SSO deadline as a remaining-time label,
	// repainted by a 30s ticker.
	sessionExpiresItem *application.MenuItem
	// connectItem is the connection row: one switch that states the current
	// state instead of a Connect/Disconnect pair that swap places. On macOS an
	// AppKit switch is grafted onto it (see tray_switch_darwin.h); elsewhere it
	// is the checkbox Wails draws.
	connectItem        *application.MenuItem
	exitNodeItem       *application.MenuItem
	exitNodeSubmenu    *application.Menu
	profileSubmenu     *application.Menu
	profileSubmenuItem *application.MenuItem
	profileEmailItem   *application.MenuItem
	settingsItem       *application.MenuItem
	daemonVersionItem  *application.MenuItem

	updater *trayUpdater

	// statusMu guards the daemon-status core mirrored on the tray. One mutex
	// covers these fields because applyStatus writes them together on every
	// Status push and the menu painters read them.
	statusMu          sync.Mutex
	connected         bool
	lastStatus        string
	lastDaemonVersion string
	// lastNetworksRevision is the daemon's routed-networks revision; a bump (or
	// a connect/disconnect transition) gates the refreshExitNodes re-fetch so
	// ListNetworks runs only when routes change. The peer-status route list
	// can't substitute: it carries only actively-routed routes, not candidate
	// exit nodes.
	lastNetworksRevision uint64
	// pendingConnectLogin is set when handleConnect fires an Up on an idle
	// daemon. The daemon flips to NeedsLogin if the peer is SSO-tracked with
	// no cached token; applyStatus consumes the flag on that transition to
	// open the browser-login flow, saving a second Connect click.
	// Profile-switch reconnects are handled separately by
	// DaemonFeed.statusStreamLoop.
	pendingConnectLogin bool

	// sessionMu guards the cached SSO deadline used by the session row.
	// Independent of statusMu so the 30s ticker reader and the Status-push
	// writer don't block each other.
	sessionMu        sync.Mutex
	sessionExpiresAt time.Time

	// profileMu guards the profile-domain state (active identity, the
	// notifications gate, the in-flight switch cancel). Independent of
	// statusMu so a long-running switch holding switchCancel doesn't block a
	// Status-push reader of t.connected.
	profileMu            sync.Mutex
	activeProfile        string
	activeUsername       string
	notificationsEnabled bool
	switchCancel         context.CancelFunc

	// profileLoadMu serializes loadProfiles so the applyStatus refresh can't
	// race the ApplicationStarted seed or the post-switch reload — all
	// manipulate profileSubmenu + SetMenu, which Wails isn't concurrency-safe
	// against.
	profileLoadMu sync.Mutex

	// profilesMu guards the cached profile rows that relayoutMenu repaints
	// into a freshly built Profiles submenu, kept separate from the live
	// submenu so a relayout always has a source to repaint from without
	// re-hitting the daemon.
	profilesMu sync.Mutex
	profiles   []services.Profile
	// profilesLoaded marks the first successful fetch, so loadProfiles can tell
	// "no profiles yet" from "no profiles, and that is the answer" and relayout
	// for the initial fill either way.
	profilesLoaded bool
	profilesUser   string

	// menuMu serialises relayoutMenu (buildMenu + SetMenu) and guards the
	// menu/item-pointer fields above. relayoutMenu is the only post-startup
	// SetMenu call site — a menu snapshot pushed outside the lock could
	// reinstall a stale tree.
	menuMu sync.Mutex

	// pendingRelayout records a rebuild that arrived while the user had the
	// menu open, for onTrayMenuClosed to run once it is out of the way. Atomic
	// rather than menuMu-guarded: relayoutMenu sets it before taking the lock.
	pendingRelayout atomic.Bool

	// exitNodesMu guards the exitNodes row cache so relayoutMenu's read (and
	// the Repaint copy) doesn't contend with status-push readers of statusMu.
	exitNodesMu sync.Mutex
	exitNodes   []exitNodeEntry
	// exitNodesShown is how many rows the submenu was last filled with, which
	// trails exitNodes while a rebuild waits for an open menu to close.
	exitNodesShown int
	// exitNodesRows is that same fill, in order: a click on a natively painted
	// row reports its position, and this is what turns it back into a node.
	exitNodesRows []exitNodeEntry
	// exitNodesRebuildMu serialises the ListNetworks fetch + submenu rebuild +
	// SetMenu cycle so back-to-back Status pushes can't run it concurrently
	// with itself.
	exitNodesRebuildMu sync.Mutex

	// featureMu guards the daemon feature kill switches mirrored on the tray.
	// Fetched at startup and refreshed on every config_changed event (the
	// daemon re-applies MDM policy per engine spawn), so featuresDisabled can
	// grey out menus without polling GetFeatures.
	featureMu       sync.Mutex
	disableProfiles bool
	disableNetworks bool
}

func NewTray(app *application.App, window *application.WebviewWindow, svc TrayServices) *Tray {
	t := &Tray{
		app:                  app,
		window:               window,
		svc:                  svc,
		notificationsEnabled: true,
		// Localizer is constructed by main so the first menu render is already
		// in the right locale — no English flash then re-paint.
		loc: svc.Localizer,
	}
	t.updater = newTrayUpdater(app, t.showMainAt, svc.Update, svc.Notifier, t.loc, func() { t.applyIcon() }, func() { t.relayoutMenu() })
	t.tray = app.SystemTray.New()
	// Seed panel-theme detection before the first paint so the initial icon
	// matches the panel's light/dark scheme (Linux only).
	t.startTrayTheme()
	t.applyIcon()
	t.tray.SetTooltip(t.loc.T("tray.tooltip"))
	// On Linux the SNI hover tooltip rides on the systray label, not
	// SetTooltip (a no-op there); without a label Wails shows the literal
	// "Wails". macOS/Windows are skipped (label paints visible text on
	// macOS; Windows uses SetTooltip above).
	if runtime.GOOS == "linux" {
		t.tray.SetLabel(t.loc.T("tray.tooltip"))
	}
	t.menu = t.buildMenu()
	t.tray.SetMenu(t.menu)
	// Adopts the connection row as an AppKit switch the next time the menu
	// opens, drives the exit-node rows painted behind Wails' back, and reports
	// the menu's own open/close. No-op off macOS.
	startNativeMenu(t.handleConnectionSwitch, t.handleNativeExitNode, t.onTrayMenuOpened, t.onTrayMenuClosed)
	// macOS/Linux give click→menu natively, so bindTrayClick is a no-op there
	// (binding OnClick→OpenMenu on macOS would freeze the tray); Windows has no
	// native left-click handler so it wires one to open the main window, leaving
	// the menu on right-click (see tray_click_*.go). On Linux AttachWindow is
	// skipped — with applySmartDefaults it would pop the window alongside the
	// menu (e.g. GNOME Shell AppIndicator).
	bindTrayClick(t)

	app.Event.On(services.EventStatusSnapshot, t.onStatusEvent)
	app.Event.On(services.EventDaemonNotification, t.onSystemEvent)
	// Refresh the Profiles submenu on ProfileSwitcher's change event. A
	// switch on an idle daemon drives no status transition, so without this
	// hook a React-initiated switch leaves the tray's submenu stale.
	app.Event.On(services.EventProfileChanged, func(*application.CustomEvent) {
		go t.loadProfiles()
	})
	// Defer the first profile load until the menu impl is live — Menu.Update()
	// short-circuits while app.running is false, and AppKit's main queue isn't
	// ready earlier (see d23ef34 InvokeSync nil-deref).
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go t.loadProfiles()
		go t.refreshRestrictions()
		go t.runSessionExpiryTicker()
		// Category registration must run after the notifications service
		// Startup populates appName/registry path on Windows; before app.Run()
		// the category lookup silently falls back to a plain notification.
		t.registerSessionWarningCategory()
	})

	t.loc.Watch(func(i18n.LanguageCode) { t.applyLanguage() })

	go t.loadConfig()
	return t
}

// ShowWindow brings the main window forward — used by SIGUSR1 / Windows event.
// Show() alone is not enough on macOS (makeKeyAndOrderFront skips activation,
// so the window pops up behind the active app); Focus() additionally calls
// activateIgnoringOtherApps:YES on macOS and SetForegroundWindow on Windows.
func (t *Tray) ShowWindow() {
	// An install supersedes every other flow, so check it before BrowserLogin.
	if w := t.svc.WindowManager.InstallProgressWindow(); w != nil {
		w.Show()
		w.Focus()
		return
	}
	if w := t.svc.WindowManager.BrowserLoginWindow(); w != nil {
		w.Show()
		w.Focus()
		return
	}
	// Route through WindowManager so the main window is centered on first
	// show — minimal WMs (fluxbox, the XEmbed tray path) otherwise drop it in
	// the top-left corner.
	if t.svc.WindowManager != nil {
		t.svc.WindowManager.ShowMain()
		return
	}
	if w := t.mainWindow(); w != nil {
		w.Show()
		w.Focus()
	}
}

func (t *Tray) mainWindow() *application.WebviewWindow {
	if t.svc.WindowManager == nil {
		return t.window
	}
	return t.svc.WindowManager.MainWindow()
}

func (t *Tray) showMainAt(url string) {
	if t.svc.WindowManager != nil {
		t.svc.WindowManager.ShowMainAt(url)
		return
	}
	if w := t.mainWindow(); w != nil {
		w.SetURL(url)
		w.Show()
		w.Focus()
	}
}

func (t *Tray) showMain() {
	if t.svc.WindowManager != nil {
		t.svc.WindowManager.ShowMain()
		return
	}
	if w := t.mainWindow(); w != nil {
		w.Show()
		w.Focus()
	}
}

func (t *Tray) showMainAndEmit(event string) {
	if t.svc.WindowManager != nil {
		t.svc.WindowManager.ShowMainAndEmit(event)
		return
	}
	t.showMain()
	t.app.Event.Emit(event)
}

// applyLanguage re-renders every translated surface in the Localizer's current
// language. Wails dispatches menu/tray APIs onto the UI thread internally, so
// calling them from the Localizer's background goroutine is safe; profileLoadMu
// prevents loadProfiles from racing the rebuild.
func (t *Tray) applyLanguage() {
	t.tray.SetTooltip(t.loc.T("tray.tooltip"))
	// Mirror the Linux label fix from NewTray (the SNI tooltip rides on the
	// label).
	if runtime.GOOS == "linux" {
		t.tray.SetLabel(t.loc.T("tray.tooltip"))
	}
	t.relayoutMenu()
}

// relayoutMenu rebuilds the entire tray menu, repaints the cached
// status/session/profile/exit-node state into the fresh items, and pushes the
// whole tree with a single SetMenu.
//
// A full rebuild is required whenever the *rows* change, because on KDE/Plasma
// the StatusNotifierItem host caches a submenu's layout on first open
// (GetLayout for that submenu id) and never re-fetches it on a
// LayoutUpdated(parent=0) signal — so Clear()+Add() into the same container
// froze both the visible rows and the click→id mapping, and stale ids no-op'd.
// buildMenu allocates a fresh submenu container id each time, which Plasma
// treats as unseen and re-queries (confirmed via dbus-monitor). This also
// covers the darwin detached-NSMenu workaround, since it rebuilds the whole
// tree against the cached top-level pointer.
//
// A rebuild is the wrong tool for a state change though: it discards the menu
// the user may have open (see refreshMenuState), so callers that only move
// state — a status push, a connect, a disconnect — go through refreshMenuState
// instead, and callers whose row set is unchanged skip both.
//
// Rows come from the profilesMu/exitNodes caches, so it never re-hits the
// daemon or recurses back into loadProfiles.
func (t *Tray) relayoutMenu() {
	// Not while the user is holding the menu open. A rebuild hands the tray a
	// different native menu, and on macOS the one on screen is the one AppKit
	// was given when it was clicked open: it would stay up but stop following
	// the daemon, since every painter now points at the items of a tree nobody
	// can see. Exit nodes appearing on connect and the profile list reloading
	// both land in exactly that window, so the rebuild waits for the menu to
	// close (see onTrayMenuClosed) while paintMenuState keeps the visible rows
	// current. Off macOS trayMenuIsOpen is always false — the menu is never
	// live there either way.
	if trayMenuIsOpen() {
		t.pendingRelayout.Store(true)
		return
	}

	t.menuMu.Lock()
	defer t.menuMu.Unlock()

	t.menu = t.buildMenu()
	t.paintMenuState(false)

	// buildMenu recreated empty submenus, so repaint both from their caches
	// before SetMenu. Neither fill re-fetches. Do NOT re-take
	// exitNodesRebuildMu here — refreshExitNodes already holds it when it
	// calls relayoutMenu.
	t.fillExitNodeSubmenu(t.exitNodeEntries())
	t.fillProfileSubmenu()

	// Single push of the whole tree: on Linux one LayoutUpdated with fresh
	// container ids; on darwin an NSMenu rebuild against the cached pointer.
	t.tray.SetMenu(t.menu)
}

// onTrayMenuOpened repaints the rows from the daemon's state as the menu opens.
// The rows come out of a Wails relayout carrying whatever the last paint left
// on them, and this is the cheap moment to make sure what the user is about to
// look at is the truth — a push that went missing cannot then sit there wrong
// until the next status change.
func (t *Tray) onTrayMenuOpened() {
	t.refreshMenuState()
}

// onTrayMenuClosed applies what an open menu held back: the deferred rebuild,
// and a refresh that puts Wails back in sync with what the native painter wrote
// past. The loop catches a rebuild requested while this one ran, and stops if
// the user reopens the menu.
func (t *Tray) onTrayMenuClosed() {
	t.applyIcon()
	relaid := false
	for !trayMenuIsOpen() && t.pendingRelayout.Swap(false) {
		t.relayoutMenu()
		relaid = true
	}
	if !relaid {
		t.refreshMenuState()
	}
}

// refreshMenuState repaints the rows that carry daemon state. Where the rows
// can be written where they stand the paint lands in a menu the user already
// has open; elsewhere only a full relayout reaches them (see relayoutMenu).
func (t *Tray) refreshMenuState() {
	if !livePaintMenu() {
		t.relayoutMenu()
		return
	}
	t.menuMu.Lock()
	defer t.menuMu.Unlock()
	t.paintMenuState(true)
}

// paintMenuState writes the cached daemon state onto the menu rows. Callers
// must hold menuMu.
//
// live is true when the items are already installed in a menu the platform may
// be showing, and false during a relayout, where buildMenu has just allocated
// them and the platform layer attaches on the trailing SetMenu.
func (t *Tray) paintMenuState(live bool) {
	state := t.menuStateSnapshot()
	p := t.painter(live)

	p.connection(state.connectionLabel, state.switchOn, state.switchEnabled)
	p.row(rowStatus, state.statusLabel, statusRowEnabled(), false, state.statusBitmap)
	p.row(rowSession, state.sessionLabel, true, state.sessionLabel == "", nil)
	p.row(rowExitNode, "", state.exitNodesEnabled, false, nil)
	p.row(rowSettings, "", state.settingsEnabled, false, nil)
	p.row(rowProfiles, "", state.profilesEnabled, false, nil)
	p.submenuRows(state)
}

// menuRow names the rows the tray paints. Both painters address a row by this
// name; the invisible marker one of them needs to find it is an implementation
// detail of the AppKit side (see rowMarker).
type menuRow int

const (
	rowConnection menuRow = iota
	rowStatus
	rowSession
	rowExitNode
	rowSettings
	rowProfiles
)

// markLabel appends the marker that makes a row findable from AppKit. Every
// writer of a row's label goes through it, or the row drops out of the native
// painter's reach.
func markLabel(row menuRow, label string) string { return label + rowMarker(row) }

// menuPainter writes state onto the menu. Which implementation runs is a
// moment-by-moment choice, not a platform one: the AppKit painter takes over
// while a menu is open, because Wails' setters cannot land then (see
// tray_native_menu_darwin.go).
type menuPainter interface {
	connection(label string, on, enabled bool)
	row(row menuRow, label string, enabled, hidden bool, bitmap []byte)
	exitNodes(labels []string)
	trayIcon(icon, dark []byte)
	// submenuRows paints what only Wails can reach: the rows nested inside the
	// submenus. Nothing in there moves while the menu is open.
	submenuRows(state menuState)
}

// painter picks the one that can reach the menu right now. live is false during
// a relayout, where the items are freshly built and not yet installed, so Wails
// is the only one that can carry state to them.
func (t *Tray) painter(live bool) menuPainter {
	// nativeMenu, not just an open menu: Windows knows when its popup is up
	// too, but has no native painter behind it — picking the stub there threw
	// away every repaint while the menu was open, the tray icon included.
	if live && nativeMenu() && trayMenuIsOpen() {
		return nativeMenuPainter{}
	}
	return wailsMenuPainter{tray: t, live: live}
}

// rowItem maps a row to the item buildMenu created for it. Callers must hold
// menuMu. A row the platform does not have — the status row where the
// connection row is a switch — has no item, and painting it is a no-op.
func (t *Tray) rowItem(row menuRow) *application.MenuItem {
	switch row {
	case rowConnection:
		return t.connectItem
	case rowStatus:
		return t.statusItem
	case rowSession:
		return t.sessionExpiresItem
	case rowExitNode:
		return t.exitNodeItem
	case rowSettings:
		return t.settingsItem
	case rowProfiles:
		return t.profileSubmenuItem
	}
	return nil
}

// wailsMenuPainter writes state through Wails, which owns the rows whenever the
// menu is closed — and off macOS, always.
type wailsMenuPainter struct {
	tray *Tray
	// live is false during a relayout: the items have no platform impl yet, so
	// the status bitmap is recorded on the item and rides the trailing SetMenu
	// instead of being pushed at AppKit directly.
	live bool
}

func (p wailsMenuPainter) connection(label string, _, enabled bool) {
	item := p.tray.connectItem
	if item == nil {
		return
	}
	item.SetLabel(markLabel(rowConnection, label))
	item.SetEnabled(enabled)
}

func (p wailsMenuPainter) row(row menuRow, label string, enabled, hidden bool, bitmap []byte) {
	item := p.tray.rowItem(row)
	if item == nil {
		return
	}
	// An empty label leaves the row's title alone: the fixed rows carry the one
	// buildMenu gave them.
	if label != "" {
		item.SetLabel(markLabel(row, label))
	}
	item.SetEnabled(enabled)
	item.SetHidden(hidden)
	if bitmap != nil {
		p.tray.applyStatusIndicator(item, bitmap, p.live)
	}
}

// exitNodes does nothing here: off the AppKit path the rows arrive with the
// rebuild relayoutMenu runs. Filling the submenu in place instead would freeze
// it on the dbusmenu hosts — see relayoutMenu.
func (wailsMenuPainter) exitNodes([]string) {}

func (p wailsMenuPainter) submenuRows(state menuState) {
	if state.daemonVersionLabel != "" && p.tray.daemonVersionItem != nil {
		p.tray.daemonVersionItem.SetLabel(state.daemonVersionLabel)
	}
	if p.tray.updater != nil {
		p.tray.updater.applyLanguage()
	}
}

// menuState is the daemon state the rows show, gathered once so the two
// painters below cannot drift apart.
type menuState struct {
	// connectionLabel is the connection row's own text: the status where that
	// row is a switch, the fixed label where it is a checkbox.
	connectionLabel string
	statusLabel     string
	statusBitmap    []byte
	// sessionLabel is empty when no SSO deadline is known, which is also when
	// the row is hidden.
	sessionLabel       string
	switchOn           bool
	switchEnabled      bool
	exitNodesEnabled   bool
	settingsEnabled    bool
	profilesEnabled    bool
	daemonVersionLabel string
}

func (t *Tray) menuStateSnapshot() menuState {
	t.statusMu.Lock()
	connected := t.connected
	lastStatus := t.lastStatus
	daemonVersion := t.lastDaemonVersion
	t.statusMu.Unlock()

	t.sessionMu.Lock()
	sessionDeadline := t.sessionExpiresAt
	t.sessionMu.Unlock()

	disableProfiles, disableNetworks := t.featuresDisabled()

	daemonUnavailable := strings.EqualFold(lastStatus, services.StatusDaemonUnavailable)
	connecting := strings.EqualFold(lastStatus, services.StatusConnecting)

	// On mid-connect too: the switch reads "on" from the moment it is flipped,
	// and flipping it back aborts. It stays live in the NeedsLogin states — the
	// flip drives the SSO re-auth flow there — and is greyed only when the
	// daemon is gone and it would be a no-op.
	switchOn := connected || connecting

	state := menuState{
		connectionLabel: t.connectionRowLabel(lastStatus, switchOn),
		statusLabel:     t.loc.StatusLabel(lastStatus),
		statusBitmap:    statusIndicatorBitmap(lastStatus),
		switchOn:        switchOn,
		switchEnabled:   !daemonUnavailable,
		// The row count is the one the menu is actually showing, not the cache:
		// the two part company while a rebuild waits for the menu to close, and
		// enabling the row then would open an empty submenu.
		exitNodesEnabled: connected && t.exitNodesInMenu() > 0 && !disableNetworks,
		settingsEnabled:  !daemonUnavailable,
		profilesEnabled:  !daemonUnavailable && !disableProfiles,
	}
	if !sessionDeadline.IsZero() {
		state.sessionLabel = t.sessionRowLabel(sessionDeadline)
	}
	if daemonVersion != "" {
		state.daemonVersionLabel = t.loc.T("tray.menu.daemonVersion", "version", daemonVersion)
	}
	return state
}

func (t *Tray) buildMenu() *application.Menu {
	menu := application.NewMenu()

	// The status row exists only where the connection row cannot carry the
	// state itself. With a switch it can — its label is the status — and a
	// second row repeating it, appearing and vanishing under the pointer as the
	// connection moves, is noise. Dropping it also takes the menu's only image
	// with it: one image anywhere in an NSMenu indents every title in it behind
	// a gutter, which is what pushed the whole menu to the right.
	//
	// Enabled state is platform-dependent (see statusRowEnabled): Windows keeps
	// it enabled because the disabled mask would desaturate the coloured status
	// dot; macOS/Linux disable it so the greyed label signals it isn't
	// clickable.
	if !nativeMenu() {
		t.statusItem = menu.Add(markLabel(rowStatus, t.loc.T("tray.status.disconnected"))).
			SetEnabled(statusRowEnabled()).
			SetBitmap(iconMenuDotIdle)

		menu.AddSeparator()
	}

	// One switch row rather than a Connect/Disconnect pair that hide each
	// other: the row keeps its place and its label and only its state moves,
	// which is what lets a menu that is already open follow the connection
	// instead of restructuring under the cursor (see refreshMenuState).
	//
	// On macOS the row becomes a real AppKit switch, adopted through the marker
	// in its label (see tray_native_menu_darwin.go); the checkbox below is what
	// it is grafted onto, and what the other platforms keep. The OnClick
	// closure captures the local item because t.connectItem is menuMu-guarded
	// and must not be read from the click goroutine.
	connectLabel := markLabel(rowConnection, t.connectionRowLabel(t.currentStatus(), t.connectedNow()))
	connectItem := menu.Add(connectLabel)
	connectItem.OnClick(func(*application.Context) { t.handleToggleConnection() })
	t.connectItem = connectItem

	menu.AddSeparator()

	// Populated asynchronously once the app has started — Menu.Update() is a
	// no-op before app.running is true, so the initial fill is gated on the
	// ApplicationStarted hook.
	profilesLabel := markLabel(rowProfiles, t.loc.T("tray.menu.profiles"))
	t.profileSubmenu = menu.AddSubmenu(profilesLabel)
	// AddSubmenu returns the child *Menu, so retrieve the parent *MenuItem via
	// FindByLabel.
	t.profileSubmenuItem = menu.FindByLabel(profilesLabel)
	t.profileEmailItem = menu.Add("").SetEnabled(false)
	t.profileEmailItem.SetHidden(true)
	// Click opens the SessionExpiration window so the user can extend ahead of
	// the daemon's T-FinalWarningLead auto-prompt.
	t.sessionExpiresItem = menu.Add(markLabel(rowSession, "")).
		OnClick(func(*application.Context) { t.openSessionExtendFlow() })
	t.sessionExpiresItem.SetHidden(true)

	menu.AddSeparator()
	// Accelerators on the Settings/Quit entries below are a no-op on Windows in
	// Wails v3 alpha.95 (impl commented out in menuitem_windows.go); still set
	// for forward compatibility. macOS/GTK render and fire them.
	menu.Add(t.loc.T("tray.menu.open")).OnClick(func(*application.Context) { t.ShowWindow() })

	menu.AddSeparator()

	// exitNodeSubmenu hosts one row per peer advertising a default route
	// (0.0.0.0/0 or ::/0). FindByLabel grabs the parent so applyStatus can flip
	// its enabled state independently of the children.
	exitNodeLabel := markLabel(rowExitNode, t.loc.T("tray.menu.exitNode"))
	t.exitNodeSubmenu = menu.AddSubmenu(exitNodeLabel)
	t.exitNodeItem = menu.FindByLabel(exitNodeLabel)
	t.exitNodeItem.SetEnabled(false)

	menu.AddSeparator()

	// The label's trailing ellipsis follows the macOS HIG convention for items
	// that open a window.
	t.settingsItem = menu.Add(markLabel(rowSettings, t.loc.T("tray.menu.settings"))).
		SetAccelerator("CmdOrCtrl+,").
		OnClick(func(*application.Context) { t.svc.WindowManager.OpenSettings("") })

	aboutLabel := menuLabel(t.loc.T("tray.menu.about"))
	about := menu.AddSubmenu(aboutLabel)
	about.Add(t.loc.T("tray.menu.github")).OnClick(func(*application.Context) {
		_ = t.app.Browser.OpenURL(urlGitHubRepo)
	})
	about.Add(t.loc.T("tray.menu.documentation")).OnClick(func(*application.Context) {
		_ = t.app.Browser.OpenURL(urlDocs)
	})
	about.Add(t.loc.T("tray.menu.troubleshoot")).OnClick(func(*application.Context) {
		t.svc.WindowManager.OpenSettings("troubleshooting")
	})
	about.AddSeparator()
	about.Add(t.loc.T("tray.menu.guiVersion", "version", version.NetbirdVersion())).SetEnabled(false)
	t.daemonVersionItem = about.Add(t.loc.T("tray.menu.daemonVersion", "version", t.loc.T("tray.menu.versionUnknown"))).SetEnabled(false)
	// trayUpdater rewrites the label between downloadLatest (opt-in) and
	// installVersion (enforced) and drives the click.
	updateItem := about.Add(t.loc.T("tray.menu.downloadLatest")).
		OnClick(func(*application.Context) { t.updater.handleClick() })
	updateItem.SetHidden(true)
	t.updater.attach(updateItem)

	menu.AddSeparator()
	menu.Add(t.loc.T("tray.menu.quit")).
		SetAccelerator("CmdOrCtrl+Q").
		OnClick(func(*application.Context) { t.handleQuit() })

	return menu
}

func (t *Tray) handleQuit() {
	services.BeginShutdown()
	t.profileMu.Lock()
	if t.switchCancel != nil {
		t.switchCancel()
		t.switchCancel = nil
	}
	t.profileMu.Unlock()
	t.svc.DaemonFeed.CancelProfileSwitch()

	if t.svc.Preferences == nil || !t.svc.Preferences.Get().KeepConnectedOnQuit {
		ctx, cancel := context.WithTimeout(context.Background(), quitDownTimeout)
		defer cancel()
		if err := t.svc.Connection.Down(ctx); err != nil {
			log.Errorf("disconnect on quit: %v", err)
		}
	}
	t.app.Quit()
}

// handleToggleConnection drives the connection row where it is a plain item:
// the label says what the click does, and the cached daemon state says which of
// the two it is. Switching off doubles as the Connecting abort path.
func (t *Tray) handleToggleConnection() {
	t.statusMu.Lock()
	status := t.lastStatus
	connected := t.connected
	t.statusMu.Unlock()

	if connected || strings.EqualFold(status, services.StatusConnecting) {
		t.handleDisconnect()
		return
	}
	t.handleConnect(status)
}

// connectionRowLabel is the status next to a switch, which states the
// connection and offers the control on one line, and a verb where there is no
// switch, because there the status row above already does the stating.
func (t *Tray) connectionRowLabel(status string, on bool) string {
	if nativeMenu() {
		return t.loc.StatusLabel(status)
	}
	if on {
		return t.loc.T("quickActions.disconnect")
	}
	return t.loc.T("quickActions.connect")
}

// connectedNow reports whether the connection is up or coming up, which is what
// the connection row's verb turns on.
func (t *Tray) connectedNow() bool {
	t.statusMu.Lock()
	status := t.lastStatus
	connected := t.connected
	t.statusMu.Unlock()
	return connected || strings.EqualFold(status, services.StatusConnecting)
}

// currentStatus snapshots the last status the daemon pushed.
func (t *Tray) currentStatus() string {
	t.statusMu.Lock()
	defer t.statusMu.Unlock()
	return t.lastStatus
}

// handleConnectionSwitch drives the AppKit switch row on macOS. Unlike the
// checkbox path above, the state the user asked for is the one the control now
// shows, so it is taken at face value: switching off doubles as the Connecting
// abort, and the daemon's next push repaints the switch through
// refreshMenuState if the request does not land.
func (t *Tray) handleConnectionSwitch(on bool) {
	if !on {
		t.handleDisconnect()
		return
	}
	t.statusMu.Lock()
	status := t.lastStatus
	t.statusMu.Unlock()
	t.handleConnect(status)
}

// handleConnect brings the tunnel up from the status the switch was flipped in.
func (t *Tray) handleConnect(status string) {
	// NeedsLogin/SessionExpired/LoginFailed won't honor a plain Up RPC — they
	// need the Login → WaitSSOLogin → Up sequence. Emit EventTriggerLogin so
	// the React startLogin() (which owns the BrowserLogin popup) drives it;
	// the WindowManager materialises a hidden main webview when none is live,
	// so only the popup shows.
	if strings.EqualFold(status, services.StatusNeedsLogin) ||
		strings.EqualFold(status, services.StatusSessionExpired) ||
		strings.EqualFold(status, services.StatusLoginFailed) {
		t.app.Event.Emit(services.EventTriggerLogin)
		return
	}
	// Arm the SSO auto-handoff: Up() is async and the daemon may flip to
	// NeedsLogin on an SSO peer with no cached token. applyStatus consumes the
	// flag on that transition to trigger browser-login without a second flip of
	// the switch, and clears it on any terminal state.
	t.statusMu.Lock()
	t.pendingConnectLogin = true
	t.statusMu.Unlock()
	go func() {
		if err := t.svc.Connection.Up(context.Background(), services.UpParams{}); err != nil {
			log.Errorf("connect: %v", err)
			t.notifyError(t.loc.T("notify.error.connect"))
			t.statusMu.Lock()
			t.pendingConnectLogin = false
			t.statusMu.Unlock()
			// Roll the switch back to what the daemon actually reports.
			t.refreshMenuState()
		}
	}()
}

// handleDisconnect aborts any in-flight profile switch before sending Down —
// otherwise the switcher's queued Up would reconnect right after, making the
// flip a no-op. Also clears Peers' optimistic-Connecting guard so the daemon's
// Idle push paints through instead of being swallowed by the suppression filter.
func (t *Tray) handleDisconnect() {
	t.profileMu.Lock()
	if t.switchCancel != nil {
		t.switchCancel()
		t.switchCancel = nil
	}
	t.profileMu.Unlock()
	t.svc.DaemonFeed.CancelProfileSwitch()
	go func() {
		if err := t.svc.Connection.Down(context.Background()); err != nil {
			log.Errorf("disconnect: %v", err)
			t.notifyError(t.loc.T("notify.error.disconnect"))
			t.refreshMenuState()
		}
	}()
}

// notify wraps the Wails notification service with the tray's standard
// id-prefix scheme and swallows errors (notifications are best-effort).
func (t *Tray) notify(title, body, id string) {
	if t.svc.Notifier == nil {
		return
	}
	_ = safeSendNotification(t.svc.Notifier.SendNotification, title, notifications.NotificationOptions{
		ID:    id,
		Title: title,
		Body:  body,
	})
}

// notifyError fires a generic "Error" notification for tray-driven action
// failures. Each tray click site already logs the underlying error; this
// adds the user-visible toast.
func (t *Tray) notifyError(message string) {
	t.notify(t.loc.T("notify.error.title"), message, notifyIDTrayError)
}
