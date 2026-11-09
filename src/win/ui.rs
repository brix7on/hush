use std::cell::RefCell;
use std::ffi::c_void;
use std::sync::atomic::{AtomicBool, Ordering::Relaxed};
use std::sync::mpsc::{channel, Sender};
use std::time::Duration;

use windows::core::{HSTRING, PCWSTR};
use windows::Win32::Foundation::*;
use windows::Win32::Graphics::Dwm::{DwmSetWindowAttribute, DWMWINDOWATTRIBUTE};
use windows::Win32::Graphics::Gdi::*;
use windows::Win32::Media::Audio::{eCapture, eRender, EDataFlow};
use windows::Win32::System::LibraryLoader::GetModuleHandleW;
use windows::Win32::UI::HiDpi::GetDpiForSystem;
use windows::Win32::UI::Shell::*;
use windows::Win32::UI::WindowsAndMessaging::*;

use hush::l;
use hush::panel::*;

use super::dev::{contains_fold, device_names};
use super::state::{Ev, G};
use super::sys::{autostart_on, hwnd_from, Com, PANEL_CLASS, WM_SHOW_PANEL};

const WM_TRAY: u32 = WM_USER + 1;
const WM_QUIT_UI: u32 = WM_USER + 2;

pub type GetState = Box<dyn Fn() -> UiState + Send>;

static PANEL_VIS: AtomicBool = AtomicBool::new(false);

struct Ui {
    get: GetState,
    tx: Sender<Ev>,
    hwnd: HWND,
    icons: [HICON; 3],
    icon_idx: usize,
    tip: String,
    st: UiState,
    hits: Vec<HitBox>,
    frame: u64,
    min_w: i32,
    min_h: i32,
    font_f: f64,
    fonts: [HFONT; FONT_COUNT],
    mem_dc: HDC,
    dib: HBITMAP,
    bits: *mut u32,
    cw: i32,
    ch: i32,
    f: f64,
    ox: i32,
    oy: i32,
    task_msg: u32,
}

thread_local! {
    static UI: RefCell<Option<Ui>> = const { RefCell::new(None) };
}

fn with<R>(f: impl FnOnce(&mut Ui) -> R) -> Option<R> {
    UI.with(|u| u.try_borrow_mut().ok().and_then(|mut g| g.as_mut().map(f)))
}

fn color_ref(c: u32) -> COLORREF {
    COLORREF(((c >> 16) & 0xff) | ((c >> 8) & 0xff) << 8 | (c & 0xff) << 16)
}

fn hwnd() -> HWND {
    hwnd_from(G.ui_hwnd.load(Relaxed))
}

// ---------- для других потоков ----------

pub fn start(get: GetState, tx: Sender<Ev>, show: bool) {
    let (rtx, rrx) = channel();
    std::thread::spawn(move || run(get, tx, show, rtx));
    let _ = rrx.recv_timeout(Duration::from_secs(5));
}

fn nid(h: HWND, flags: NOTIFY_ICON_DATA_FLAGS) -> NOTIFYICONDATAW {
    NOTIFYICONDATAW {
        cbSize: std::mem::size_of::<NOTIFYICONDATAW>() as u32,
        hWnd: h,
        uID: 1,
        uFlags: flags,
        uCallbackMessage: WM_TRAY,
        ..Default::default()
    }
}

fn copy16(dst: &mut [u16], s: &str) {
    let w: Vec<u16> = s.encode_utf16().take(dst.len() - 1).collect();
    dst[..w.len()].copy_from_slice(&w);
    dst[w.len()] = 0;
}

// уведомление у значка
pub fn balloon(title: &str, text: &str) {
    let h = hwnd();
    if h.0.is_null() {
        return;
    }
    let mut n = nid(h, NIF_INFO);
    copy16(&mut n.szInfoTitle, title);
    copy16(&mut n.szInfo, text);
    n.dwInfoFlags = NIIF_INFO;
    unsafe {
        let _ = Shell_NotifyIconW(NIM_MODIFY, &n);
    }
}

pub fn close() {
    let h = hwnd();
    if !h.0.is_null() {
        unsafe {
            let _ = PostMessageW(Some(h), WM_QUIT_UI, WPARAM(0), LPARAM(0));
        }
    }
}

// ---------- поток окна ----------

