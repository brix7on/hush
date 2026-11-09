use super::RATE;
use crate::audio::Resampler;
use std::time::Duration;

// поток 48 кГц в частоту устройства вывода с запасом буфера

const QUIET_LEVEL: f64 = 1e-3;
const QUIET_OVER_FLOOR: f64 = 2.5;

pub struct Sink {
    rs: Resampler,
    q: Vec<f32>,
    rate: usize,
    pub target: i64,
    pub base: i64,
    pub drops: usize,
    pub quiet_slack: i64,
    pub quiet_drops: i64,
    floor: f64,
    sub_min: [f64; 8],
    out: Vec<f32>,
    fill_cur: i64,
    fill_prev: i64,
    fill_n: usize,
    fill_ready: bool,
    sub_idx: usize,
    sub_cnt: usize,
    starve_t: Option<Duration>,
    calm_t: Duration,
    started: bool,
    pub starves: usize,
}

pub fn mic_base_ms(_period_ms: f64) -> f64 {
    5.0
}

impl Sink {
    pub fn new(dev_rate: usize, target_ms: usize) -> Self {
        let t = (dev_rate * target_ms / 1000) as i64;
        Sink {
            rs: Resampler::new(RATE, dev_rate, true),
            q: Vec::new(),
            rate: dev_rate,
            target: t,
            base: t,
            drops: 0,
            quiet_slack: 0,
            quiet_drops: 0,
            floor: 0.0,
            sub_min: [0.0; 8],
            out: Vec::new(),
            fill_cur: 0,
            fill_prev: 0,
            fill_n: 0,
            fill_ready: false,
            sub_idx: 0,
            sub_cnt: 0,
            starve_t: None,
            calm_t: Duration::ZERO,
            started: false,
            starves: 0,
        }
    }

    pub fn rate(&self) -> usize {
        self.rate
    }

    pub fn set_base_ms(&mut self, ms: f64) {
        let nb = (self.rate as f64 * ms / 1000.0 + 0.5) as i64;
        if nb < 1 || nb == self.base {
            return;
        }
        if self.target <= self.base || self.target < nb {
            self.target = nb;
        }
        self.base = nb;
    }

    pub fn queued(&self) -> usize {
        self.q.len()
    }

    pub fn adapt(&mut self, padding: usize, now: Duration) {
        let r = self.rate as i64;
        if !self.started {
            self.started = true;
            self.starve_t = None;
            self.calm_t = now;
        }
        let past = |t: Option<Duration>, d: Duration| t.map_or(true, |t| now.saturating_sub(t) > d);
        if padding == 0 && now > Duration::from_secs(2) && past(self.starve_t, Duration::from_millis(300)) {
            self.starves += 1;
            self.starve_t = Some(now);
            if self.target < r * 60 / 1000 {
                self.target += r * 3 / 1000;
            }
            self.calm_t = now;
        } else if self.target > self.base
            && past(self.starve_t, Duration::from_secs(60))
            && now.saturating_sub(self.calm_t) > Duration::from_secs(60)
        {
            self.target = (self.target - r / 1000).max(self.base);
            self.calm_t = now;
        }
    }

    pub fn feed(&mut self, mono48: &[f32]) {
        const WIN: usize = 240;
        let mut i = 0;
        while i + WIN <= mono48.len() {
            let e: f64 = mono48[i..i + WIN].iter().map(|&v| v as f64 * v as f64).sum();
            i += WIN;
            let r = (e / WIN as f64).sqrt();
            if r < 1e-5 {
                continue;
            }
            if self.sub_min[self.sub_idx] == 0.0 || r < self.sub_min[self.sub_idx] {
                self.sub_min[self.sub_idx] = r;
            }
            self.sub_cnt += 1;
            if self.sub_cnt >= 40 {
                self.sub_cnt = 0;
                self.sub_idx = (self.sub_idx + 1) % self.sub_min.len();
                self.sub_min[self.sub_idx] = 0.0;
            }
            self.floor = 0.0;
            for &m in &self.sub_min {
                if m > 0.0 && (self.floor == 0.0 || m < self.floor) {
                    self.floor = m;
                }
            }
        }
        self.rs.process(mono48, &mut self.q);
    }

    fn drop_quiet(&mut self, want: i64) -> i64 {
        let fade = (self.rate / 500) as i64;
        let min_drop = (self.rate / 1000) as i64;
        if want < min_drop {
            return 0;
        }
        let mut want = want.min((self.rate * 10 / 1000) as i64);
        let ql = self.q.len() as i64;
        if ql < want + fade {
            want = ql - fade;
            if want < min_drop {
                return 0;
            }
        }
        let thr = QUIET_LEVEL.max(self.floor * QUIET_OVER_FLOOR);
        let span = (want + fade) as usize;
        let e: f64 = self.q[..span].iter().map(|&v| v as f64 * v as f64).sum();
        if (e / span as f64).sqrt() > thr {
            return 0;
        }
        let (w0, f) = (want as usize, fade as usize);
        for i in 0..f {
            let w = (i + 1) as f32 / (f + 1) as f32;
            self.q[w0 + i] = self.q[i] * (1.0 - w) + self.q[w0 + i] * w;
        }
        self.q.drain(..w0);
        want
    }

    // padding — уже в буфере устройства, free — сколько ещё влезет
    pub fn take(&mut self, padding: usize, free: usize) -> &[f32] {
        let r = self.rate as i64;
        let fill = (padding + self.q.len()) as i64;
        if self.fill_n == 0 || fill < self.fill_cur {
            self.fill_cur = fill;
        }
        self.fill_n += 1;
        if self.fill_n >= 100 {
            self.fill_prev = self.fill_cur;
            self.fill_n = 0;
            self.fill_ready = true;
        }
        let mut min_fill = self.fill_cur;
        if self.fill_ready && self.fill_prev < min_fill {
            min_fill = self.fill_prev;
        }
        let mut excess = min_fill - self.target;
        if self.quiet_slack > 0 && self.fill_ready && excess > self.quiet_slack {
            let n = self.drop_quiet(excess - r / 1000);
            if n > 0 {
                self.quiet_drops += n;
                self.fill_cur -= n;
                self.fill_prev -= n;
                excess -= n;
            }
        }
        if self.fill_ready {
            let err = (excess as f64 / self.target as f64).clamp(-1.0, 1.0);
            self.rs.set_step_scale(1.0 + 0.002 * err);
        }
        if self.fill_ready && excess > r * 80 / 1000 {
            let drop = ((excess - r * 10 / 1000) as usize).min(self.q.len());
            self.q.drain(..drop);
            self.fill_cur -= drop as i64;
            self.fill_prev -= drop as i64;
            self.drops += 1;
        }
        let mut n = (self.q.len() as i64).min(free as i64);
        let limit = self.target + r * 3 / 1000 - padding as i64;
        if n > limit {
            n = limit;
        }
        if n <= 0 {
            return &[];
        }
        let n = n as usize;
        self.out.clear();
        self.out.extend_from_slice(&self.q[..n]);
        self.q.drain(..n);
        &self.out
    }

    // сколько движку можно ждать запоздавший звук наушников, отсчётов 48 кГц
    pub fn hold_for(&self, padding: usize, max_hold: i64) -> i64 {
        let fill = (padding + self.q.len()) as i64 * RATE as i64 / self.rate as i64;
        (fill - RATE as i64 * 2 / 1000).clamp(0, max_hold)
    }
}
