//go:build windows

package main

// Панель и значок в трее. Чистый Win32 через syscall.

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows/registry"
)

var (
	u32   = syscall.NewLazyDLL("user32.dll")
	g32   = syscall.NewLazyDLL("gdi32.dll")
	sh32  = syscall.NewLazyDLL("shell32.dll")
	k32ui = syscall.NewLazyDLL("kernel32.dll")
	dwm   = syscall.NewLazyDLL("dwmapi.dll")

	pRegisterClassEx       = u32.NewProc("RegisterClassExW")
	pCreateWindowEx        = u32.NewProc("CreateWindowExW")
	pDefWindowProc         = u32.NewProc("DefWindowProcW")
	pGetMessage            = u32.NewProc("GetMessageW")
	pTranslateMessage      = u32.NewProc("TranslateMessage")
	pDispatchMessage       = u32.NewProc("DispatchMessageW")
	pPostMessage           = u32.NewProc("PostMessageW")
	pPostQuitMessage       = u32.NewProc("PostQuitMessage")
	pShowWindow            = u32.NewProc("ShowWindow")
	pSetTimer              = u32.NewProc("SetTimer")
	pInvalidateRect        = u32.NewProc("InvalidateRect")
	pBeginPaint            = u32.NewProc("BeginPaint")
	pEndPaint              = u32.NewProc("EndPaint")
	pDrawText              = u32.NewProc("DrawTextW")
	pLoadCursor            = u32.NewProc("LoadCursorW")
	pCreatePopupMenu       = u32.NewProc("CreatePopupMenu")
	pAppendMenu            = u32.NewProc("AppendMenuW")
	pTrackPopupMenu        = u32.NewProc("TrackPopupMenu")
	pDestroyMenu           = u32.NewProc("DestroyMenu")
	pGetCursorPos          = u32.NewProc("GetCursorPos")
	pScreenToClient        = u32.NewProc("ScreenToClient")
	pSetForeground         = u32.NewProc("SetForegroundWindow")
	pCreateIconInd         = u32.NewProc("CreateIconIndirect")
	pAdjustWindowRect      = u32.NewProc("AdjustWindowRect")
	pRegisterWindowMsg     = u32.NewProc("RegisterWindowMessageW")
	pGetSystemMetrics      = u32.NewProc("GetSystemMetrics")
	pDestroyWindow         = u32.NewProc("DestroyWindow")
	pSetProcessDPIAware    = u32.NewProc("SetProcessDPIAware")
	pGetDpiForSystem       = u32.NewProc("GetDpiForSystem")
	pGetModuleHandle       = k32ui.NewProc("GetModuleHandleW")
	pShellNotifyIcon       = sh32.NewProc("Shell_NotifyIconW")
	pCreateSolidBrush      = g32.NewProc("CreateSolidBrush")
	pDeleteObject          = g32.NewProc("DeleteObject")
	pSelectObject          = g32.NewProc("SelectObject")
	pSetBkMode             = g32.NewProc("SetBkMode")
	pSetTextColor          = g32.NewProc("SetTextColor")
	pCreateFont            = g32.NewProc("CreateFontW")
	pCreateCompatDC        = g32.NewProc("CreateCompatibleDC")
	pDeleteDC              = g32.NewProc("DeleteDC")
	pCreateBitmap          = g32.NewProc("CreateBitmap")
	pCreateDIBSection      = g32.NewProc("CreateDIBSection")
	pStretchBlt            = g32.NewProc("StretchBlt")
	pSetStretchBltMode     = g32.NewProc("SetStretchBltMode")
	pGdiFlush              = g32.NewProc("GdiFlush")
	pDwmSetWindowAttribute = dwm.NewProc("DwmSetWindowAttribute")
	pGetClientRect         = u32.NewProc("GetClientRect")
	pFillRect              = u32.NewProc("FillRect")
)

