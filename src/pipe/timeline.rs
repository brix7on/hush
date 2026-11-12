use super::RATE;
use std::sync::{Arc, Mutex};

struct TlInner {
    buf: Vec<f32>,
    end: i64,
    first: i64,
    set: bool,
}

// кольцевой буфер моно-сигнала с абсолютной нумерацией отсчётов
pub struct Timeline {
    size: i64,
    m: Mutex<TlInner>,
}

#[inline]
fn wrap(i: i64, size: i64) -> usize {
    i.rem_euclid(size) as usize
}

impl Timeline {
    pub fn new(size: usize) -> Self {
        Timeline { size: size as i64, m: Mutex::new(TlInner { buf: vec![0.0; size], end: 0, first: 0, set: false }) }
    }

    pub fn first(&self) -> Option<i64> {
        let t = self.m.lock().unwrap();
        t.set.then_some(t.first)
    }

    pub fn end(&self) -> Option<i64> {
        let t = self.m.lock().unwrap();
        t.set.then_some(t.end)
    }

    pub fn start(&self, idx: i64) {
        let mut t = self.m.lock().unwrap();
        if !t.set {
            t.end = idx;
            t.first = idx;
            t.set = true;
        }
    }

    pub fn append(&self, s: &[f32]) {
        let mut t = self.m.lock().unwrap();
        for &v in s {
            let i = wrap(t.end, self.size);
            t.buf[i] = v;
            t.end += 1;
        }
    }

    pub fn pad_to(&self, idx: i64) {
        let mut t = self.m.lock().unwrap();
        if idx - t.end > self.size {
            t.end = idx - self.size;
        }
        while t.end < idx {
            let i = wrap(t.end, self.size);
            t.buf[i] = 0.0;
            t.end += 1;
        }
    }

    // d>0 — вставить тишину, d<0 — отступить назад
    pub fn shift(&self, d: i64) {
        let mut t = self.m.lock().unwrap();
        if d > 0 {
            for _ in 0..d.min(self.size) {
                let i = wrap(t.end, self.size);
                t.buf[i] = 0.0;
                t.end += 1;
            }
        } else {
            t.end += d;
        }
    }

    pub fn read_at(&self, start: i64, dst: &mut [f32]) {
        let t = self.m.lock().unwrap();
        for (i, d) in dst.iter_mut().enumerate() {
            let idx = start + i as i64;
            *d = if idx >= t.end || idx < t.end - self.size { 0.0 } else { t.buf[wrap(idx, self.size)] };
        }
    }
}

struct SiInner {
    delay: i64,
    tl: Vec<Arc<Timeline>>,
    resyncs: usize,
    lag: f64,
    old_lag: f64,
    jump_at: i64,
}

// пакеты одного устройства (уже 48 кГц) на общей шкале
pub struct StreamIn {
    pub t0: i64,
    pub tol: i64,
    size: usize,
    m: Mutex<SiInner>,
}

// метка QPC (100 нс) в индекс шкалы
pub fn idx_from_qpc(qpc: i64, t0: i64) -> i64 {
    let d = qpc - t0;
    d / 10_000_000 * RATE as i64 + (d % 10_000_000) * RATE as i64 / 10_000_000
}

impl StreamIn {
    pub fn new(size: usize, t0: i64, delay: i64) -> Self {
        StreamIn {
            t0,
            tol: RATE as i64 * 40 / 1000,
            size,
            m: Mutex::new(SiInner { delay, tl: Vec::new(), resyncs: 0, lag: 0.0, old_lag: 0.0, jump_at: i64::MIN }),
        }
    }

    fn timelines(&self) -> Vec<Arc<Timeline>> {
        self.m.lock().unwrap().tl.clone()
    }

    pub fn channels(&self) -> usize {
        self.m.lock().unwrap().tl.len()
    }

    pub fn end(&self) -> Option<i64> {
        let t = self.m.lock().unwrap().tl.first().cloned();
        t.and_then(|t| t.end())
    }

    pub fn first(&self) -> Option<i64> {
        let t = self.m.lock().unwrap().tl.first().cloned();
        t.and_then(|t| t.first())
    }

    pub fn read_at(&self, ch: usize, start: i64, dst: &mut [f32]) {
        let t = self.m.lock().unwrap().tl.get(ch).cloned();
        match t {
            Some(t) => t.read_at(start, dst),
            None => dst.iter_mut().for_each(|v| *v = 0.0),
        }
    }

    pub fn pad_to(&self, idx: i64) {
        for t in self.timelines() {
            t.pad_to(idx);
        }
    }

    pub fn set_delay(&self, d: i64) {
        let (old, tl) = {
            let mut m = self.m.lock().unwrap();
            let old = m.delay;
            m.delay = d;
            (old, m.tl.clone())
        };
        for t in tl {
            t.shift(d - old);
        }
    }

    pub fn cur_delay(&self) -> i64 {
        self.m.lock().unwrap().delay
    }

    pub fn resync_count(&self) -> usize {
        self.m.lock().unwrap().resyncs
    }

    fn ensure(&self, nch: usize) -> Vec<Arc<Timeline>> {
        let mut m = self.m.lock().unwrap();
        while m.tl.len() < nch {
            m.tl.push(Arc::new(Timeline::new(self.size)));
        }
        m.tl.clone()
    }

    // отставание шкалы от меток времени в точке idx, отсчётов
    pub fn lag_at(&self, idx: i64) -> i64 {
        let m = self.m.lock().unwrap();
        let v = if idx < m.jump_at { m.old_lag } else { m.lag };
        v.round() as i64
    }

    fn track(&self, diff: i64, end: i64) {
        let mut m = self.m.lock().unwrap();
        let d = diff as f64;
        if (d - m.lag).abs() > (RATE / 200) as f64 {
            m.old_lag = m.lag;
            m.lag = d;
            m.jump_at = end;
        } else {
            m.lag += 0.02 * (d - m.lag);
        }
    }

    fn rebase(&self, at: i64) {
        let mut m = self.m.lock().unwrap();
        m.resyncs += 1;
        m.old_lag = m.lag;
        m.lag = 0.0;
        m.jump_at = at;
    }

    // pkt[c] — отсчёты канала c, все одной длины; qpc — метка начала пакета (0 — неизвестна)
    pub fn push(&self, pkt: &[Vec<f32>], qpc: i64) {
        if pkt.is_empty() || pkt[0].is_empty() {
            return;
        }
        let tl = self.ensure(pkt.len());
        let delay = self.cur_delay();
        let end = tl[0].end();
        let mut skip = 0usize;
        if qpc != 0 {
            let pi = idx_from_qpc(qpc, self.t0) + delay;
            match end {
                None => tl.iter().for_each(|t| t.start(pi)),
                Some(end) => {
                    let diff = pi - end;
                    if diff > self.tol {
                        tl.iter().for_each(|t| t.pad_to(pi));
                        self.rebase(pi);
                    } else if diff < -self.tol {
                        let drop = (-diff) as usize;
                        if drop >= pkt[0].len() {
                            return;
                        }
                        skip = drop;
                        self.rebase(end);
                    } else {
                        self.track(diff, end);
                    }
                }
            }
        } else if end.is_none() {
            tl.iter().for_each(|t| t.start(0));
        }
        for (c, p) in pkt.iter().enumerate() {
            tl[c].append(&p[skip..]);
        }
    }
}
