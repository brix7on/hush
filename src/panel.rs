use crate::glyphs::glyph;
use crate::{l, VERSION};

// ---------- общие типы панели ----------

#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Point {
    pub x: i32,
    pub y: i32,
}

#[derive(Clone, Copy, Debug, Default, PartialEq)]
#[repr(C)]
pub struct Rect {
    pub l: i32,
    pub t: i32,
    pub r: i32,
    pub b: i32,
}

pub const fn rc(l: i32, t: i32, r: i32, b: i32) -> Rect {
    Rect { l, t, r, b }
}

impl Rect {
    pub fn has(&self, p: Point) -> bool {
        p.x >= self.l && p.x < self.r && p.y >= self.t && p.y < self.b
    }
}

#[derive(Clone, Debug, Default)]
pub struct UiState {
    pub mic_db: f64,
    pub ref_db: f64,
    pub out_db: f64,
    pub reduction: f64,
    pub voice: bool,
    pub calibrating: bool,
    pub found: bool,
    pub running: bool,
    pub limiting: f64,
    pub gain_db: f64,
    pub beta: f64,
    pub noise: i32,
    pub eq: i32,
    pub agc: bool,
    pub agc_gain: f64,
    pub ptt: i32,
    pub ptt_key: String,
    pub ptt_open: bool,
    pub binding: bool,
    pub cue: bool,
    pub latency_ms: i64,
    pub excl_want: bool,
    pub excl_on: bool,
    pub mic_period_ms: f64,
    pub mic_ex_want: bool,
    pub mic_ex_on: bool,
    pub mic_ex_why: String,
    pub mic_glitches: i64,
    pub out_starves: i64,
    pub mic_own_clock: bool,
    pub mic_loss_pct: f64,
    pub mic: String,
    pub reff: String,
    pub out: String,
    pub note: String,
    pub upd_latest: String,
    pub upd_msg: String,
    pub upd_busy: bool,
}

// просьба интерфейса к основной программе
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Action {
    pub kind: &'static str,
    pub s: String,
    pub f: f64,
}

pub fn act(kind: &'static str, f: f64) -> Action {
    Action { kind, s: String::new(), f }
}

#[derive(Clone, Debug)]
pub struct HitBox {
    pub r: Rect,
    pub act: Action,
}

pub const FONT_TITLE: usize = 0;
pub const FONT_BODY: usize = 1;
pub const FONT_SMALL: usize = 2;
pub const FONT_MONO: usize = 3;
pub const FONT_COUNT: usize = 4;

pub const DT_LEFT: u32 = 0x0;
pub const DT_CENTER: u32 = 0x1;
pub const DT_RIGHT: u32 = 0x2;
pub const DT_VCENTER: u32 = 0x4;
pub const DT_SINGLE: u32 = 0x20;
pub const DT_ELLIPSIS: u32 = 0x8000;

#[derive(Clone, Debug)]
pub struct TextOp {
    pub r: Rect,
    pub s: String,
    pub ink: bool,
    pub font: usize,
    pub flags: u32,
}

pub fn note_of(st: &UiState) -> String {
    if !st.note.is_empty() {
        return st.note.clone();
    }
    let s = if !st.running {
        l("переподключение", "reconnecting")
    } else if st.binding {
        l("нажми кнопку для микрофона", "press a key for the mic")
    } else if st.ptt != 0 && !st.ptt_open {
        l("микрофон выключен", "mic is off")
    } else if st.calibrating {
        l("калибровка", "calibrating")
    } else if !st.found {
        l("подстройка под гарнитуру", "learning the headset")
    } else if st.voice {
        l("говоришь", "speaking")
    } else {
        l("тишина", "silence")
    };
    s.to_string()
}

// ---------- холст ----------

pub const PANEL_W: i32 = 600;
pub const PANEL_H: i32 = 580;
pub const C_INK: u32 = 0x0a0a0b;
pub const C_PAPER: u32 = 0xded8ca;

// буфер пикселей; рисование в координатах панели, f — масштаб, ox/oy — сдвиг
pub struct Canvas<'a> {
    pub w: i32,
    pub h: i32,
    pub px: &'a mut [u32],
    pub f: f64,
    pub ox: i32,
    pub oy: i32,
}

