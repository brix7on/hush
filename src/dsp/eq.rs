use super::RATE;
use std::sync::atomic::{AtomicI32, Ordering};
use std::sync::Arc;

#[derive(Clone, Copy)]
struct Biquad {
    b0: f64,
    b1: f64,
    b2: f64,
    a1: f64,
    a2: f64,
    z1: f64,
    z2: f64,
}

impl Biquad {
    fn new(b0: f64, b1: f64, b2: f64, a1: f64, a2: f64) -> Self {
        Biquad { b0, b1, b2, a1, a2, z1: 0.0, z2: 0.0 }
    }
    #[inline]
    fn proc(&mut self, x: f64) -> f64 {
        let y = self.b0 * x + self.z1;
        self.z1 = self.b1 * x - self.a1 * y + self.z2;
        self.z2 = self.b2 * x - self.a2 * y;
        y
    }
}

fn hpf(f: f64, qf: f64) -> Biquad {
    let w = 2.0 * std::f64::consts::PI * f / RATE as f64;
    let al = w.sin() / (2.0 * qf);
    let c = w.cos();
    let a0 = 1.0 + al;
    Biquad::new((1.0 + c) / 2.0 / a0, -(1.0 + c) / a0, (1.0 + c) / 2.0 / a0, -2.0 * c / a0, (1.0 - al) / a0)
}

fn peak(f: f64, qf: f64, gain_db: f64) -> Biquad {
    let a = 10f64.powf(gain_db / 40.0);
    let w = 2.0 * std::f64::consts::PI * f / RATE as f64;
    let al = w.sin() / (2.0 * qf);
    let c = w.cos();
    let a0 = 1.0 + al / a;
    Biquad::new((1.0 + al * a) / a0, -2.0 * c / a0, (1.0 - al * a) / a0, -2.0 * c / a0, (1.0 - al / a) / a0)
}

fn high_shelf(f: f64, gain_db: f64) -> Biquad {
    let a = 10f64.powf(gain_db / 40.0);
    let w = 2.0 * std::f64::consts::PI * f / RATE as f64;
    let (c, s) = (w.cos(), w.sin());
    let al = s / 2.0 * 2f64.sqrt();
    let sq = 2.0 * a.sqrt() * al;
    let a0 = (a + 1.0) - (a - 1.0) * c + sq;
    Biquad::new(
        a * ((a + 1.0) + (a - 1.0) * c + sq) / a0,
        -2.0 * a * ((a - 1.0) + (a + 1.0) * c) / a0,
        a * ((a + 1.0) + (a - 1.0) * c - sq) / a0,
        2.0 * ((a - 1.0) - (a + 1.0) * c) / a0,
        ((a + 1.0) - (a - 1.0) * c - sq) / a0,
    )
}

pub const EQ_OFF: i32 = 0;
pub const EQ_CLEAR: i32 = 1;
pub const EQ_SOFT: i32 = 2;
const EQ_COUNT: i32 = 3;

pub struct EqCtl {
    preset: AtomicI32,
}

impl EqCtl {
    pub fn set_preset(&self, p: i32) {
        let p = if (0..EQ_COUNT).contains(&p) { p } else { 0 };
        self.preset.store(p, Ordering::Relaxed);
    }
    pub fn preset(&self) -> i32 {
        self.preset.load(Ordering::Relaxed)
    }
}

pub struct Eq {
    ctl: Arc<EqCtl>,
    cur: i32,
    chain: Vec<Biquad>,
}

impl Default for Eq {
    fn default() -> Self {
        Self::new()
    }
}

impl Eq {
    pub fn new() -> Self {
        Eq { ctl: Arc::new(EqCtl { preset: AtomicI32::new(0) }), cur: 0, chain: Vec::new() }
    }

    pub fn ctl(&self) -> Arc<EqCtl> {
        self.ctl.clone()
    }

    fn build(&mut self, p: i32) {
        self.chain = match p {
            EQ_CLEAR => vec![hpf(90.0, 0.707), peak(250.0, 1.0, -2.5), peak(3200.0, 0.9, 3.0), high_shelf(9000.0, -1.5)],
            EQ_SOFT => vec![hpf(80.0, 0.707), peak(5500.0, 1.2, -2.5), high_shelf(8000.0, -4.0)],
            _ => Vec::new(),
        };
    }

    pub fn process(&mut self, x: &mut [f32]) {
        let p = self.ctl.preset();
        if p != self.cur {
            self.cur = p;
            self.build(p);
        }
        if self.chain.is_empty() {
            return;
        }
        for v in x.iter_mut() {
            let mut y = *v as f64;
            for q in self.chain.iter_mut() {
                y = q.proc(y);
            }
            *v = y as f32;
        }
    }
}
