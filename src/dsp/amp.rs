use super::{f64_cell, RATE};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

// усилитель с ограничителем

pub struct AmpCtl {
    gain: AtomicU64,
    lim: AtomicU64,
}

impl AmpCtl {
    pub fn set_gain_db(&self, db: f64) {
        self.gain.store(db.to_bits(), Ordering::Relaxed);
    }
    pub fn gain_db(&self) -> f64 {
        f64::from_bits(self.gain.load(Ordering::Relaxed))
    }
    pub fn limiting(&self) -> f64 {
        f64::from_bits(self.lim.load(Ordering::Relaxed))
    }
}

pub struct Amp {
    ctl: Arc<AmpCtl>,
    pub ceil: f64,
    lim: f64,
    release: f64,
}

impl Amp {
    pub fn new(gain_db: f64) -> Self {
        Amp {
            ctl: Arc::new(AmpCtl { gain: f64_cell(gain_db), lim: f64_cell(0.0) }),
            ceil: 0.89,
            lim: 1.0,
            release: 10f64.powf(10.0 / 20.0 / (0.3 * RATE as f64)),
        }
    }

    pub fn ctl(&self) -> Arc<AmpCtl> {
        self.ctl.clone()
    }

    pub fn process(&mut self, x: &mut [f32]) {
        let db = self.ctl.gain_db();
        if db == 0.0 && self.lim == 1.0 {
            self.ctl.lim.store(0f64.to_bits(), Ordering::Relaxed);
            return;
        }
        let g = 10f64.powf(db / 20.0);
        let mut min_lim = 1.0f64;
        for v in x.iter_mut() {
            let mut y = *v as f64 * g;
            if y.abs() * self.lim > self.ceil {
                self.lim = self.ceil / y.abs();
            }
            y *= self.lim;
            min_lim = min_lim.min(self.lim);
            self.lim = (self.lim * self.release).min(1.0);
            *v = y as f32;
        }
        self.ctl.lim.store((-20.0 * min_lim.log10()).to_bits(), Ordering::Relaxed);
    }
}