impl<'a> Canvas<'a> {
    pub fn new(w: i32, h: i32, px: &'a mut [u32]) -> Self {
        Canvas { w, h, px, f: 1.0, ox: 0, oy: 0 }
    }

    pub fn dev(&self, v: i32) -> i32 {
        (v as f64 * self.f + 0.5) as i32
    }

    pub fn dev_rect(&self, r: Rect) -> Rect {
        rc(self.ox + self.dev(r.l), self.oy + self.dev(r.t), self.ox + self.dev(r.r), self.oy + self.dev(r.b))
    }

    pub fn to_panel(&self, x: i32, y: i32) -> Point {
        Point { x: ((x - self.ox) as f64 / self.f) as i32, y: ((y - self.oy) as f64 / self.f) as i32 }
    }

    pub fn fill_dev(&mut self, r: Rect, col: u32) {
        let (x0, y0) = (r.l.max(0), r.t.max(0));
        let (x1, y1) = (r.r.min(self.w), r.b.min(self.h));
        for y in y0..y1 {
            let row = &mut self.px[(y * self.w) as usize..((y + 1) * self.w) as usize];
            for x in x0..x1 {
                row[x as usize] = col;
            }
        }
    }

    pub fn set(&mut self, x: i32, y: i32, col: u32) {
        self.fill_dev(self.dev_rect(rc(x, y, x + 1, y + 1)), col);
    }

    pub fn fill(&mut self, r: Rect, col: u32) {
        self.fill_dev(self.dev_rect(r), col);
    }

    pub fn frame(&mut self, r: Rect, col: u32) {
        self.fill(rc(r.l, r.t, r.r, r.t + 1), col);
        self.fill(rc(r.l, r.b - 1, r.r, r.b), col);
        self.fill(rc(r.l, r.t, r.l + 1, r.b), col);
        self.fill(rc(r.r - 1, r.t, r.r, r.b), col);
    }

    // точечная заливка, как узор --dots в Gust
    pub fn dots(&mut self, r: Rect, col: u32) {
        let mut y = r.t;
        while y < r.b {
            let mut x = r.l;
            while x < r.r {
                self.set(x, y, col);
                x += 2;
            }
            y += 2;
        }
    }

    fn panel_box(&mut self, r: Rect) {
        self.fill(r, C_INK);
        self.frame(r, C_PAPER);
    }

    pub fn glyph(&mut self, name: &str, cx: i32, cy: i32, col: u32) {
        let g = glyph(name);
        if g.is_empty() {
            return;
        }
        let (x0, y0) = (cx - g[0].len() as i32 / 2, cy - g.len() as i32 / 2);
        for (y, row) in g.iter().enumerate() {
            for (x, ch) in row.bytes().enumerate() {
                if ch == b'#' {
                    self.set(x0 + x as i32, y0 + y as i32, col);
                }
            }
        }
    }

    // значок уровня: n из m столбиков
    fn bars(&mut self, n: i32, m: i32, cx: i32, cy: i32, col: u32) {
        let (bw, gap) = (3, 2);
        let total = m * bw + (m - 1) * gap;
        let mut x = cx - total / 2;
        for i in 0..m {
            let h = 3 + i * 9 / (m - 1).max(1);
            let r = rc(x, cy + 6 - h, x + bw, cy + 6);
            if i < n {
                self.fill(r, col);
            } else {
                self.frame(r, col);
            }
            x += bw + gap;
        }
    }

    // фон: целое увеличение по высоте, справа внизу, слева повторяется край неба
    pub fn scene(&mut self) {
        let mask = scene_mask();
        let k = ((self.h + PANEL_H - 1) / PANEL_H).max(1);
        let (x0, y0) = (self.w - PANEL_W * k, self.h - PANEL_H * k);
        for y in 0..self.h {
            let sy = (y - y0).div_euclid(k);
            let row = &mut self.px[(y * self.w) as usize..((y + 1) * self.w) as usize];
            for (x, p) in row.iter_mut().enumerate() {
                let x = x as i32;
                let mut sx = (x - x0) / k;
                if x < x0 {
                    sx = ((x - x0) / k % 64 + 64) % 64;
                }
                let on = sy >= 0 && sx < PANEL_W && sy < PANEL_H && mask[(sy * PANEL_W + sx) as usize];
                *p = if on { C_PAPER } else { C_INK };
            }
        }
    }
}

