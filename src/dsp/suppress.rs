use super::{db, f64_cell, Fft, C64};
use serde::{Deserialize, Serialize};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender};
use std::sync::{Arc, Mutex};

// подавитель наводки по огибающей

const VAD_NF_MUL: f64 = 3.0;
const VAD_START: f64 = 6.0;
const VAD_FULL: f64 = 16.0;

pub const SUP_FRAME: usize = 512;
pub const SUP_HOP: usize = 256;
const SUP_HIST: usize = 80;
const SUP_MAX_D: usize = 64;

const SUP_EDGES: [f64; 26] = [
    0.0, 100.0, 200.0, 300.0, 400.0, 510.0, 630.0, 770.0, 920.0, 1080.0, 1270.0, 1480.0, 1720.0,
    2000.0, 2320.0, 2700.0, 3150.0, 3700.0, 4400.0, 5300.0, 6400.0, 7700.0, 9500.0, 12000.0,
    15500.0, 1e9,
];

const AUTO_BUF: usize = 2400;
const AUTO_EVERY: usize = 375;
const AUTO_RECENT: usize = 1100;
const AUTO_MIN: usize = 900;
const AUTO_SWITCH: f64 = 1.5;

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct Calib {
    #[serde(default)]
    pub rate: usize,
    #[serde(rename = "delay_frames", default)]
    pub d: usize,
    #[serde(default)]
    pub w: Vec<f64>,
    #[serde(default)]
    pub corr: f64,
    #[serde(default)]
    pub found: bool,
    #[serde(rename = "leak_db", default)]
    pub leak_db: f64,
    #[serde(rename = "w_cal", default, skip_serializing_if = "Vec::is_empty")]
    pub w0: Vec<f64>,
}

#[derive(Clone, Copy, Debug, Default)]
pub struct SupStats {
    pub mic_db: f64,
    pub ref_db: f64,
    pub out_db: f64,
    pub reduction_db: f64,
    pub calibrating: bool,
    pub voice: bool,
    pub found: bool,
    pub delay_ms: f64,
}

#[derive(Clone, Copy, Default)]
struct Pub {
    mic: f64,
    r#ref: f64,
    out: f64,
    vad: f64,
    da: f64,
    oa: f64,
}

struct Locked {
    cal: Calib,
    w0: Vec<f64>,
    d0: usize,
    cal_req: usize,
    cal_left: usize,
    adapted: bool,
    pub_: Pub,
    pending: Option<(Calib, f64)>,
}

// общее состояние: читается из интерфейса, пока поток звука работает
pub struct SupShared {
    rate: usize,
    hop: usize,
    nb: usize,
    beta: AtomicU64,
    auto_off: AtomicBool,
    auto_busy: AtomicBool,
    vad: AtomicU64,
    m: Mutex<Locked>,
    cal_tx: SyncSender<Calib>,
    cal_rx: Mutex<Receiver<Calib>>,
}

impl SupShared {
    pub fn set_beta(&self, b: f64) {
        self.beta.store(b.to_bits(), Ordering::Relaxed);
    }
    pub fn beta(&self) -> f64 {
        f64::from_bits(self.beta.load(Ordering::Relaxed))
    }
    pub fn set_auto_learn(&self, on: bool) {
        self.auto_off.store(!on, Ordering::Relaxed);
    }
    pub fn rate(&self) -> usize {
        self.rate
    }
    pub fn hop(&self) -> usize {
        self.hop
    }
    pub fn voice_level(&self) -> f64 {
        f64::from_bits(self.vad.load(Ordering::Relaxed))
    }

    pub fn set_calib(&self, mut c: Calib) -> bool {
        if c.rate != self.rate || c.w.len() != self.nb {
            return false;
        }
        if c.w0.len() != self.nb {
            c.w0 = c.w.clone();
        }
        for i in 0..c.w.len() {
            c.w[i] = (0.7 * c.w0[i]).max((2.5 * c.w0[i]).min(c.w[i]));
        }
        let mut l = self.m.lock().unwrap();
        l.w0 = c.w0.clone();
        l.d0 = c.d;
        l.cal = c;
        true
    }

