use super::f64_cell;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;

// авто-громкость

pub struct AgcCtl {
    enabled: AtomicBool,
    gain: AtomicU64,
}

impl AgcCtl {
    pub fn set_enabled(&self, on: bool) {
        self.enabled.store(on, Ordering::Relaxed);
    }
    pub fn enabled(&self) -> bool {
        self.enabled.load(Ordering::Relaxed)
    }
    pub fn gain_db(&self) -> f64 {
        f64::from_bits(self.gain.load(Ordering::Relaxed))
    }
}

pub struct Agc {
    ctl: Arc<AgcCtl>,
    pub target_db: f64,
    pub max_up_db: f64,
    pub max_dn_db: f64,
    gain: f64,
    floor: f64,
    hold: i32,
}

impl Default for Agc {
    fn default() -> Self {
        Self::new()
    }
}

impl Agc {
    pub fn new() -> Self {
        Agc {
            ctl: Arc::new(AgcCtl { enabled: AtomicBool::new(false), gain: f64_cell(0.0) }),
            target_db: -23.0,
            max_up_db: 18.0,
            max_dn_db: -9.0,
            gain: 0.0,
            floor: -70.0,
            hold: 0,
        }
    }

    pub fn ctl(&self) -> Arc<AgcCtl> {
        self.ctl.clone()
    }

    pub fn process(&mut self, x: &mut [f32]) {
        let sum: f64 = x.iter().map(|&v| v as f64 * v as f64).sum();
        let lvl = 10.0 * (sum / x.len() as f64 + 1e-12).log10();
        if lvl < self.floor {
            self.floor = 0.7 * self.floor + 0.3 * lvl;
        } else {
            self.floor += 0.0008 * (lvl - self.floor);
        }
        let prev = self.gain;
        if !self.ctl.enabled() {
            self.gain *= 0.97;
            if self.gain.abs() < 0.01 {
                self.gain = 0.0;
            }
        } else {
            let speech = lvl > self.floor + 9.0 && lvl > -52.0;
            if speech {
                self.hold = 40;
            } else if self.hold > 0 {
                self.hold -= 1;
            }
            if speech {
                let want = (self.target_db - lvl).min(self.max_up_db).max(self.max_dn_db);
                if want < self.gain {
                    self.gain += 0.25 * (want - self.gain);
                } else {
                    self.gain += 0.004 * (want - self.gain);
                }
            }
        }
        self.ctl.gain.store(self.gain.to_bits(), Ordering::Relaxed);
        if prev == 0.0 && self.gain == 0.0 {
            return;
        }
        let (g0, g1) = (10f64.powf(prev / 20.0), 10f64.powf(self.gain / 20.0));
        let n = x.len() as f64;
        for (i, v) in x.iter_mut().enumerate() {
            let g = g0 + (g1 - g0) * (i + 1) as f64 / n;
            *v = (*v as f64 * g) as f32;
        }
    }
}