// ---------- фон ----------

static SCENE_PNG: &[u8] = include_bytes!("../assets/scene.png");

fn scene_mask() -> &'static [bool] {
    static MASK: std::sync::OnceLock<Vec<bool>> = std::sync::OnceLock::new();
    MASK.get_or_init(|| {
        let mut m = vec![false; (PANEL_W * PANEL_H) as usize];
        let mut dec = png::Decoder::new(std::io::Cursor::new(SCENE_PNG));
        dec.set_transformations(png::Transformations::EXPAND | png::Transformations::STRIP_16);
        let Ok(mut rd) = dec.read_info() else { return m };
        let mut buf = vec![0u8; rd.output_buffer_size().unwrap_or(0)];
        let Ok(info) = rd.next_frame(&mut buf) else { return m };
        let ch = info.color_type.samples();
        for y in 0..(PANEL_H as usize).min(info.height as usize) {
            for x in 0..(PANEL_W as usize).min(info.width as usize) {
                let v = buf[y * info.line_size + x * ch];
                m[y * PANEL_W as usize + x] = v > 0x80;
            }
        }
        m
    })
}

// ---------- отрисовка ----------

fn db_frac(db: f64) -> f64 {
    ((db + 70.0) / 70.0).clamp(0.0, 1.0)
}

struct Painter<'c, 'a> {
    c: &'c mut Canvas<'a>,
    hits: Vec<HitBox>,
    texts: Vec<TextOp>,
    tip: String,
    hov: Point,
}

impl Painter<'_, '_> {
    fn text(&mut self, r: Rect, s: impl Into<String>, ink: bool, font: usize, flags: u32) {
        self.texts.push(TextOp { r, s: s.into(), ink, font, flags: flags | DT_SINGLE });
    }

    fn row_icon(&mut self, y: i32, ic: &str, warn: &str) {
        self.c.glyph(ic, 34, y + 13, C_PAPER);
        if !warn.is_empty() {
            self.c.glyph("warn", 46, y + 5, C_PAPER);
            if rc(22, y, 52, y + 26).has(self.hov) {
                self.tip = warn.to_string();
            }
        }
    }

    fn button(&mut self, r: Rect, ic: &str, label: &str, a: Action, on: bool, tp: &str) {
        let hot = r.has(self.hov);
        if hot {
            self.tip = tp.to_string();
        }
        let inv = on != hot;
        self.c.fill(r, if inv { C_PAPER } else { C_INK });
        self.c.frame(r, C_PAPER);
        let fg = if inv { C_INK } else { C_PAPER };
        if !ic.is_empty() && !label.is_empty() {
            self.c.glyph(ic, r.l + 18, (r.t + r.b) / 2, fg);
            self.text(rc(r.l + 30, r.t, r.r - 6, r.b), label, inv, FONT_MONO, DT_VCENTER | DT_ELLIPSIS);
        } else if !ic.is_empty() {
            self.c.glyph(ic, (r.l + r.r) / 2, (r.t + r.b) / 2, fg);
        } else {
            self.text(r, label, inv, FONT_MONO, DT_VCENTER | DT_CENTER | DT_ELLIPSIS);
        }
        self.hits.push(HitBox { r, act: a });
    }