    pub fn calib(&self) -> Calib {
        let l = self.m.lock().unwrap();
        let mut c = l.cal.clone();
        c.w0 = l.w0.clone();
        c
    }

    pub fn start_calib(&self, frames: usize) {
        while self.cal_rx.lock().unwrap().try_recv().is_ok() {}
        self.m.lock().unwrap().cal_req = frames;
    }

    pub fn wait_calib(&self, timeout: std::time::Duration) -> Option<Calib> {
        self.cal_rx.lock().unwrap().recv_timeout(timeout).ok()
    }

    pub fn take_adapted(&self) -> bool {
        let mut l = self.m.lock().unwrap();
        std::mem::replace(&mut l.adapted, false)
    }

    pub fn stats(&self) -> SupStats {
        let l = self.m.lock().unwrap();
        let p = l.pub_;
        let mut st = SupStats {
            mic_db: db(p.mic),
            ref_db: db(p.r#ref),
            out_db: db(p.out),
            calibrating: l.cal_left > 0 || l.cal_req > 0,
            voice: p.vad > 0.5,
            found: l.cal.found,
            delay_ms: (l.cal.d * self.hop) as f64 * 1000.0 / self.rate as f64,
            reduction_db: 0.0,
        };
        if p.oa > 0.0 {
            st.reduction_db = db(p.da) - db(p.oa);
        }
        st
    }
}

struct AutoLearn {
    m: Vec<Vec<f64>>,
    s: Vec<Vec<f64>>,
    r: Vec<Vec<f64>>,
    ok: Vec<bool>,
    lv: Vec<f64>,
    pos: usize,
    n: usize,
    since: usize,
}

pub struct Suppressor {
    pub rate: usize,
    pub floor_db: f64,
    pub adapt: bool,
    pub n: usize,
    pub h: usize,
    nb: usize,
    sh: Arc<SupShared>,

    f: Fft,
    win: Vec<f64>,
    band_of: Vec<usize>,
    in_mic: Vec<f64>,
    in_ref: Vec<f64>,
    olap: Vec<f64>,
    mm: Vec<C64>,
    rr: Vec<C64>,
    mb: Vec<f64>,
    rb: Vec<f64>,
    s: Vec<f64>,
    hist: Vec<Vec<f64>>,
    hpos: usize,
    nf: Vec<f64>,
    nf_init: bool,
    g: Vec<f64>,
    gbin: Vec<f64>,

    vad: f64,
    bb_floor: f64,
    bb_init: bool,

    cal_m: Vec<Vec<f64>>,
    cal_s: Vec<Vec<f64>>,
    s_mic: f64,
    s_ref: f64,
    s_out: f64,
    s_da: f64,
    s_oa: f64,