const (
	wmDestroy     = 0x0002
	wmPaint       = 0x000F
	wmClose       = 0x0010
	wmEraseBkgnd  = 0x0014
	wmTimer       = 0x0113
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmUser        = 0x0400
	wmTray        = wmUser + 1
	wmQuitUI      = wmUser + 2
	wmShowPanel   = wmUser + 4

	swHide = 0
	swShow = 5

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2
	nifMsg    = 1
	nifIcon   = 2
	nifTip    = 4
	nifInfo   = 0x10

	mfString    = 0
	mfChecked   = 8
	mfPopup     = 0x10
	mfSeparator = 0x800

	tpmReturnCmd = 0x100
	tpmRightBtn  = 0x2
	tpmBottomAl  = 0x20

	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsMinimizeBox = 0x00020000
	wsMaximizeBox = 0x00010000
	wsThickFrame  = 0x00040000

	wmSize          = 0x0005
	wmGetMinMaxInfo = 0x0024
)

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type paintStruct struct {
	Hdc         uintptr
	FErase      int32
	Paint       rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

type bitmapInfoHeader struct {
	Size          uint32
	Width, Height int32
	Planes, Bits  uint16
	Compression   uint32
	SizeImage     uint32
	XPels, YPels  int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type uiCtl struct {
	getState func() uiState
	acts     chan action
	hwnd     uintptr
	icons    [3]uintptr
	iconIdx  int
	mu       sync.Mutex
	st       uiState
	menuAct  map[uint16]action
	tip      string
	ready    chan struct{}
	panelVis atomic.Bool
	scale    int32
	hits     []hitBox
	frame    int
	minW     int32
	minH     int32
	drawS    int32
	ox, oy   int32

	memDC   uintptr
	dib     uintptr
	oldBmp  uintptr
	cv      *canvas
	inkBr   uintptr
	fonts   [fontCount]uintptr
	taskMsg uint32
}

var theUI *uiCtl

// ptrFrom превращает указатель из сообщения окна в unsafe.Pointer.
func ptrFrom(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

func u16(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func colorRef(c uint32) uintptr { return uintptr((c>>16)&0xff | (c>>8&0xff)<<8 | (c&0xff)<<16) }

// startUI запускает интерфейс в отдельном потоке.
func startUI(getState func() uiState, showPanel bool) *uiCtl {
	c := &uiCtl{getState: getState, acts: make(chan action, 16), ready: make(chan struct{}), menuAct: map[uint16]action{}, scale: 1}
	theUI = c
	go c.loop(showPanel)
	<-c.ready
	return c
}

func (c *uiCtl) loop(showPanel bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()
	pSetProcessDPIAware.Call()
	if pGetDpiForSystem.Find() == nil {
		if dpi, _, _ := pGetDpiForSystem.Call(); dpi >= 168 {
			c.scale = 2
		}
	}

	hi, _, _ := pGetModuleHandle.Call(0)
	cur, _, _ := pLoadCursor.Call(0, 32512)
	bg, _, _ := pCreateSolidBrush.Call(colorRef(cInk))
	cls := u16(panelClass)
	c.icons = [3]uintptr{makeTrayIcon(0), makeTrayIcon(1), makeTrayIcon(2)}
	appIcon, _, _ := u32.NewProc("LoadIconW").Call(hi, uintptr(unsafe.Pointer(u16("APP"))))
	if appIcon == 0 {
		appIcon = c.icons[0]
	}
	wc := wndClassEx{LpfnWndProc: syscall.NewCallback(wndProc), HInstance: hi, HCursor: cur, HbrBackground: bg,
		LpszClassName: cls, HIcon: appIcon, HIconSm: appIcon}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	c.taskMsg = func() uint32 {
		r, _, _ := pRegisterWindowMsg.Call(uintptr(unsafe.Pointer(u16("TaskbarCreated"))))
		return uint32(r)
	}()

	style := uintptr(wsCaption | wsSysMenu | wsMinimizeBox | wsMaximizeBox | wsThickFrame)
	mr := rect{0, 0, panelW, panelH}
	pAdjustWindowRect.Call(uintptr(unsafe.Pointer(&mr)), style, 0)
	c.minW, c.minH = mr.R-mr.L, mr.B-mr.T
	c.inkBr = bg
	wr := rect{0, 0, panelW * c.scale, panelH * c.scale}
	pAdjustWindowRect.Call(uintptr(unsafe.Pointer(&wr)), style, 0)
	w, h := wr.R-wr.L, wr.B-wr.T
	sw, _, _ := pGetSystemMetrics.Call(16) // SM_CXFULLSCREEN
	sh, _, _ := pGetSystemMetrics.Call(17)
	x, y := int32(sw)-w-24, int32(sh)-h-24
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	c.hwnd, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(u16(appName))), style,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0, 0, hi, 0)
	darkTitle(c.hwnd)
	c.trayAdd()
	pSetTimer.Call(c.hwnd, 1, 120, 0)
	if showPanel {
		c.ShowPanel(true)
	}
	close(c.ready)

	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	c.trayDel()
}

// darkTitle красит заголовок окна в цвета панели (Windows 11).
func darkTitle(hwnd uintptr) {
	if pDwmSetWindowAttribute.Find() != nil {
		return
	}
	set := func(attr uintptr, v uint32) {
		pDwmSetWindowAttribute.Call(hwnd, attr, uintptr(unsafe.Pointer(&v)), 4)
	}
	set(20, 1)
	set(35, uint32(colorRef(cInk)))
	set(34, uint32(colorRef(cPaper)))
	set(36, uint32(colorRef(cPaper)))
}

// makeTrayIcon: 0 — работает, 1 — калибровка, 2 — нет звука.
func makeTrayIcon(state int) uintptr {
	const n = 32
	hdr := bitmapInfoHeader{Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: n, Height: -n, Planes: 1, Bits: 32}
	var bits unsafe.Pointer
	hbm, _, _ := pCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&hdr)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		return 0
	}
	cv := newCanvas(n, n)
	cv.fill(rect{0, 0, n, n}, cInk)
	if state == 1 {
		cv.dots(rect{0, 0, n, n}, cPaper)
		cv.fill(rect{5, 3, 27, 29}, cInk)
	}
	cv.frame(rect{0, 0, n, n}, cPaper)
	g := glyphs["mic"]
	for y, row := range g {
		for x := 0; x < len(row); x++ {
			if row[x] == '#' {
				cv.fill(rect{int32(3 + 2*x), int32(3 + 2*y), int32(5 + 2*x), int32(5 + 2*y)}, cPaper)
			}
		}
	}
	if state == 2 {
		for i := 2; i < n-2; i++ {
			cv.fill(rect{int32(i), int32(n - 1 - i - 1), int32(i + 2), int32(n - 1 - i + 1)}, cPaper)
		}
	}
	px := unsafe.Slice((*uint32)(bits), n*n)
	for i, v := range cv.px {
		px[i] = 0xFF000000 | v
	}
	mask, _, _ := pCreateBitmap.Call(n, n, 1, 1, 0)
	ii := iconInfo{FIcon: 1, HbmMask: mask, HbmColor: hbm}
	h, _, _ := pCreateIconInd.Call(uintptr(unsafe.Pointer(&ii)))
	pDeleteObject.Call(mask)
	pDeleteObject.Call(hbm)
	return h
}