    #[allow(clippy::too_many_arguments)]
    fn seg(&mut self, y: i32, ic: &str, glyphs: &[&str], tips: &[String], sel: i32, kind: &'static str, warn: &str) {
        self.row_icon(y, ic, warn);
        let cr = rc(56, y, 352, y + 26);
        self.c.frame(cr, C_PAPER);
        let n = glyphs.len() as i32;
        let bw = (cr.r - cr.l) / n;
        for (i, g) in glyphs.iter().enumerate() {
            let i = i as i32;
            let lft = cr.l + i * bw;
            let mut r = rc(lft, cr.t, lft + bw, cr.b);
            if i == n - 1 {
                r.r = cr.r;
            }
            if i > 0 {
                self.c.fill(rc(r.l, r.t, r.l + 1, r.b), C_PAPER);
            }
            let mut fg = C_PAPER;
            if i == sel {
                self.c.fill(rc(r.l + 2, r.t + 2, r.r - 2, r.b - 2), C_PAPER);
                fg = C_INK;
            } else if r.has(self.hov) {
                self.c.frame(rc(r.l + 2, r.t + 2, r.r - 2, r.b - 2), C_PAPER);
            }
            if r.has(self.hov) {
                self.tip = tips[i as usize].clone();
            }
            let (cx, cy) = ((r.l + r.r) / 2, (r.t + r.b) / 2);
            if let Some(rest) = g.strip_prefix("lvl:") {
                let mut p = rest.split(':').map(|v| v.parse::<i32>().unwrap_or(0));
                let (a, b) = (p.next().unwrap_or(0), p.next().unwrap_or(1));
                self.c.bars(a, b, cx, cy, fg);
            } else {
                self.c.glyph(g, cx, cy, fg);
            }
            self.hits.push(HitBox { r, act: act(kind, i as f64) });
        }
    }
}

fn s(v: &str) -> String {
    v.to_string()
}

pub fn beta_sel(beta: f64) -> i32 {
    if beta < 1.25 {
        0
    } else if beta > 1.75 {
        2
    } else {
        1
    }
}