    auto: AutoLearn,
}

impl Suppressor {
    pub fn new(rate: usize) -> Self {
        let (n, h) = (SUP_FRAME, SUP_HOP);
        let win: Vec<f64> = (0..n)
            .map(|i| (0.5 - 0.5 * (2.0 * std::f64::consts::PI * i as f64 / n as f64).cos()).sqrt())
            .collect();
        let nbins = n / 2 + 1;
        let mut band_of = vec![0usize; nbins];
        let mut nb = 0;
        for (k, bo) in band_of.iter_mut().enumerate() {
            let hz = k as f64 * rate as f64 / n as f64;
            let mut b = 0;
            while b + 1 < SUP_EDGES.len() && hz >= SUP_EDGES[b + 1] {
                b += 1;
            }
            *bo = b;
            nb = nb.max(b + 1);
        }
        let (tx, rx) = sync_channel(1);
        let sh = Arc::new(SupShared {
            rate,
            hop: h,
            nb,
            beta: f64_cell(1.5),
            auto_off: AtomicBool::new(false),
            auto_busy: AtomicBool::new(false),
            vad: f64_cell(0.0),
            m: Mutex::new(Locked {
                cal: Calib { rate, w: vec![0.0; nb], ..Default::default() },
                w0: Vec::new(),
                d0: 0,
                cal_req: 0,
                cal_left: 0,
                adapted: false,
                pub_: Pub::default(),
                pending: None,
            }),
            cal_tx: tx,
            cal_rx: Mutex::new(rx),
        });
        Suppressor {
            rate,
            floor_db: -20.0,
            adapt: true,
            n,
            h,
            nb,
            sh,
            f: Fft::new(n),
            win,
            band_of,
            in_mic: vec![0.0; n],
            in_ref: vec![0.0; n],
            olap: vec![0.0; n],
            mm: vec![C64::ZERO; n],
            rr: vec![C64::ZERO; n],
            mb: vec![0.0; nb],
            rb: vec![0.0; nb],
            s: vec![0.0; nb],
            hist: vec![vec![0.0; nb]; SUP_HIST],
            hpos: 0,
            nf: vec![0.0; nb],
            nf_init: false,
            g: vec![1.0; nb],
            gbin: vec![0.0; nbins],
            vad: 0.0,
            bb_floor: 0.0,
            bb_init: false,
            cal_m: Vec::new(),
            cal_s: Vec::new(),
            s_mic: 0.0,
            s_ref: 0.0,
            s_out: 0.0,
            s_da: 0.0,
            s_oa: 0.0,
            auto: AutoLearn {
                m: vec![vec![0.0; nb]; AUTO_BUF],
                s: vec![vec![0.0; nb]; AUTO_BUF],
                r: vec![vec![0.0; nb]; AUTO_BUF],
                ok: vec![false; AUTO_BUF],
                lv: vec![0.0; AUTO_BUF],
                pos: 0,
                n: 0,
                since: 0,
            },
        }
    }

    pub fn shared(&self) -> Arc<SupShared> {
        self.sh.clone()
    }

    pub fn bands(&self) -> usize {
        self.nb
    }