// ---------- трей ----------

func (c *uiCtl) nid(flags uint32) notifyIconData {
	var n notifyIconData
	n.CbSize = uint32(unsafe.Sizeof(n))
	n.HWnd = c.hwnd
	n.UID = 1
	n.UFlags = flags
	n.UCallbackMessage = wmTray
	n.HIcon = c.icons[c.iconIdx]
	return n
}

func copyU16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

func (c *uiCtl) trayAdd() {
	n := c.nid(nifMsg | nifIcon | nifTip)
	tip := c.tip
	if tip == "" {
		tip = appName
	}
	copyU16(n.SzTip[:], tip)
	pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&n)))
}

func (c *uiCtl) trayDel() {
	n := c.nid(0)
	pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&n)))
}

func (c *uiCtl) trayUpdate(tip string, icon int) {
	c.iconIdx = icon
	n := c.nid(nifIcon | nifTip)
	copyU16(n.SzTip[:], tip)
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&n)))
}

// Balloon показывает уведомление у значка.
func (c *uiCtl) Balloon(title, text string) {
	if c == nil {
		return
	}
	n := c.nid(nifInfo)
	copyU16(n.SzInfoTitle[:], title)
	copyU16(n.SzInfo[:], text)
	n.DwInfoFlags = 1
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&n)))
}

func (c *uiCtl) ShowPanel(show bool) {
	if show {
		pShowWindow.Call(c.hwnd, swShow)
		pSetForeground.Call(c.hwnd)
	} else {
		pShowWindow.Call(c.hwnd, swHide)
	}
	c.panelVis.Store(show)
}