fn make_icon(state: i32) -> HICON {
    const N: i32 = 32;
    unsafe {
        let mut bi = BITMAPINFO::default();
        bi.bmiHeader = BITMAPINFOHEADER {
            biSize: std::mem::size_of::<BITMAPINFOHEADER>() as u32,
            biWidth: N,
            biHeight: -N,
            biPlanes: 1,
            biBitCount: 32,
            ..Default::default()
        };
        let mut bits: *mut c_void = std::ptr::null_mut();
        let Ok(hbm) = CreateDIBSection(None, &bi, DIB_RGB_COLORS, &mut bits, None, 0) else { return HICON::default() };
        let px = tray_icon_pixels(state);
        let dst = std::slice::from_raw_parts_mut(bits as *mut u32, (N * N) as usize);
        for (d, s) in dst.iter_mut().zip(px) {
            *d = 0xFF00_0000 | s;
        }
        let mask = CreateBitmap(N, N, 1, 1, None);
        let ii = ICONINFO { fIcon: TRUE, xHotspot: 0, yHotspot: 0, hbmMask: mask, hbmColor: hbm };
        let h = CreateIconIndirect(&ii).unwrap_or_default();
        let _ = DeleteObject(mask.into());
        let _ = DeleteObject(hbm.into());
        h
    }
}

// заголовок окна в цветах панели (Windows 11)
fn dark_title(h: HWND) {
    let set = |attr: i32, v: u32| unsafe {
        let _ = DwmSetWindowAttribute(h, DWMWINDOWATTRIBUTE(attr), &v as *const _ as *const c_void, 4);
    };
    set(20, 1);
    set(35, color_ref(C_INK).0);
    set(34, color_ref(C_PAPER).0);
    set(36, color_ref(C_PAPER).0);
}

fn run(get: GetState, tx: Sender<Ev>, show: bool, ready: Sender<()>) {
    let _com = Com::init();
    unsafe {
        let _ = windows::Win32::UI::HiDpi::SetProcessDpiAwarenessContext(windows::Win32::UI::HiDpi::DPI_AWARENESS_CONTEXT_SYSTEM_AWARE);
        let scale = if GetDpiForSystem() >= 168 { 2 } else { 1 };
        let hi: HINSTANCE = GetModuleHandleW(None).unwrap_or_default().into();
        let cur = LoadCursorW(None, IDC_ARROW).unwrap_or_default();
        let bg = CreateSolidBrush(color_ref(C_INK));
        let icons = [make_icon(0), make_icon(1), make_icon(2)];
        let mut app_icon = LoadIconW(Some(hi), windows::core::w!("APP")).unwrap_or_default();
        if app_icon.is_invalid() {
            app_icon = icons[0];
        }
        let cls = HSTRING::from(PANEL_CLASS);
        let wc = WNDCLASSEXW {
            cbSize: std::mem::size_of::<WNDCLASSEXW>() as u32,
            lpfnWndProc: Some(wnd_proc),
            hInstance: hi,
            hCursor: cur,
            hbrBackground: bg,
            lpszClassName: PCWSTR(cls.as_ptr()),
            hIcon: app_icon,
            hIconSm: app_icon,
            ..Default::default()
        };
        RegisterClassExW(&wc);
        let task_msg = RegisterWindowMessageW(windows::core::w!("TaskbarCreated"));

        let style = WS_CAPTION | WS_SYSMENU | WS_MINIMIZEBOX | WS_MAXIMIZEBOX | WS_THICKFRAME;
        let mut mr = RECT { left: 0, top: 0, right: PANEL_W, bottom: PANEL_H };
        let _ = AdjustWindowRect(&mut mr, style, false);
        let mut wr = RECT { left: 0, top: 0, right: PANEL_W * scale, bottom: PANEL_H * scale };
        let _ = AdjustWindowRect(&mut wr, style, false);
        let (w, h) = (wr.right - wr.left, wr.bottom - wr.top);
        let (sw, sh) = (GetSystemMetrics(SM_CXFULLSCREEN), GetSystemMetrics(SM_CYFULLSCREEN));
        let (x, y) = ((sw - w - 24).max(0), (sh - h - 24).max(0));

        UI.with(|u| {
            *u.borrow_mut() = Some(Ui {
                get,
                tx,
                hwnd: HWND::default(),
                icons,
                icon_idx: 0,
                tip: String::new(),
                st: UiState::default(),
                hits: Vec::new(),
                frame: 0,
                min_w: mr.right - mr.left,
                min_h: mr.bottom - mr.top,
                font_f: 0.0,
                fonts: [HFONT::default(); FONT_COUNT],
                mem_dc: HDC::default(),
                dib: HBITMAP::default(),
                bits: std::ptr::null_mut(),
                cw: 0,
                ch: 0,
                f: 1.0,
                ox: 0,
                oy: 0,
                task_msg,
            })
        });
        let Ok(hwnd) = CreateWindowExW(WINDOW_EX_STYLE(0), &cls, &HSTRING::from(hush::APP), style, x, y, w, h, None, None, Some(hi), None) else {
            let _ = ready.send(());
            return;
        };
        with(|u| u.hwnd = hwnd);
        G.ui_hwnd.store(hwnd.0 as isize, Relaxed);
        dark_title(hwnd);
        tray_add();
        SetTimer(Some(hwnd), 1, 120, None);
        if show {
            show_panel(true);
        }
        let _ = ready.send(());

        let mut m = MSG::default();
        while GetMessageW(&mut m, None, 0, 0).0 > 0 {
            let _ = TranslateMessage(&m);
            DispatchMessageW(&m);
        }
        let n = nid(hwnd, NOTIFY_ICON_DATA_FLAGS(0));
        let _ = Shell_NotifyIconW(NIM_DELETE, &n);
        G.ui_hwnd.store(0, Relaxed);
    }
}

