use super::{f64_cell, Fft, C64};
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU64, Ordering};
use std::sync::Arc;

// шумодав: убирает стационарный фон

const DN_SUB: usize = 8;
const DN_SUB_LEN: usize = 48;

pub struct DnCtl {
    strength: AtomicI32,
    hint: AtomicU64,
    has_hint: AtomicBool,
}

impl DnCtl {
    // 0 выкл, 1 лёгкий, 2 средний, 3 сильный, 4 максимум
    pub fn set_strength(&self, s: i32) {
        self.strength.store(s.clamp(0, 4), Ordering::Relaxed);
    }
    pub fn strength(&self) -> i32 {
        self.strength.load(Ordering::Relaxed)
    }
    pub fn voice_hint(&self, v: f64) {
        self.has_hint.store(true, Ordering::Relaxed);
        self.hint.store(v.to_bits(), Ordering::Relaxed);
    }
}

pub struct Denoiser {
    pub n: usize,
    pub h: usize,
    ctl: Arc<DnCtl>,
    f: Fft,
    win: Vec<f64>,
    inp: Vec<f64>,
    olap: Vec<f64>,
    x: Vec<C64>,
    ps: Vec<f64>,
    noise: Vec<f64>,
    sub_min: Vec<Vec<f64>>,
    cur: Vec<f64>,
    sub_cnt: usize,
    sub_idx: usize,
    g_prev: Vec<f64>,
    p_prev: Vec<f64>,
    g: Vec<f64>,
    inited: bool,
    hang: i32,
    gate: f64,
    raw: Vec<f32>,
    mix: f32,
}

impl Default for Denoiser {
    fn default() -> Self {
        Self::new()
    }
}

impl Denoiser {
    pub fn new() -> Self {
        let (n, h) = (256usize, 128usize);
        let nb = n / 2 + 1;
        Denoiser {
            n,
            h,
            ctl: Arc::new(DnCtl {
                strength: AtomicI32::new(0),
                hint: f64_cell(0.0),
                has_hint: AtomicBool::new(false),
            }),
            f: Fft::new(n),
            win: (0..n)
                .map(|i| (0.5 - 0.5 * (2.0 * std::f64::consts::PI * i as f64 / n as f64).cos()).sqrt())
                .collect(),
            inp: vec![0.0; n],
            olap: vec![0.0; n],
            x: vec![C64::ZERO; n],
            ps: vec![0.0; nb],
            noise: vec![0.0; nb],
            sub_min: vec![vec![f64::INFINITY; nb]; DN_SUB],
            cur: vec![f64::INFINITY; nb],
            sub_cnt: 0,
            sub_idx: 0,
            g_prev: vec![1.0; nb],
            p_prev: vec![0.0; nb],
            g: vec![0.0; nb],
            inited: false,
            hang: 0,
            gate: 1.0,
            raw: vec![0.0; h],
            mix: 1.0,
        }
    }

    pub fn ctl(&self) -> Arc<DnCtl> {
        self.ctl.clone()
    }

    fn floor_db(&self) -> f64 {
        match self.ctl.strength() {
            1 => -9.0,
            2 => -15.0,
            3 => -26.0,
            _ => -36.0,
        }
    }

    fn params(&self) -> (f64, f64) {
        match self.ctl.strength() {
            3 => (1.4, -18.0),
            4 => (1.8, -40.0),
            _ => (1.0, 0.0),
        }
    }

    pub fn delay(&self) -> usize {
        if self.ctl.strength() == 0 {
            0
        } else {
            self.n - self.h
        }
    }

    // обрабатывает на месте кусок длиной, кратной H
    pub fn process(&mut self, x: &mut [f32]) {
        let h = self.h;
        let mut i = 0;
        while i + h <= x.len() {
            let seg = &mut x[i..i + h];
            self.raw.copy_from_slice(seg);
            self.hop(seg);
            let target: f32 = if self.ctl.strength() != 0 { 1.0 } else { 0.0 };
            if self.mix == target {
                if target == 0.0 {
                    seg.copy_from_slice(&self.raw);
                }
                i += h;
                continue;
            }
            let step = 1.0 / h as f32;
            for j in 0..h {
                if self.mix < target {
                    self.mix = target.min(self.mix + step);
                } else if self.mix > target {
                    self.mix = target.max(self.mix - step);
                }
                seg[j] = self.mix * seg[j] + (1.0 - self.mix) * self.raw[j];
            }
            i += h;
        }
    }