func (c *uiCtl) Close() {
	if c == nil {
		return
	}
	pPostMessage.Call(c.hwnd, wmQuitUI, 0, 0)
}

func (c *uiCtl) send(a action) {
	select {
	case c.acts <- a:
	default:
	}
}

// ---------- окно ----------

func wndProc(hwnd, m, wp, lp uintptr) uintptr {
	c := theUI
	switch uint32(m) {
	case wmPaint:
		c.paint(hwnd)
		return 0
	case wmEraseBkgnd:
		return 1
	case wmSize:
		pInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmGetMinMaxInfo:
		if c != nil && c.minW > 0 {
			mm := (*[10]int32)(ptrFrom(lp)) // MINMAXINFO: ptMinTrackSize = [6],[7]
			mm[6], mm[7] = c.minW, c.minH
		}
		return 0
	case 0x0218: // WM_POWERBROADCAST: выход из сна
		if wp == 7 || wp == 18 {
			c.send(action{Kind: "resume"})
		}
		return 1
	case wmTimer:
		st := c.getState()
		c.mu.Lock()
		c.st = st
		c.mu.Unlock()
		c.frame++
		icon := 0
		switch {
		case !st.Running:
			icon = 2
		case st.Calibrating:
			icon = 1
		}
		tip := appName + " — " + noteOf(st)
		if tip != c.tip || icon != c.iconIdx {
			c.tip = tip
			c.trayUpdate(tip, icon)
		}
		if c.panelVis.Load() {
			pInvalidateRect.Call(hwnd, 0, 0)
		}
		return 0
	case wmLButtonDown:
		s := max(c.drawS, 1)
		p := point{int32(int16(lp&0xFFFF))/s - c.ox, int32(int16((lp>>16)&0xFFFF))/s - c.oy}
		for _, h := range c.hits {
			if h.r.has(p) {
				c.doAction(h.act)
				pInvalidateRect.Call(hwnd, 0, 0)
				break
			}
		}
		return 0
	case wmClose:
		c.ShowPanel(false)
		return 0
	case wmQuitUI:
		pDestroyWindow.Call(hwnd)
		return 0
	case wmShowPanel:
		c.ShowPanel(true)
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	case wmTray:
		switch uint32(lp & 0xFFFF) {
		case wmLButtonUp:
			c.ShowPanel(!c.panelVis.Load())
		case wmRButtonUp:
			c.popup()
		}
		return 0
	}
	if c != nil && c.taskMsg != 0 && uint32(m) == c.taskMsg {
		c.trayAdd() // проводник перезапустился
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, m, wp, lp)
	return r
}

func (c *uiCtl) doAction(a action) {
	switch a.Kind {
	case "hidepanel":
		c.ShowPanel(false)
	case "panel":
		c.ShowPanel(true)
	case "beta":
		c.send(action{Kind: "beta", F: []float64{1.0, 1.5, 2.0}[int(a.F)]})
	default:
		c.send(a)
	}
}

func mkFont(face string, h int, bold bool) uintptr {
	w := 400
	if bold {
		w = 700
	}
	f, _, _ := pCreateFont.Call(uintptr(uint32(-h)), 0, 0, 0, uintptr(w), 0, 0, 0, 1, 0, 0, 3, 0, uintptr(unsafe.Pointer(u16(face))))
	return f
}