fn tray_add() {
    let Some((h, icon, tip)) = with(|u| (u.hwnd, u.icons[u.icon_idx], if u.tip.is_empty() { hush::APP.to_string() } else { u.tip.clone() })) else {
        return;
    };
    let mut n = nid(h, NIF_MESSAGE | NIF_ICON | NIF_TIP);
    n.hIcon = icon;
    copy16(&mut n.szTip, &tip);
    unsafe {
        let _ = Shell_NotifyIconW(NIM_ADD, &n);
    }
}

fn tray_update(h: HWND, icon: HICON, tip: &str) {
    let mut n = nid(h, NIF_ICON | NIF_TIP);
    n.hIcon = icon;
    copy16(&mut n.szTip, tip);
    unsafe {
        let _ = Shell_NotifyIconW(NIM_MODIFY, &n);
    }
}

pub fn show_panel(show: bool) {
    let h = hwnd();
    if h.0.is_null() {
        return;
    }
    PANEL_VIS.store(show, Relaxed);
    unsafe {
        if show {
            let _ = ShowWindow(h, SW_SHOW);
            let _ = SetForegroundWindow(h);
        } else {
            let _ = ShowWindow(h, SW_HIDE);
        }
    }
}

fn send(a: Action) {
    with(|u| {
        let _ = u.tx.send(Ev::Act(a));
    });
}

fn do_action(a: Action) {
    match a.kind {
        "hidepanel" => show_panel(false),
        "panel" => show_panel(true),
        "beta" => send(act("beta", [1.0, 1.5, 2.0][(a.f as usize).min(2)])),
        _ => send(a),
    }
}

fn lparam_xy(lp: LPARAM) -> (i32, i32) {
    ((lp.0 & 0xFFFF) as i16 as i32, ((lp.0 >> 16) & 0xFFFF) as i16 as i32)
}

fn to_panel(u: &Ui, x: i32, y: i32) -> Point {
    Point { x: ((x - u.ox) as f64 / u.f) as i32, y: ((y - u.oy) as f64 / u.f) as i32 }
}