// рисует панель; возвращает зоны нажатий и подписи для системного шрифта
pub fn paint_panel(c: &mut Canvas, st: &UiState, hov: Point, blink: bool) -> (Vec<HitBox>, Vec<TextOp>) {
    if c.f <= 0.0 {
        c.f = 1.0;
    }
    let (pw, ph) = (c.dev(PANEL_W), c.dev(PANEL_H));
    c.ox = 0;
    c.oy = ((c.h - ph) / 2).max(0);
    let extra = c.w - pw;
    if extra > 0 {
        c.ox = extra.min(c.w / 25);
    }
    c.scene();
    let mut p = Painter { c, hits: Vec::new(), texts: Vec::new(), tip: String::new(), hov };

    // шапка
    p.c.panel_box(rc(16, 14, 364, 52));
    p.text(rc(28, 14, 240, 52), "H U S H", false, FONT_TITLE, DT_VCENTER);
    if st.upd_busy {
        p.text(rc(180, 14, 352, 52), st.upd_msg.clone(), false, FONT_SMALL, DT_VCENTER | DT_RIGHT);
    } else if !st.upd_latest.is_empty() {
        let r = rc(212, 20, 356, 46);
        let hot = r.has(hov);
        p.c.fill(r, C_PAPER);
        if hot {
            p.c.fill(r, C_INK);
            p.c.frame(r, C_PAPER);
            p.tip = if crate::lang::is_ru() {
                format!("Скачать и установить {}, hush перезапустится", st.upd_latest)
            } else {
                format!("Download and install {}, hush will restart", st.upd_latest)
            };
        }
        let label = if crate::lang::is_ru() {
            format!("обновить до {}", st.upd_latest)
        } else {
            format!("update to {}", st.upd_latest)
        };
        p.text(r, label, !hot, FONT_SMALL, DT_VCENTER | DT_CENTER);
        p.hits.push(HitBox { r, act: act("update", 0.0) });
    } else {
        p.text(rc(200, 14, 352, 52), VERSION, false, FONT_SMALL, DT_VCENTER | DT_RIGHT);
    }

    // состояние
    p.c.panel_box(rc(16, 60, 364, 96));
    let lamp = rc(28, 71, 42, 85);
    if !st.running {
        p.c.frame(lamp, C_PAPER);
        p.c.frame(rc(lamp.l + 2, lamp.t + 2, lamp.r - 2, lamp.b - 2), C_PAPER);
    } else if st.calibrating || st.binding {
        if blink {
            p.c.fill(lamp, C_PAPER);
        } else {
            p.c.frame(lamp, C_PAPER);
            p.c.dots(lamp, C_PAPER);
        }
    } else if st.ptt != 0 && !st.ptt_open {
        p.c.frame(lamp, C_PAPER);
        for i in 0..14 {
            p.c.set(lamp.l + i, lamp.t + i, C_PAPER);
            p.c.set(lamp.r - 1 - i, lamp.t + i, C_PAPER);
        }
    } else if st.voice {
        p.c.fill(lamp, C_PAPER);
    } else {
        p.c.frame(lamp, C_PAPER);
    }
    p.text(rc(54, 60, 352, 96), note_of(st), false, FONT_BODY, DT_VCENTER | DT_ELLIPSIS);

    // индикаторы
    p.c.panel_box(rc(16, 104, 364, 216));
    let meter = |p: &mut Painter, y: i32, ic: &str, db: f64| {
        p.c.glyph(ic, 34, y, C_PAPER);
        let bar = rc(56, y - 5, 352, y + 6);
        p.c.frame(bar, C_PAPER);
        let w = ((bar.r - bar.l - 4) as f64 * db_frac(db)) as i32;
        if w > 0 {
            p.c.fill(rc(bar.l + 2, bar.t + 2, bar.l + 2 + w, bar.b - 2), C_PAPER);
        }
    };
    meter(&mut p, 124, "mic", st.mic_db);
    meter(&mut p, 150, "phones", st.ref_db);
    meter(&mut p, 176, "plug", st.out_db);
    if st.limiting > 1.0 {
        p.c.dots(rc(352 - 30, 170, 352, 182), C_PAPER);
    }
    let line = if !st.running {
        String::new()
    } else if st.calibrating {
        s(l("Не говори, идёт калибровка", "Stay quiet, calibrating"))
    } else if !st.found {
        s(l("Включи звук в наушниках, hush подстроится", "Play something in the headphones to let hush adapt"))
    } else if st.ref_db < -80.0 {
        s(l("В наушниках тихо", "Headphones are silent"))
    } else if crate::lang::is_ru() {
        format!("Наводка −{:.1} дБ", st.reduction.abs())
    } else {
        format!("Bleed −{:.1} dB", st.reduction.abs())
    };
    let status_r = rc(28, 190, 352, 210);

    // ряды настроек
    p.c.panel_box(rc(16, 224, 364, 532));
    let row_y = |i: i32| 236 + i * 32;

    p.row_icon(row_y(0), "speaker", "");
    let gain = if crate::lang::is_ru() { format!("{:+.0} дБ", st.gain_db) } else { format!("{:+.0} dB", st.gain_db) };
    p.text(rc(56, row_y(0), 240, row_y(0) + 26), gain, false, FONT_MONO, DT_VCENTER);
    p.button(rc(256, row_y(0), 302, row_y(0) + 26), "minus", "", act("gain", -1.0), false, l("Усиление −1 дБ", "Gain −1 dB"));
    p.button(rc(306, row_y(0), 352, row_y(0) + 26), "plus", "", act("gain", 1.0), false, l("Усиление +1 дБ", "Gain +1 dB"));

    p.seg(
        row_y(1),
        "phones",
        &["lvl:1:3", "lvl:2:3", "lvl:3:3"],
        &[
            s(l("Подавление наводки: слабое", "Bleed suppression: soft")),
            s(l("Подавление наводки: обычное", "Bleed suppression: normal")),
            s(l("Подавление наводки: сильное", "Bleed suppression: strong")),
        ],
        beta_sel(st.beta),
        "beta",
        "",
    );
    p.seg(
        row_y(2),
        "waves",
        &["off", "lvl:1:4", "lvl:2:4", "lvl:3:4", "lvl:4:4"],
        &[
            s(l("Шумоподавление выключено", "Noise reduction off")),
            s(l("Шумоподавление: слабое", "Noise reduction: light")),
            s(l("Шумоподавление: среднее", "Noise reduction: medium")),
            s(l("Шумоподавление: сильное", "Noise reduction: strong")),
            s(l("Шумоподавление: максимум", "Noise reduction: maximum")),
        ],
        st.noise,
        "noise",
        "",
    );
    p.seg(
        row_y(3),
        "sliders",
        &["off", "speech", "soft"],
        &[
            s(l("Эквалайзер выключен", "Equalizer off")),
            s(l("Эквалайзер: чёткая речь", "Equalizer: clear speech")),
            s(l("Эквалайзер: мягкий", "Equalizer: soft")),
        ],
        st.eq,
        "eq",
        "",
    );
    let agc_on = if crate::lang::is_ru() {
        format!("Автогромкость включена ({:+.0} дБ)", st.agc_gain)
    } else {
        format!("Auto gain on ({:+.0} dB)", st.agc_gain)
    };
    p.seg(row_y(4), "auto", &["off", "auto"], &[s(l("Автогромкость выключена", "Auto gain off")), agc_on], st.agc as i32, "agc", "");
    p.seg(
        row_y(5),
        "mic",
        &["mic", "hold", "toggle"],
        &[
            s(l("Микрофон всегда включён", "Mic always on")),
            s(l("Микрофон работает, пока нажата кнопка", "Push to talk")),
            s(l("Кнопка включает и выключает микрофон", "Key toggles the mic")),
        ],
        st.ptt,
        "ptt",
        "",
    );

    p.row_icon(row_y(6), "key", "");
    let key_label = if st.binding { s(l("нажми клавишу", "press a key")) } else { st.ptt_key.clone() };
    p.button(rc(56, row_y(6), 302, row_y(6) + 26), "", &key_label, act("bind", 0.0), st.binding, l("Назначить кнопку микрофона", "Assign the mic key"));
    p.button(rc(306, row_y(6), 352, row_y(6) + 26), "speaker", "", act("cue", 0.0), st.cue, l("Звук при включении микрофона", "Sound on mic toggle"));

    let mic_sel = st.mic_ex_want as i32;
    let ex_sel = if st.excl_want { 0 } else { 1 };
    let mut mic_warn = String::new();
    let mut ex_warn = String::new();
    if st.running && st.mic_period_ms > 0.0 {
        if st.mic_ex_want && !st.mic_ex_on {
            mic_warn = s(l("Монопольный захват не включился", "Exclusive capture is not active"));
            if !st.mic_ex_why.is_empty() {
                mic_warn += ": ";
                mic_warn += &st.mic_ex_why;
            }
        } else if st.mic_glitches > 0 || st.mic_loss_pct >= 0.1 {
            mic_warn = if crate::lang::is_ru() {
                format!("Сбоев микрофона: {}, потери {:.1}%", st.mic_glitches, st.mic_loss_pct)
            } else {
                format!("Mic glitches: {}, loss {:.1}%", st.mic_glitches, st.mic_loss_pct)
            };
        }
    }
    if st.excl_want && st.running && !st.excl_on {
        ex_warn = s(l("Монопольный вывод не включился, работает обычный", "Exclusive output is not active, shared mode is used"));
    }
    p.seg(
        row_y(7),
        "capture",
        &["shared", "lock"],
        &[s(l("Захват микрофона: общий", "Mic capture: shared")), s(l("Захват микрофона: монопольный", "Mic capture: exclusive"))],
        mic_sel,
        "micexcl",
        &mic_warn,
    );
    p.seg(
        row_y(8),
        "plug",
        &["lock", "shared"],
        &[s(l("Вывод в кабель: монопольный", "Cable output: exclusive")), s(l("Вывод в кабель: общий", "Cable output: shared"))],
        ex_sel,
        "excl",
        &ex_warn,
    );

    // кнопки
    p.button(rc(16, 542, 188, 568), "calib", l("Калибровка", "Calibrate"), act("recal", 0.0), false, l("Заново измерить наводку", "Measure the bleed again"));
    p.button(rc(192, 542, 364, 568), "tray", l("Свернуть", "Hide"), act("hidepanel", 0.0), false, l("Свернуть в трей", "Hide to tray"));

    // строка подсказки
    if !p.tip.is_empty() {
        let tip = p.tip.clone();
        p.text(status_r, tip, false, FONT_SMALL, DT_VCENTER | DT_ELLIPSIS);
    } else {
        p.text(rc(status_r.l, status_r.t, 260, status_r.b), line, false, FONT_SMALL, DT_VCENTER | DT_ELLIPSIS);
        if st.running && st.latency_ms > 0 {
            let mut lat = if crate::lang::is_ru() { format!("~{} мс", st.latency_ms) } else { format!("~{} ms", st.latency_ms) };
            if st.out_starves > 0 {
                lat += &if crate::lang::is_ru() {
                    format!(" · сбоев {}", st.out_starves)
                } else {
                    format!(" · {} dropouts", st.out_starves)
                };
            }
            p.text(rc(240, status_r.t, status_r.r, status_r.b), lat, false, FONT_SMALL, DT_VCENTER | DT_RIGHT);
        }
    }
    (p.hits, p.texts)
}