func (c *uiCtl) ensureSurface(hdc uintptr, w, h int) bool {
	if c.memDC != 0 && c.cv != nil && c.cv.w == w && c.cv.h == h {
		return true
	}
	hdr := bitmapInfoHeader{Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: int32(w), Height: int32(-h), Planes: 1, Bits: 32}
	var bits unsafe.Pointer
	dib, _, _ := pCreateDIBSection.Call(hdc, uintptr(unsafe.Pointer(&hdr)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if dib == 0 || bits == nil {
		return false
	}
	if c.memDC == 0 {
		c.memDC, _, _ = pCreateCompatDC.Call(hdc)
		pSetBkMode.Call(c.memDC, 1)
	}
	old, _, _ := pSelectObject.Call(c.memDC, dib)
	if c.dib != 0 {
		pDeleteObject.Call(c.dib)
	} else {
		c.oldBmp = old
	}
	c.dib = dib
	c.cv = &canvas{w: w, h: h, px: unsafe.Slice((*uint32)(bits), w*h)}
	if c.fonts[0] != 0 {
		return true
	}
	c.fonts = [fontCount]uintptr{
		mkFont("Courier New", 22, true),
		mkFont("Segoe UI", 16, false),
		mkFont("Segoe UI", 13, false),
		mkFont("Courier New", 14, true),
	}
	return true
}

func (c *uiCtl) paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	var cr rect
	pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	cw, ch := max(cr.R, 1), max(cr.B, 1)
	s := max(1, min(cw/panelW, ch/panelH))
	bw, bh := max(cw/s, panelW), max(ch/s, panelH)
	if !c.ensureSurface(hdc, int(bw), int(bh)) {
		return
	}
	c.drawS = s
	c.mu.Lock()
	st := c.st
	c.mu.Unlock()
	var cp point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&cp)))
	pScreenToClient.Call(hwnd, uintptr(unsafe.Pointer(&cp)))
	cp.X, cp.Y = cp.X/s-c.ox, cp.Y/s-c.oy

	pGdiFlush.Call()
	hits, texts := paintPanel(c.cv, st, cp, c.frame/4%2 == 0)
	c.hits = hits
	c.ox, c.oy = int32(c.cv.ox), int32(c.cv.oy)
	for _, t := range texts {
		col := cPaper
		if t.ink {
			col = cInk
		}
		pSelectObject.Call(c.memDC, c.fonts[t.font])
		pSetTextColor.Call(c.memDC, colorRef(col))
		u, _ := syscall.UTF16FromString(t.s)
		r := rect{t.r.L + c.ox, t.r.T + c.oy, t.r.R + c.ox, t.r.B + c.oy}
		pDrawText.Call(c.memDC, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&r)), uintptr(t.flags))
	}
	pGdiFlush.Call()
	pSetStretchBltMode.Call(hdc, 3) // COLORONCOLOR
	pStretchBlt.Call(hdc, 0, 0, uintptr(bw*s), uintptr(bh*s), c.memDC, 0, 0, uintptr(bw), uintptr(bh), 0x00CC0020)
	if bw*s < cw {
		r := rect{bw * s, 0, cw, ch}
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&r)), c.inkBr)
	}
	if bh*s < ch {
		r := rect{0, bh * s, cw, ch}
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&r)), c.inkBr)
	}
}

// ---------- меню в трее ----------