    fn hop(&mut self, x: &mut [f32]) {
        let (n, h) = (self.n, self.h);
        self.inp.copy_within(h.., 0);
        for i in 0..h {
            self.inp[n - h + i] = x[i] as f64;
        }
        for i in 0..n {
            self.x[i] = C64::new(self.inp[i] * self.win[i], 0.0);
        }
        self.f.forward(&mut self.x);
        let nb = n / 2 + 1;
        const EPS: f64 = 1e-14;
        for k in 0..nb {
            let p = self.x[k].norm2();
            if !self.inited {
                self.ps[k] = p;
            } else {
                self.ps[k] = 0.84 * self.ps[k] + 0.16 * p;
            }
            if self.ps[k] < self.cur[k] {
                self.cur[k] = self.ps[k];
            }
        }
        self.sub_cnt += 1;
        if self.sub_cnt >= DN_SUB_LEN {
            self.sub_min[self.sub_idx].copy_from_slice(&self.cur);
            self.sub_idx = (self.sub_idx + 1) % DN_SUB;
            self.cur.iter_mut().for_each(|v| *v = f64::INFINITY);
            self.sub_cnt = 0;
        }
        for k in 0..nb {
            let mut m = self.cur[k];
            for i in 0..DN_SUB {
                if self.sub_min[i][k] < m {
                    m = self.sub_min[i][k];
                }
            }
            if m.is_infinite() {
                m = self.ps[k];
            }
            let nn = m * 3.0;
            if !self.inited {
                self.noise[k] = nn;
            } else if nn > self.noise[k] {
                self.noise[k] += 0.025 * (nn - self.noise[k]);
            } else {
                self.noise[k] = nn;
            }
        }
        self.inited = true;

        let st = self.ctl.strength();
        let gmin = 10f64.powf(self.floor_db() / 20.0);
        let (over, gate_db) = self.params();

        let (mut sp, mut sn) = (0.0, 0.0);
        for k in 1..nb.min(22) {
            sp += self.x[k].norm2();
            sn += self.noise[k];
        }
        let mut speech = sp > 2.2 * sn + EPS;
        let hint = f64::from_bits(self.ctl.hint.load(Ordering::Relaxed));
        if hint >= 0.0 && self.ctl.has_hint.load(Ordering::Relaxed) {
            speech = speech && hint > 0.25;
        }
        if speech {
            self.hang = 80;
        } else if self.hang > 0 {
            self.hang -= 1;
        }
        let mut gate_t = 1.0;
        if gate_db < 0.0 && self.hang == 0 {
            gate_t = 10f64.powf(gate_db / 20.0);
        }
        if gate_t > self.gate {
            self.gate += 0.3 * (gate_t - self.gate);
        } else {
            self.gate += 0.02 * (gate_t - self.gate);
        }
        for k in 0..nb {
            let lam = over * self.noise[k] + EPS;
            let p = self.x[k].norm2();
            let gamma = p / lam;
            let xi = (0.9 * self.g_prev[k] * self.g_prev[k] * self.p_prev[k] / lam + 0.1 * (gamma - 1.0).max(0.0))
                .max(1e-3);
            self.g[k] = xi / (1.0 + xi);
        }
        for k in 0..nb {
            let a = self.g[k.saturating_sub(1)];
            let b = self.g[k];
            let c = self.g[(k + 1).min(nb - 1)];
            let mut g = 0.25 * a + 0.5 * b + 0.25 * c;
            if g < self.g_prev[k] * 0.92 {
                g = self.g_prev[k] * 0.92;
            }
            g = g.max(gmin).min(1.0);
            self.p_prev[k] = self.x[k].norm2();
            self.g_prev[k] = g;
            self.g[k] = g;
        }
        if st == 0 {
            self.g.iter_mut().for_each(|v| *v = 1.0);
        } else if self.gate < 0.999 {
            let gt = self.gate;
            self.g.iter_mut().for_each(|v| *v *= gt);
        }
        for k in 0..nb {
            let g = self.g[k];
            self.x[k] *= g;
            if k > 0 && k < n / 2 {
                self.x[n - k] *= g;
            }
        }
        self.f.inverse(&mut self.x);
        for i in 0..n {
            self.olap[i] += self.x[i].re * self.win[i];
        }
        for i in 0..h {
            x[i] = self.olap[i].clamp(-1.0, 1.0) as f32;
        }
        self.olap.copy_within(h.., 0);
        for v in &mut self.olap[n - h..] {
            *v = 0.0;
        }
    }
}