    // шаг из H отсчётов: reff — наушники (моно), mic — микрофон
    pub fn process(&mut self, reff: &[f32], mic: &[f32], out: &mut [f32]) {
        let (n, h, nb) = (self.n, self.h, self.nb);
        self.in_mic.copy_within(h.., 0);
        self.in_ref.copy_within(h.., 0);
        let (mut me, mut re) = (0.0f64, 0.0f64);
        for i in 0..h {
            self.in_mic[n - h + i] = mic[i] as f64;
            self.in_ref[n - h + i] = reff[i] as f64;
            me += mic[i] as f64 * mic[i] as f64;
            re += reff[i] as f64 * reff[i] as f64;
        }
        me /= h as f64;
        re /= h as f64;
        for i in 0..n {
            self.mm[i] = C64::new(self.in_mic[i] * self.win[i], 0.0);
            self.rr[i] = C64::new(self.in_ref[i] * self.win[i], 0.0);
        }
        self.f.forward(&mut self.mm);
        self.f.forward(&mut self.rr);
        self.mb.iter_mut().for_each(|v| *v = 0.0);
        self.rb.iter_mut().for_each(|v| *v = 0.0);
        for k in 0..=n / 2 {
            let b = self.band_of[k];
            self.mb[b] += self.mm[k].norm2();
            self.rb[b] += self.rr[k].norm2();
        }
        // опора: мгновенная атака, спад ~25 мс
        for b in 0..nb {
            if self.rb[b] > self.s[b] {
                self.s[b] = self.rb[b];
            } else {
                self.s[b] = 0.8 * self.s[b] + 0.2 * self.rb[b];
            }
        }
        self.hpos = (self.hpos + 1) % SUP_HIST;
        self.hist[self.hpos].copy_from_slice(&self.s);

        // шумовой пол микрофона
        for b in 0..nb {
            if !self.nf_init || self.mb[b] < self.nf[b] {
                if !self.nf_init {
                    self.nf[b] = self.mb[b];
                } else {
                    self.nf[b] = 0.9 * self.nf[b] + 0.1 * self.mb[b];
                }
            } else {
                self.nf[b] *= 1.002;
            }
        }
        self.nf_init = true;

        let (collecting, cal) = {
            let mut l = self.sh.m.lock().unwrap();
            if l.cal_req > 0 {
                l.cal_left = l.cal_req;
                l.cal_req = 0;
                self.cal_m.clear();
                self.cal_s.clear();
            }
            let collecting = l.cal_left > 0;
            if !collecting {
                if let Some((c, cur)) = l.pending.take() {
                    apply_auto(&mut l, c, cur);
                }
            }
            (collecting, l.cal.clone())
        };

        if collecting {
            self.cal_m.push(self.mb.clone());
            self.cal_s.push(self.s.clone());
            let left = {
                let mut l = self.sh.m.lock().unwrap();
                l.cal_left -= 1;
                l.cal_left
            };
            if left == 0 {
                let mut c = solve_calib(self.rate, nb, &self.cal_m, &self.cal_s);
                c.w0 = c.w.clone();
                {
                    let mut l = self.sh.m.lock().unwrap();
                    l.w0 = c.w.clone();
                    l.d0 = c.d;
                    l.cal = c.clone();
                }
                let _ = self.sh.cal_tx.try_send(c);
            }
        }

        let lv_frame;
        {
            let lagi = (self.hpos + SUP_HIST - cal.d % SUP_HIST) % SUP_HIST;
            let (mut sm, mut sp) = (0.0, 0.0);
            let mut b = 2;
            while b <= 18 && b < nb {
                let mut p = self.nf[b] * VAD_NF_MUL;
                if cal.found && !collecting {
                    p += cal.w[b] * self.hist[lagi][b];
                }
                sm += self.mb[b];
                sp += p;
                b += 1;
            }
            let ratio = sm / (sp + 1e-20);
            let lv = 10.0 * (me + 1e-14).log10();
            lv_frame = lv;
            if !self.bb_init {
                self.bb_floor = lv;
                self.bb_init = true;
            } else if lv < self.bb_floor {
                self.bb_floor += 0.05 * (lv - self.bb_floor);
            } else if lv < self.bb_floor + 9.0 {
                self.bb_floor += 0.004 * (lv - self.bb_floor);
            } else {
                self.bb_floor += 0.0015;
            }
            let mut raw = if ratio > VAD_FULL {
                1.0
            } else if ratio > VAD_START {
                (ratio - VAD_START) / (VAD_FULL - VAD_START)
            } else {
                0.0
            };
            if lv < self.bb_floor + 9.0 {
                raw = 0.0;
            } else if lv < self.bb_floor + 13.0 {
                raw *= (lv - self.bb_floor - 9.0) / 4.0;
            }
            if raw > self.vad {
                self.vad += 0.5 * (raw - self.vad);
            } else {
                self.vad *= 0.985;
            }
        }
        let voice = self.vad;
        self.sh.vad.store(voice.to_bits(), Ordering::Relaxed);
        self.auto_push(re > 1e-6 && !collecting, lv_frame, &cal);
        let beta_eff = self.sh.beta() * (1.0 - 0.25 * voice);

        let gmin = 10f64.powf(self.floor_db / 10.0);
        if cal.found && !collecting {
            let lagi = (self.hpos + SUP_HIST - cal.d % SUP_HIST) % SUP_HIST;
            let learn = voice < 0.1 && re > 1e-9;
            let mut l = if self.adapt && learn { Some(self.sh.m.lock().unwrap()) } else { None };
            for b in 0..nb {
                let lag = self.hist[lagi][b];
                let leak = cal.w[b] * lag;
                let g = (1.0 - beta_eff * leak / (self.mb[b] + 1e-20)).clamp(gmin, 1.0);
                if g < self.g[b] {
                    self.g[b] = g;
                } else {
                    self.g[b] = 0.75 * self.g[b] + 0.25 * g;
                }
                if let Some(l) = l.as_mut() {
                    if lag > 0.0 && b < l.w0.len() {
                        let pred = leak + self.nf[b];
                        if leak > 0.5 * self.nf[b] && self.mb[b] < 4.0 * pred {
                            let target = ((self.mb[b] - self.nf[b]) / lag).max(0.0);
                            let rate = if target < l.cal.w[b] { 0.0015 } else { 0.006 };
                            let mut w = l.cal.w[b] + rate * (target - l.cal.w[b]);
                            let hi = 2.5 * l.w0[b];
                            if w > hi {
                                w = hi;
                            }
                            let lo = 0.7 * l.w0[b];
                            if w < lo {
                                w = lo;
                            }
                            l.cal.w[b] = w;
                            l.adapted = true;
                        }
                    }
                }
            }
        } else {
            self.g.iter_mut().for_each(|v| *v = 1.0);
        }

        // усиление по бинам со сглаживанием между полосами
        for k in 0..=n / 2 {
            let b = self.band_of[k];
            let mut g = self.g[b];
            if b > 0 {
                g = 0.25 * self.g[b - 1] + 0.5 * g;
            } else {
                g *= 0.75;
            }
            if b + 1 < nb {
                g += 0.25 * self.g[b + 1];
            } else {
                g += 0.25 * self.g[b];
            }
            self.gbin[k] = g.sqrt();
        }
        for k in 0..=n / 2 {
            let gb = self.gbin[k];
            self.mm[k] *= gb;
            if k > 0 && k < n / 2 {
                self.mm[n - k] *= gb;
            }
        }
        self.f.inverse(&mut self.mm);
        for i in 0..n {
            self.olap[i] += self.mm[i].re * self.win[i];
        }
        let mut oe = 0.0;
        for i in 0..h {
            let mut v = self.olap[i];
            if collecting {
                v = 0.0;
            }
            v = v.clamp(-1.0, 1.0);
            out[i] = v as f32;
            oe += v * v;
        }
        oe /= h as f64;
        self.olap.copy_within(h.., 0);
        for v in &mut self.olap[n - h..] {
            *v = 0.0;
        }

        let a = 0.9;
        self.s_mic = a * self.s_mic + (1.0 - a) * me;
        self.s_ref = a * self.s_ref + (1.0 - a) * re;
        self.s_out = a * self.s_out + (1.0 - a) * oe;
        if re > 1e-9 && !collecting {
            self.s_da = 0.97 * self.s_da + 0.03 * me;
            self.s_oa = 0.97 * self.s_oa + 0.03 * oe;
        }
        self.sh.m.lock().unwrap().pub_ = Pub {
            mic: self.s_mic,
            r#ref: self.s_ref,
            out: self.s_out,
            vad: self.vad,
            da: self.s_da,
            oa: self.s_oa,
        };
    }