// значок в трее: 0 — работает, 1 — калибровка, 2 — нет звука
pub fn tray_icon_pixels(state: i32) -> Vec<u32> {
    const N: i32 = 32;
    let mut px = vec![0u32; (N * N) as usize];
    let mut cv = Canvas::new(N, N, &mut px);
    cv.fill(rc(0, 0, N, N), C_INK);
    if state == 1 {
        cv.dots(rc(0, 0, N, N), C_PAPER);
        cv.fill(rc(5, 3, 27, 29), C_INK);
    }
    cv.frame(rc(0, 0, N, N), C_PAPER);
    for (y, row) in glyph("mic").iter().enumerate() {
        for (x, ch) in row.bytes().enumerate() {
            if ch == b'#' {
                let (x, y) = (x as i32, y as i32);
                cv.fill(rc(3 + 2 * x, 3 + 2 * y, 5 + 2 * x, 5 + 2 * y), C_PAPER);
            }
        }
    }
    if state == 2 {
        for i in 2..N - 2 {
            cv.fill(rc(i, N - 1 - i - 1, i + 2, N - 1 - i + 1), C_PAPER);
        }
    }
    px
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn panel_paints() {
        let st = UiState { running: true, found: true, mic_db: -30.0, ptt_key: "Mouse 4".into(), latency_ms: 20, ..Default::default() };
        for (w, h, f) in [(600, 580, 1.0), (1920, 1009, 1.625)] {
            let mut px = vec![0u32; (w * h) as usize];
            let mut c = Canvas::new(w, h, &mut px);
            c.f = f;
            let (hits, texts) = paint_panel(&mut c, &st, Point { x: 100, y: 300 }, false);
            assert!(hits.len() > 20 && texts.len() > 5);
            assert!(px.iter().any(|&p| p == C_PAPER) && px.iter().any(|&p| p == C_INK));
        }
        assert!(scene_mask().iter().filter(|&&v| v).count() > 1000);
    }
}