unsafe extern "system" fn wnd_proc(h: HWND, m: u32, wp: WPARAM, lp: LPARAM) -> LRESULT {
    match m {
        WM_PAINT => {
            paint(h);
            return LRESULT(0);
        }
        WM_ERASEBKGND => return LRESULT(1),
        WM_SIZE => {
            let _ = InvalidateRect(Some(h), None, false);
            return LRESULT(0);
        }
        WM_GETMINMAXINFO => {
            if let Some((w, hh)) = with(|u| (u.min_w, u.min_h)) {
                let mm = &mut *(lp.0 as *mut MINMAXINFO);
                mm.ptMinTrackSize = POINT { x: w, y: hh };
            }
            return LRESULT(0);
        }
        WM_POWERBROADCAST => {
            if wp.0 == 7 || wp.0 == 18 {
                send(act("resume", 0.0));
            }
            return LRESULT(1);
        }
        WM_TIMER => {
            on_timer(h);
            return LRESULT(0);
        }
        WM_LBUTTONDOWN => {
            let (x, y) = lparam_xy(lp);
            let hit = with(|u| {
                let p = to_panel(u, x, y);
                u.hits.iter().find(|b| b.r.has(p)).map(|b| b.act.clone())
            })
            .flatten();
            if let Some(a) = hit {
                do_action(a);
                let _ = InvalidateRect(Some(h), None, false);
            }
            return LRESULT(0);
        }
        WM_CLOSE => {
            show_panel(false);
            return LRESULT(0);
        }
        WM_QUIT_UI => {
            let _ = DestroyWindow(h);
            return LRESULT(0);
        }
        WM_SHOW_PANEL => {
            show_panel(true);
            return LRESULT(0);
        }
        WM_DESTROY => {
            PostQuitMessage(0);
            return LRESULT(0);
        }
        WM_TRAY => {
            match (lp.0 & 0xFFFF) as u32 {
                WM_LBUTTONUP => show_panel(!PANEL_VIS.load(Relaxed)),
                WM_RBUTTONUP => popup(h),
                _ => {}
            }
            return LRESULT(0);
        }
        _ => {}
    }
    if with(|u| u.task_msg != 0 && m == u.task_msg) == Some(true) {
        tray_add();
        return LRESULT(0);
    }
    DefWindowProcW(h, m, wp, lp)
}

fn on_timer(h: HWND) {
    let upd = with(|u| {
        let st = (u.get)();
        u.frame += 1;
        let icon = if !st.running {
            2
        } else if st.calibrating {
            1
        } else {
            0
        };
        let tip = format!("{} — {}", hush::APP, note_of(&st));
        u.st = st;
        if tip != u.tip || icon != u.icon_idx {
            u.tip = tip.clone();
            u.icon_idx = icon;
            Some((u.icons[icon], tip))
        } else {
            None
        }
    })
    .flatten();
    if let Some((icon, tip)) = upd {
        tray_update(h, icon, &tip);
    }
    if PANEL_VIS.load(Relaxed) {
        unsafe {
            let _ = InvalidateRect(Some(h), None, false);
        }
    }
}

fn mk_font(face: &str, h: i32, bold: bool) -> HFONT {
    unsafe {
        CreateFontW(
            -h,
            0,
            0,
            0,
            if bold { 700 } else { 400 },
            0,
            0,
            0,
            DEFAULT_CHARSET,
            OUT_DEFAULT_PRECIS,
            CLIP_DEFAULT_PRECIS,
            NONANTIALIASED_QUALITY,
            0,
            &HSTRING::from(face),
        )
    }
}

fn ensure_surface(u: &mut Ui, hdc: HDC, w: i32, h: i32) -> bool {
    if !u.mem_dc.is_invalid() && !u.bits.is_null() && u.cw == w && u.ch == h {
        return true;
    }
    unsafe {
        let mut bi = BITMAPINFO::default();
        bi.bmiHeader = BITMAPINFOHEADER {
            biSize: std::mem::size_of::<BITMAPINFOHEADER>() as u32,
            biWidth: w,
            biHeight: -h,
            biPlanes: 1,
            biBitCount: 32,
            ..Default::default()
        };
        let mut bits: *mut c_void = std::ptr::null_mut();
        let Ok(dib) = CreateDIBSection(Some(hdc), &bi, DIB_RGB_COLORS, &mut bits, None, 0) else { return false };
        if bits.is_null() {
            return false;
        }
        if u.mem_dc.is_invalid() {
            u.mem_dc = CreateCompatibleDC(Some(hdc));
            SetBkMode(u.mem_dc, TRANSPARENT);
        }
        SelectObject(u.mem_dc, dib.into());
        if !u.dib.is_invalid() {
            let _ = DeleteObject(u.dib.into());
        }
        u.dib = dib;
        u.bits = bits as *mut u32;
        u.cw = w;
        u.ch = h;
    }
    true
}

// шрифты под масштаб f
fn use_fonts(u: &mut Ui, f: f64) {
    if !u.fonts[0].is_invalid() && u.font_f == f {
        return;
    }
    for h in u.fonts {
        if !h.is_invalid() {
            unsafe {
                let _ = DeleteObject(h.into());
            }
        }
    }
    let sz = |v: f64| (v * f + 0.5) as i32;
    u.fonts = [
        mk_font("Courier New", sz(22.0), true),
        mk_font("Segoe UI", sz(16.0), false),
        mk_font("Segoe UI", sz(13.0), false),
        mk_font("Courier New", sz(14.0), true),
    ];
    u.font_f = f;
}

