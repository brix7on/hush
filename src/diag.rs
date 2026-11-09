// проверка «звук из вывода возвращается во вход»

pub const FRAME_MS: usize = 20;
pub const PERIOD_MS: usize = 800;
pub const ON_MS: usize = 400;
pub const PERIODS: usize = 6;
pub const MAX_LAG_MS: usize = 780;

// чередование шума и тишины
pub fn tone(rate: usize, amp: f64) -> Vec<f32> {
    let n = rate * PERIOD_MS / 1000 * PERIODS;
    let mut out = vec![0f32; n];
    let mut s: u32 = 12345;
    let mut lp = 0f64;
    for (i, o) in out.iter_mut().enumerate() {
        let t = i * 1000 / rate;
        if t % PERIOD_MS < ON_MS {
            s = s.wrapping_mul(1664525).wrapping_add(1013904223);
            let w = s as i32 as f64 / 2147483648.0;
            lp += 0.5 * (w - lp);
            *o = (amp * lp * 1.7) as f32;
        }
    }
    out
}

#[derive(Clone, Copy, Debug, Default)]
pub struct DetectResult {
    pub hears: bool,
    pub r: f64,
    pub diff_db: f64,
    pub lag_ms: usize,
}

fn mean(x: &[f64]) -> f64 {
    if x.is_empty() {
        0.0
    } else {
        x.iter().sum::<f64>() / x.len() as f64
    }
}

fn corr(x: &[f64], y: &[f64]) -> f64 {
    let (mx, my) = (mean(x), mean(y));
    let (mut sxy, mut sxx, mut syy) = (0.0, 0.0, 0.0);
    for i in 0..x.len() {
        let (a, b) = (x[i] - mx, y[i] - my);
        sxy += a * b;
        sxx += a * a;
        syy += b * b;
    }
    if sxx == 0.0 || syy == 0.0 {
        0.0
    } else {
        sxy / (sxx * syy).sqrt()
    }
}

// rec — запись входа; tone_at_ms — когда после начала записи начал играть сигнал
pub fn detect(rec: &[f32], rate: usize, tone_at_ms: usize) -> DetectResult {
    let fr = rate * FRAME_MS / 1000;
    let nf = rec.len() / fr;
    if nf == 0 {
        return DetectResult::default();
    }
    let le: Vec<f64> = (0..nf)
        .map(|k| {
            let e: f64 = rec[k * fr..(k + 1) * fr].iter().map(|&v| v as f64 * v as f64).sum();
            10.0 * (e / fr as f64 + 1e-14).log10()
        })
        .collect();
    let mut best = DetectResult { r: -2.0, ..Default::default() };
    let tone_frames = PERIOD_MS * PERIODS / FRAME_MS;
    for lag in (0..=MAX_LAG_MS).step_by(FRAME_MS) {
        let start = (tone_at_ms + lag) / FRAME_MS;
        if start + tone_frames > nf {
            continue;
        }
        let (mut on, mut off, mut xs, mut ps) = (vec![], vec![], vec![], vec![]);
        for k in 0..tone_frames {
            let ph = (k * FRAME_MS) % PERIOD_MS;
            if ph < FRAME_MS || (ph >= ON_MS && ph < ON_MS + FRAME_MS) {
                continue;
            }
            let v = le[start + k];
            let p = if ph < ON_MS {
                on.push(v);
                1.0
            } else {
                off.push(v);
                0.0
            };
            xs.push(v);
            ps.push(p);
        }
        let r = corr(&xs, &ps);
        if r > best.r {
            best = DetectResult { r, diff_db: mean(&on) - mean(&off), lag_ms: lag, hears: false };
        }
    }
    best.hears = best.r > 0.6 && best.diff_db > 3.0;
    best
}