    fn auto_push(&mut self, usable: bool, lv: f64, cur: &Calib) {
        if self.sh.auto_off.load(Ordering::Relaxed) {
            return;
        }
        let a = &mut self.auto;
        a.m[a.pos].copy_from_slice(&self.mb);
        a.s[a.pos].copy_from_slice(&self.s);
        a.r[a.pos].copy_from_slice(&self.rb);
        a.ok[a.pos] = usable;
        a.lv[a.pos] = lv;
        a.pos = (a.pos + 1) % AUTO_BUF;
        if a.n < AUTO_BUF {
            a.n += 1;
        }
        a.since += 1;
        if a.since < AUTO_EVERY || self.sh.auto_busy.load(Ordering::Acquire) {
            return;
        }
        let cnt = a.ok[..a.n].iter().filter(|&&v| v).count();
        if cnt < if cur.found { AUTO_MIN } else { AUTO_MIN / 2 } {
            return;
        }
        a.since = 0;
        let n = a.n;
        let start = (a.pos + AUTO_BUF - n) % AUTO_BUF;
        let mut m = Vec::with_capacity(n);
        let mut sv = Vec::with_capacity(n);
        let mut rv = Vec::with_capacity(n);
        let mut ok = Vec::with_capacity(n);
        let mut lvs = Vec::new();
        for i in 0..n {
            let j = (start + i) % AUTO_BUF;
            m.push(a.m[j].clone());
            sv.push(a.s[j].clone());
            rv.push(a.r[j].clone());
            ok.push(a.ok[j]);
            if a.ok[j] {
                lvs.push(a.lv[j]);
            }
        }
        // голос заметно громче наводки: для поиска задержки громкие кадры не берём
        let gate = pctl(&lvs, 0.2) + 15.0;
        let quiet: Vec<bool> = (0..n).map(|i| ok[i] && a.lv[(start + i) % AUTO_BUF] <= gate).collect();
        let cur = cur.clone();
        let (rate, nb) = (self.rate, self.nb);
        let sh = self.sh.clone();
        sh.auto_busy.store(true, Ordering::Release);
        std::thread::spawn(move || {
            let (c, gain) = solve_model(rate, nb, &m, &sv, &rv, &ok, &quiet, &cur);
            if c.found {
                sh.m.lock().unwrap().pending = Some((c, gain));
            }
            sh.auto_busy.store(false, Ordering::Release);
        });
    }