fn paint(h: HWND) {
    unsafe {
        let mut ps = PAINTSTRUCT::default();
        let hdc = BeginPaint(h, &mut ps);
        with(|u| {
            let mut cr = RECT::default();
            let _ = GetClientRect(h, &mut cr);
            let (cw, ch) = (cr.right.max(PANEL_W), cr.bottom.max(PANEL_H));
            if !ensure_surface(u, hdc, cw, ch) {
                return;
            }
            let f = (cw as f64 / PANEL_W as f64).min(ch as f64 * 0.94 / PANEL_H as f64);
            let f = ((f * 8.0) as i32 as f64 / 8.0).max(1.0);
            use_fonts(u, f);
            let mut cp = POINT::default();
            let _ = GetCursorPos(&mut cp);
            let _ = ScreenToClient(h, &mut cp);
            let _ = GdiFlush();
            let px = std::slice::from_raw_parts_mut(u.bits, (cw * ch) as usize);
            let mut cv = Canvas::new(cw, ch, px);
            cv.f = f;
            // сдвиг панели известен только после отрисовки: курсор пересчитывается по прошлому кадру
            let hov = Point { x: ((cp.x - u.ox) as f64 / f) as i32, y: ((cp.y - u.oy) as f64 / f) as i32 };
            let (hits, texts) = paint_panel(&mut cv, &u.st, hov, u.frame / 4 % 2 == 0);
            let texts: Vec<(Rect, TextOp)> = texts.into_iter().map(|t| (cv.dev_rect(t.r), t)).collect();
            u.f = cv.f;
            u.ox = cv.ox;
            u.oy = cv.oy;
            u.hits = hits;
            for (r, t) in texts.into_iter().filter(|(_, t)| !t.s.is_empty()) {
                SelectObject(u.mem_dc, u.fonts[t.font].into());
                SetTextColor(u.mem_dc, color_ref(if t.ink { C_INK } else { C_PAPER }));
                let mut w: Vec<u16> = t.s.encode_utf16().collect();
                let mut rr = RECT { left: r.l, top: r.t, right: r.r, bottom: r.b };
                DrawTextW(u.mem_dc, &mut w, &mut rr, DRAW_TEXT_FORMAT(t.flags));
            }
            let _ = GdiFlush();
            let _ = BitBlt(hdc, 0, 0, cw, ch, Some(u.mem_dc), 0, 0, SRCCOPY);
        });
        let _ = EndPaint(h, &ps);
    }
}

// ---------- меню в трее ----------

struct Menu {
    h: HMENU,
    acts: Vec<(u32, Action)>,
    id: u32,
}

impl Menu {
    fn add(&mut self, m: HMENU, label: &str, a: Action, checked: bool) {
        self.id += 1;
        self.acts.push((self.id, a));
        let flags = if checked { MF_STRING | MF_CHECKED } else { MF_STRING };
        unsafe {
            let _ = AppendMenuW(m, flags, self.id as usize, &HSTRING::from(label));
        }
    }
    fn sep(&self, m: HMENU) {
        unsafe {
            let _ = AppendMenuW(m, MF_SEPARATOR, 0, PCWSTR::null());
        }
    }
    fn sub(&self, title: &str, m: HMENU) {
        unsafe {
            let _ = AppendMenuW(self.h, MF_POPUP, m.0 as usize, &HSTRING::from(title));
        }
    }
    fn radio(&mut self, title: &str, kind: &'static str, opts: &[&str], sel: i32) {
        let m = unsafe { CreatePopupMenu().unwrap_or_default() };
        for (i, o) in opts.iter().enumerate() {
            self.add(m, o, act(kind, i as f64), i as i32 == sel);
        }
        self.sub(title, m);
    }
    fn devices(&mut self, title: &str, flow: EDataFlow, kind: &'static str, cur: &str, with_default: bool) {
        let m = unsafe { CreatePopupMenu().unwrap_or_default() };
        if with_default {
            self.add(m, l("По умолчанию в Windows", "Windows default"), Action { kind, s: String::new(), f: 0.0 }, cur.is_empty());
        }
        for n in device_names(flow) {
            let on = !cur.is_empty() && contains_fold(&n, cur);
            self.add(m, &n, Action { kind, s: n.clone(), f: 0.0 }, on);
        }
        self.sub(title, m);
    }
}