func (c *uiCtl) popup() {
	st := c.getState()
	menu, _, _ := pCreatePopupMenu.Call()
	defer pDestroyMenu.Call(menu)
	c.menuAct = map[uint16]action{}
	id := uint16(100)
	add := func(m uintptr, label string, a action, flags uintptr) {
		id++
		c.menuAct[id] = a
		pAppendMenu.Call(m, mfString|flags, uintptr(id), uintptr(unsafe.Pointer(u16(label))))
	}
	check := func(on bool) uintptr {
		if on {
			return mfChecked
		}
		return 0
	}
	sep := func(m uintptr) { pAppendMenu.Call(m, mfSeparator, 0, 0) }
	radio := func(title, kind string, opts []string, sel int) {
		m, _, _ := pCreatePopupMenu.Call()
		for i, o := range opts {
			add(m, o, action{Kind: kind, F: float64(i)}, check(i == sel))
		}
		pAppendMenu.Call(menu, mfPopup, m, uintptr(unsafe.Pointer(u16(title))))
	}

	add(menu, L("Открыть панель", "Open panel"), action{Kind: "panel"}, 0)
	sep(menu)
	betaSel := 1
	switch {
	case st.Beta < 1.25:
		betaSel = 0
	case st.Beta > 1.75:
		betaSel = 2
	}
	radio(L("Подавление наводки", "Bleed suppression"), "beta", []string{L("Слабое", "Soft"), L("Обычное", "Normal"), L("Сильное", "Strong")}, betaSel)
	radio(L("Шумоподавление", "Noise reduction"), "noise", []string{L("Выкл", "Off"), L("Слабое", "Light"), L("Среднее", "Medium"), L("Сильное", "Strong"), L("Максимум", "Maximum")}, st.Noise)
	radio(L("Эквалайзер", "Equalizer"), "eq", []string{L("Выкл", "Off"), L("Чёткая речь", "Clear speech"), L("Мягкий", "Soft")}, st.EQ)
	radio(L("Микрофон по кнопке", "Push to talk"), "ptt", []string{L("Всегда включён", "Always on"), L("Удерживать", "Hold"), L("Вкл/выкл", "Toggle")}, st.PTT)
	micSel, exSel := 0, 1
	if st.MicExWant {
		micSel = 1
	}
	if st.ExclWant {
		exSel = 0
	}
	radio(L("Захват микрофона", "Mic capture"), "micexcl", []string{L("Общий", "Shared"), L("Монопольный", "Exclusive")}, micSel)
	radio(L("Вывод в кабель", "Cable output"), "excl", []string{L("Монопольный", "Exclusive"), L("Общий", "Shared")}, exSel)
	add(menu, L("Кнопка микрофона: ", "Mic key: ")+st.PTTKey, action{Kind: "bind"}, 0)
	add(menu, L("Звук при включении микрофона", "Sound on mic toggle"), action{Kind: "cue"}, check(st.Cue))
	add(menu, L("Автогромкость", "Auto gain"), action{Kind: "agc", F: 2}, check(st.AGC))
	sep(menu)

	devMenu := func(title string, flow uint32, kind, cur string, withDefault bool) {
		m, _, _ := pCreatePopupMenu.Call()
		if withDefault {
			add(m, L("По умолчанию в Windows", "Windows default"), action{Kind: kind, S: ""}, check(cur == ""))
		}
		if names, err := deviceNames(flow); err == nil {
			for _, n := range names {
				add(m, n, action{Kind: kind, S: n}, check(cur != "" && containsFold(n, cur)))
			}
		}
		pAppendMenu.Call(menu, mfPopup, m, uintptr(unsafe.Pointer(u16(title))))
	}
	devMenu(L("Микрофон", "Microphone"), wca.ECapture, "mic", st.Mic, true)
	devMenu(L("Наушники", "Headphones"), wca.ERender, "ref", st.Ref, true)
	devMenu(L("Вывод (виртуальный кабель)", "Output (virtual cable)"), wca.ERender, "out", st.Out, false)
	sep(menu)
	add(menu, L("Калибровка", "Calibrate"), action{Kind: "recal"}, 0)
	add(menu, L("Запускать вместе с Windows", "Start with Windows"), action{Kind: "autostart"}, check(autostartOn()))
	add(menu, fmt.Sprintf(L("Сон ПК при тишине (%d мин)", "Sleep on silence (%d min)"), idleSleepMin()), action{Kind: "idlesleep"}, check(idleSleepOn.Load()))
	add(menu, L("Открыть журнал", "Open log"), action{Kind: "log"}, 0)
	switch {
	case st.UpdBusy:
	case st.UpdLatest != "":
		add(menu, fmt.Sprintf(L("Обновить до %s", "Update to %s"), st.UpdLatest), action{Kind: "update"}, 0)
	default:
		add(menu, L("Проверить обновления", "Check for updates"), action{Kind: "checkupd"}, 0)
	}
	sep(menu)
	add(menu, L("Выход", "Quit"), action{Kind: "quit"}, 0)

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForeground.Call(c.hwnd)
	r, _, _ := pTrackPopupMenu.Call(menu, tpmReturnCmd|tpmRightBtn|tpmBottomAl, uintptr(pt.X), uintptr(pt.Y), 0, c.hwnd, 0)
	pPostMessage.Call(c.hwnd, 0, 0, 0)
	if a, ok := c.menuAct[uint16(r)]; ok && r != 0 {
		c.doAction(a)
	}
}

func containsFold(s, sub string) bool {
	return len(sub) > 0 && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	ls, lsub := []rune(lowerASCII(s)), []rune(lowerASCII(sub))
	for i := 0; i+len(lsub) <= len(ls); i++ {
		ok := true
		for j := range lsub {
			if ls[i+j] != lsub[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

func lowerASCII(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}

// ---------- автозапуск ----------

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func autostartOn() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(appName)
	return err == nil
}

func setAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(appName); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(appName, `"`+exe+`" -background`)
}

// migrateAutostart заменяет автозапуск прежней версии (micaec) на hush; true — он был включён.
func migrateAutostart() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	if _, _, err := k.GetStringValue("micaec"); err != nil {
		return false
	}
	k.DeleteValue("micaec")
	setAutostart(true)
	return true
}