#[cfg(test)]
mod preview {
    use super::*;

    // HUSH_PREVIEW=путь: пишет путь.rgb (сырые пиксели) и путь.txt (подписи)
    #[test]
    fn panel_preview() {
        let Ok(out) = std::env::var("HUSH_PREVIEW") else { return };
        crate::lang::init(true);
        let st = UiState {
            running: true, found: true, voice: true, mic_db: -28.0, ref_db: -22.0, out_db: -18.0, reduction: 9.4,
            gain_db: 6.0, beta: 1.5, noise: 2, eq: 1, agc: true, agc_gain: 4.0, ptt: 1, ptt_key: "Mouse 5".into(),
            ptt_open: true, cue: true, latency_ms: 28, excl_want: true, excl_on: true, mic_period_ms: 10.0,
            mic: "Headset".into(), out: "CABLE Input".into(), ..Default::default()
        };
        let mut px = vec![0u32; (PANEL_W * PANEL_H) as usize];
        let mut c = Canvas::new(PANEL_W, PANEL_H, &mut px);
        let (_, texts) = paint_panel(&mut c, &st, Point { x: -1, y: -1 }, true);
        let raw: Vec<u8> = px.iter().flat_map(|v| [(v >> 16) as u8, (v >> 8) as u8, *v as u8]).collect();
        std::fs::write(format!("{out}.rgb"), raw).unwrap();
        let t: Vec<String> = texts.iter().map(|t| format!("{} {} {} {} {:?} {} {} {}", t.r.l, t.r.t, t.r.r, t.r.b, t.s, t.ink, t.font, t.flags)).collect();
        std::fs::write(format!("{out}.txt"), t.join("\n")).unwrap();
    }
}