    // для тестов: дождаться фонового пересчёта
    pub fn auto_busy(&self) -> bool {
        self.sh.auto_busy.load(Ordering::Acquire)
    }
}

// gain — во сколько раз новая модель точнее текущей на свежих данных
fn apply_auto(l: &mut Locked, mut c: Calib, gain: f64) {
    if !l.cal.found || gain > AUTO_SWITCH {
        c.w0 = c.w.clone();
        l.w0 = c.w.clone();
        l.d0 = c.d;
        l.cal = c;
        l.adapted = true;
    }
}

pub fn pctl(x: &[f64], p: f64) -> f64 {
    if x.is_empty() {
        return 0.0;
    }
    let mut y = x.to_vec();
    y.sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
    y[(p * (y.len() - 1) as f64) as usize]
}

pub fn corr_f(x: &[f64], y: &[f64]) -> f64 {
    let n = x.len() as f64;
    if n == 0.0 {
        return 0.0;
    }
    let mx = x.iter().sum::<f64>() / n;
    let my = y.iter().sum::<f64>() / n;
    let (mut sxy, mut sxx, mut syy) = (0.0, 0.0, 0.0);
    for i in 0..x.len() {
        let (a, b) = (x[i] - mx, y[i] - my);
        sxy += a * b;
        sxx += a * a;
        syy += b * b;
    }
    if sxx == 0.0 || syy == 0.0 {
        return 0.0;
    }
    sxy / (sxx * syy).sqrt()
}

fn lg(v: f64) -> f64 {
    (v + 1e-14).log10()
}

fn solve_calib(rate: usize, nb: usize, cm: &[Vec<f64>], cs: &[Vec<f64>]) -> Calib {
    let t = cm.len();
    let mut c = Calib { rate, w: vec![0.0; nb], ..Default::default() };
    if t < 2 * SUP_MAX_D {
        return c;
    }
    let (mut best_d, mut best_c) = (0usize, -1.0f64);
    let mut x = Vec::with_capacity(t);
    let mut y = Vec::with_capacity(t);
    for d in 0..SUP_MAX_D {
        let mut sum = 0.0;
        let mut cnt = 0;
        for b in 1..nb {
            x.clear();
            y.clear();
            for tt in SUP_MAX_D..t {
                x.push(lg(cm[tt][b]));
                y.push(lg(cs[tt - d][b]));
            }
            sum += corr_f(&x, &y);
            cnt += 1;
        }
        let v = sum / cnt as f64;
        if v > best_c {
            best_c = v;
            best_d = d;
        }
    }
    c.d = best_d;
    c.corr = best_c;
    let (mut leak_sum, mut nf_sum) = (0.0, 0.0);
    for b in 0..nb {
        let mcol: Vec<f64> = (SUP_MAX_D..t).map(|tt| cm[tt][b]).collect();
        let nf = pctl(&mcol, 0.15);
        let (mut num, mut den) = (0.0, 0.0);
        for tt in SUP_MAX_D..t {
            let xv = cs[tt - best_d][b];
            let yv = (cm[tt][b] - nf).max(0.0);
            num += xv * yv;
            den += xv * xv;
        }
        if den > 0.0 {
            c.w[b] = num / den;
        }
        let mut pred = 0.0;
        for tt in SUP_MAX_D..t {
            pred += c.w[b] * cs[tt - best_d][b];
        }
        leak_sum += pred;
        nf_sum += nf * (t - SUP_MAX_D) as f64;
    }
    c.leak_db = 10.0 * ((leak_sum + nf_sum) / (nf_sum + 1e-20)).log10();
    c.found = c.corr > 0.3 && c.leak_db > 1.0;
    if !c.found {
        c.w.iter_mut().for_each(|v| *v = 0.0);
    }
    c
}

// подбор модели по обычному звуку: задержка по резким изменениям, уровни — регрессией без кадров с голосом
#[allow(clippy::too_many_arguments)]
fn solve_model(
    rate: usize,
    nb: usize,
    m: &[Vec<f64>],
    sv: &[Vec<f64>],
    rv: &[Vec<f64>],
    ok: &[bool],
    quiet: &[bool],
    cur: &Calib,
) -> (Calib, f64) {
    let mut c = Calib { rate, w: vec![0.0; nb], ..Default::default() };
    let idx: Vec<usize> = (SUP_MAX_D..m.len()).filter(|&t| quiet[t]).collect();
    if idx.len() < AUTO_MIN / 4 {
        return (c, 0.0);
    }

    // задержка
    let lm: Vec<Vec<f64>> = (0..nb).map(|b| m.iter().map(|r| lg(r[b])).collect()).collect();
    let lr: Vec<Vec<f64>> = (0..nb).map(|b| rv.iter().map(|r| lg(r[b])).collect()).collect();
    let mut x = vec![0.0; idx.len()];
    let mut y = vec![0.0; idx.len()];
    let (mut best_d, mut best_c) = (0usize, -1.0f64);
    for d in 0..SUP_MAX_D {
        let mut sum = 0.0;
        for b in 1..nb {
            for (i, &t) in idx.iter().enumerate() {
                x[i] = lm[b][t];
                y[i] = lr[b][t - d];
            }
            sum += corr_f(&x, &y);
        }
        let v = sum / (nb - 1) as f64;
        if v > best_c {
            best_c = v;
            best_d = d;
        }
    }
    c.d = best_d;
    c.corr = best_c;

    // кадры без голоса: микрофон не громче, чем объясняет опора
    // уровни — по свежей части записи, чтобы быстро замечать сдвиг гарнитуры
    let mut all: Vec<usize> = (SUP_MAX_D..m.len()).filter(|&t| ok[t]).collect();
    let recent = all.iter().filter(|&&t| t + AUTO_RECENT >= m.len()).count();
    if recent >= AUTO_MIN / 2 {
        all.retain(|&t| t + AUTO_RECENT >= m.len());
    }
    let tot = |r: &Vec<f64>| r[1..].iter().sum::<f64>();
    let q: Vec<f64> = all.iter().map(|&t| tot(&m[t]) / (tot(&sv[t - best_d]) + 1e-20)).collect();
    let qlim = pctl(&q, 0.3) * 4.0;
    let mut keep: Vec<usize> = all.iter().zip(&q).filter(|(_, &v)| v <= qlim).map(|(&t, _)| t).collect();
    let mut nf = vec![0.0; nb];
    for pass in 0..3 {
        if keep.len() < AUTO_MIN / 6 {
            return (c, 0.0);
        }
        for b in 0..nb {
            let (w, n) = fit_band(m, sv, &keep, best_d, b);
            c.w[b] = w;
            nf[b] = n;
        }
        if pass == 2 {
            break;
        }
        keep.retain(|&t| {
            let pred: f64 = (1..nb).map(|b| c.w[b] * sv[t - best_d][b] + nf[b]).sum();
            tot(&m[t]) <= 3.0 * pred
        });
    }

    // проверка на кадрах, не участвовавших в подборе: две половины по очереди
    let chunk = (keep.len() / 4).max(1);
    let half = |k: usize| -> Vec<usize> { keep.iter().enumerate().filter(|(i, _)| (i / chunk) % 2 == k).map(|(_, &t)| t).collect() };
    let parts = [half(0), half(1)];
    let err = |w: &[f64], n: &[f64], d: usize, on: &[usize]| -> f64 {
        let mut e = 0.0;
        for &t in on {
            for b in 1..nb {
                let v = m[t][b] - n[b] - w[b] * sv[t - d][b];
                e += v * v;
            }
        }
        e
    };
    let cv = |d: usize| -> f64 {
        let mut e = 0.0;
        for k in 0..2 {
            let fit: Vec<(f64, f64)> = (0..nb).map(|b| fit_band(m, sv, &parts[k], d, b)).collect();
            let w: Vec<f64> = fit.iter().map(|v| v.0).collect();
            let n: Vec<f64> = fit.iter().map(|v| v.1).collect();
            e += err(&w, &n, d, &parts[1 - k]);
        }
        e
    };
    let cur_ok = cur.found && cur.w.len() == nb && cur.d <= SUP_MAX_D;
    let mut cands = vec![best_d, best_d.saturating_sub(1), (best_d + 1).min(SUP_MAX_D - 1)];
    if cur_ok {
        cands.push(cur.d);
    }
    cands.dedup();
    let (mut pick, mut pick_e) = (best_d, f64::INFINITY);
    for &d in &cands {
        let e = cv(d);
        if e < pick_e {
            pick_e = e;
            pick = d;
        }
    }
    if pick != best_d {
        c.d = pick;
        for b in 0..nb {
            let (w, n) = fit_band(m, sv, &keep, pick, b);
            c.w[b] = w;
            nf[b] = n;
        }
    }

    let (mut leak_sum, mut nf_sum) = (0.0, 0.0);
    for b in 0..nb {
        for &t in &keep {
            leak_sum += c.w[b] * sv[t - c.d][b];
        }
        nf_sum += nf[b] * keep.len() as f64;
    }
    c.leak_db = 10.0 * ((leak_sum + nf_sum) / (nf_sum + 1e-20)).log10();
    // шум модели не может быть заметно выше самых тихих кадров: иначе в подбор попал голос
    let lows: Vec<f64> = m[SUP_MAX_D..].iter().map(tot).collect();
    let clean = c.leak_db > 6.0 || nf[1..].iter().sum::<f64>() <= 4.0 * pctl(&lows, 0.1);
    c.found = c.corr > 0.3 && c.leak_db > if cur_ok { 3.0 } else { 1.0 } && clean;
    let gain = if cur_ok { err(&cur.w, &nf, cur.d, &keep) / (pick_e + 1e-30) } else { f64::INFINITY };
    (c, gain)
}

// m ≈ w·s + n по одной полосе, w и n неотрицательные
fn fit_band(m: &[Vec<f64>], sv: &[Vec<f64>], keep: &[usize], d: usize, b: usize) -> (f64, f64) {
    let k = keep.len() as f64;
    let (mut sx, mut sy, mut sxx, mut sxy) = (0.0, 0.0, 0.0, 0.0);
    for &t in keep {
        let (xv, yv) = (sv[t - d][b], m[t][b]);
        sx += xv;
        sy += yv;
        sxx += xv * xv;
        sxy += xv * yv;
    }
    let den = sxx - sx * sx / k;
    let mut w = if den > 0.0 { (sxy - sx * sy / k) / den } else { 0.0 };
    w = w.max(0.0);
    let mut n = (sy - w * sx) / k;
    if n < 0.0 {
        n = 0.0;
        w = if sxx > 0.0 { sxy / sxx } else { 0.0 };
    }
    (w, n)
}
