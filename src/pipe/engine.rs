use super::{StreamIn, RATE};
use crate::dsp::{agc::Agc, amp::Amp, denoise::Denoiser, eq::Eq, suppress::Suppressor};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

// цепочка обработки живёт между сессиями
pub struct Chain {
    pub sup: Suppressor,
    pub dn: Denoiser,
    pub eq: Eq,
    pub agc: Agc,
    pub amp: Amp,
}

#[derive(Default)]
pub struct EngineStats {
    pub out_db: AtomicU64,
}

impl EngineStats {
    pub fn out_db(&self) -> f64 {
        let v = f64::from_bits(self.out_db.load(Ordering::Relaxed));
        if v == 0.0 {
            -120.0
        } else {
            v
        }
    }
}

pub struct Engine {
    pub mic: Arc<StreamIn>,
    pub reff: Arc<StreamIn>,
    pub chain: Arc<Mutex<Chain>>,
    pub bypass: bool,
    pub open: Option<Arc<AtomicBool>>,
    pub stats: Arc<EngineStats>,
    pub hold: i64,
    pub ref_stale: i64,
    pub b: usize,
    next: i64,
    started: bool,
    rbs: Vec<Vec<f32>>,
    mb: Vec<f32>,
    ob: Vec<f32>,
    smono: Vec<f32>,
    gate: f64,
    pub first: i64,
    pub blocks: usize,
    pub waited: usize,
    pub late_ref: usize,
    pub late_max_ms: f64,
    wait_from: i64,
}

impl Engine {
    pub fn new(mic: Arc<StreamIn>, reff: Arc<StreamIn>, chain: Arc<Mutex<Chain>>, refs: usize) -> Self {
        let h = chain.lock().unwrap().sup.h;
        Engine {
            mic,
            reff,
            chain,
            bypass: false,
            open: None,
            stats: Arc::new(EngineStats::default()),
            hold: RATE as i64 * 31 / 1000,
            ref_stale: RATE as i64 * 35 / 1000,
            b: h,
            next: 0,
            started: false,
            rbs: vec![vec![0.0; h]; refs.max(1)],
            mb: vec![0.0; h],
            ob: vec![0.0; h],
            smono: vec![0.0; h],
            gate: 1.0,
            first: 0,
            blocks: 0,
            waited: 0,
            late_ref: 0,
            late_max_ms: 0.0,
            wait_from: 0,
        }
    }

    // обрабатывает все готовые блоки; now_idx — «сейчас» на шкале
    pub fn step(&mut self, now_idx: i64, out: &mut Vec<f32>) {
        let b = self.b as i64;
        loop {
            let Some(mic_end) = self.mic.end() else { return };
            if !self.started {
                self.next = mic_end - mic_end.rem_euclid(b);
                if self.next < mic_end {
                    self.next += b;
                }
                self.first = self.next;
                self.started = true;
            }
            let s = self.next;
            if mic_end < s + b {
                return;
            }
            // опора читается со сдвигом: шкалы микрофона и наушников уходят от меток времени по-разному
            let lm = self.mic.lag_at(s);
            let max_off = RATE as i64 * 80 / 1000;
            let rs = s + (lm - self.reff.lag_at(s)).clamp(-max_off, max_off);
            let ref_end = self.reff.end();
            if ref_end.map_or(true, |e| e < rs + b) {
                let alive = self.ref_stale <= 0
                    || ref_end.is_some_and(|e| now_idx - (e - self.reff.cur_delay()) <= self.ref_stale);
                if alive && now_idx < s + lm + b + self.hold {
                    self.waited += 1;
                    if self.wait_from == 0 {
                        self.wait_from = now_idx;
                    }
                    return;
                }
            }
            if self.wait_from != 0 {
                let w = now_idx - self.wait_from;
                if w > RATE as i64 * 4 / 1000 {
                    self.late_ref += 1;
                    let ms = w as f64 * 1000.0 / RATE as f64;
                    if ms > self.late_max_ms {
                        self.late_max_ms = ms;
                    }
                }
                self.wait_from = 0;
            }
            self.mic.read_at(0, s, &mut self.mb);
            for (k, rb) in self.rbs.iter_mut().enumerate() {
                self.reff.read_at(k, rs, rb);
            }
            {
                let mut ch = self.chain.lock().unwrap();
                let c = &mut *ch;
                if self.bypass {
                    self.ob.copy_from_slice(&self.mb);
                } else {
                    let inv = 1.0 / self.rbs.len() as f32;
                    for i in 0..self.smono.len() {
                        self.smono[i] = self.rbs.iter().map(|r| r[i]).sum::<f32>() * inv;
                    }
                    c.sup.process(&self.smono, &self.mb, &mut self.ob);
                    c.dn.ctl().voice_hint(c.sup.shared().voice_level());
                    c.dn.process(&mut self.ob);
                    c.eq.process(&mut self.ob);
                    c.agc.process(&mut self.ob);
                }
                c.amp.process(&mut self.ob);
            }
            self.apply_gate();
            self.track_out();
            out.extend_from_slice(&self.ob);
            self.next += b;
            self.blocks += 1;
        }
    }

    pub fn next(&self) -> Option<i64> {
        self.started.then_some(self.next)
    }

    // собственная задержка обработки, отсчётов 48 кГц
    pub fn algo_delay(&self) -> i64 {
        if self.bypass {
            return 0;
        }
        let c = self.chain.lock().unwrap();
        (c.sup.n - c.sup.h + c.dn.delay()) as i64
    }

    fn track_out(&mut self) {
        let sum: f64 = self.ob.iter().map(|&v| v as f64 * v as f64).sum();
        let db = 10.0 * (sum / self.ob.len() as f64 + 1e-12).log10();
        let mut cur = f64::from_bits(self.stats.out_db.load(Ordering::Relaxed));
        if cur == 0.0 {
            cur = -120.0;
        }
        cur = if db > cur { db } else { 0.92 * cur + 0.08 * db };
        self.stats.out_db.store(cur.to_bits(), Ordering::Relaxed);
    }

    // плавно (~10 мс) открывает/закрывает микрофон по кнопке
    fn apply_gate(&mut self) {
        let target = match &self.open {
            Some(o) if !o.load(Ordering::Relaxed) => 0.0,
            _ => 1.0,
        };
        if self.gate == target && target == 1.0 {
            return;
        }
        const STEP: f64 = 1.0 / 480.0;
        for v in self.ob.iter_mut() {
            if self.gate < target {
                self.gate = (self.gate + STEP).min(target);
            } else if self.gate > target {
                self.gate = (self.gate - STEP).max(target);
            }
            *v = (*v as f64 * self.gate) as f32;
        }
    }
}