fn popup(h: HWND) {
    let Some(st) = with(|u| (u.get)()) else { return };
    let ru = hush::lang::is_ru();
    let hm = unsafe { CreatePopupMenu().unwrap_or_default() };
    let mut m = Menu { h: hm, acts: Vec::new(), id: 100 };
    m.add(hm, l("Открыть панель", "Open panel"), act("panel", 0.0), false);
    m.sep(hm);
    m.radio(l("Подавление наводки", "Bleed suppression"), "beta", &[l("Слабое", "Soft"), l("Обычное", "Normal"), l("Сильное", "Strong")], beta_sel(st.beta));
    m.radio(
        l("Шумоподавление", "Noise reduction"),
        "noise",
        &[l("Выкл", "Off"), l("Слабое", "Light"), l("Среднее", "Medium"), l("Сильное", "Strong"), l("Максимум", "Maximum")],
        st.noise,
    );
    m.radio(l("Эквалайзер", "Equalizer"), "eq", &[l("Выкл", "Off"), l("Чёткая речь", "Clear speech"), l("Мягкий", "Soft")], st.eq);
    m.radio(l("Микрофон по кнопке", "Push to talk"), "ptt", &[l("Всегда включён", "Always on"), l("Удерживать", "Hold"), l("Вкл/выкл", "Toggle")], st.ptt);
    m.radio(l("Захват микрофона", "Mic capture"), "micexcl", &[l("Общий", "Shared"), l("Монопольный", "Exclusive")], st.mic_ex_want as i32);
    m.radio(l("Вывод в кабель", "Cable output"), "excl", &[l("Монопольный", "Exclusive"), l("Общий", "Shared")], if st.excl_want { 0 } else { 1 });
    m.add(hm, &format!("{}{}", l("Кнопка микрофона: ", "Mic key: "), st.ptt_key), act("bind", 0.0), false);
    m.add(hm, l("Звук при включении микрофона", "Sound on mic toggle"), act("cue", 0.0), st.cue);
    m.add(hm, l("Автогромкость", "Auto gain"), act("agc", 2.0), st.agc);
    m.sep(hm);
    m.devices(l("Микрофон", "Microphone"), eCapture, "mic", &st.mic, true);
    m.devices(l("Наушники", "Headphones"), eRender, "ref", &st.reff, true);
    m.devices(l("Вывод (виртуальный кабель)", "Output (virtual cable)"), eRender, "out", &st.out, false);
    m.sep(hm);
    m.add(hm, l("Калибровка", "Calibrate"), act("recal", 0.0), false);
    m.add(hm, l("Запускать вместе с Windows", "Start with Windows"), act("autostart", 0.0), autostart_on());
    let mins = super::app::idle_sleep_min();
    let sl = if ru { format!("Сон ПК при тишине ({mins} мин)") } else { format!("Sleep on silence ({mins} min)") };
    m.add(hm, &sl, act("idlesleep", 0.0), G.idle_sleep_on.load(Relaxed));
    m.add(hm, l("Открыть журнал", "Open log"), act("log", 0.0), false);
    if !st.upd_busy {
        if !st.upd_latest.is_empty() {
            let s = if ru { format!("Обновить до {}", st.upd_latest) } else { format!("Update to {}", st.upd_latest) };
            m.add(hm, &s, act("update", 0.0), false);
        } else {
            m.add(hm, l("Проверить обновления", "Check for updates"), act("checkupd", 0.0), false);
        }
    }
    m.sep(hm);
    m.add(hm, l("Выход", "Quit"), act("quit", 0.0), false);

    unsafe {
        let mut pt = POINT::default();
        let _ = GetCursorPos(&mut pt);
        let _ = SetForegroundWindow(h);
        let r = TrackPopupMenu(hm, TPM_RETURNCMD | TPM_RIGHTBUTTON | TPM_BOTTOMALIGN, pt.x, pt.y, None, h, None);
        let _ = PostMessageW(Some(h), WM_NULL, WPARAM(0), LPARAM(0));
        let _ = DestroyMenu(hm);
        if r.0 != 0 {
            if let Some((_, a)) = m.acts.into_iter().find(|(id, _)| *id == r.0 as u32) {
                do_action(a);
            }
        }
    }
}
