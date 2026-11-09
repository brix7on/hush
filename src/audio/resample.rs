// потоковый ресемплер на оконном sinc (Blackman)

const ZERO_CROSSINGS: f64 = 12.0;
const TABLE_RES: f64 = 512.0;

pub struct Resampler {
    half: f64,
    table: Vec<f64>,
    buf: Vec<f32>,
    base: i64,
    pos: f64,
    step: f64,
    scale: f64,
    pub identity: bool,
}

impl Resampler {
    pub fn new(in_rate: usize, out_rate: usize, force_active: bool) -> Self {
        let (ir, or) = (in_rate as f64, out_rate as f64);
        let fc = (or / ir).min(1.0);
        let half = ZERO_CROSSINGS / fc;
        let n = (half * TABLE_RES) as usize + 2;
        let kernel = |x: f64| -> f64 {
            if x >= half {
                return 0.0;
            }
            let a = fc * x;
            let s = if a != 0.0 { (std::f64::consts::PI * a).sin() / (std::f64::consts::PI * a) } else { 1.0 };
            let t = x / half;
            let w = 0.42 + 0.5 * (std::f64::consts::PI * t).cos() + 0.08 * (2.0 * std::f64::consts::PI * t).cos();
            fc * s * w
        };
        let table = (0..n).map(|j| kernel(j as f64 / TABLE_RES)).collect();
        Resampler {
            half,
            table,
            buf: Vec::new(),
            base: 0,
            pos: 0.0,
            step: ir / or,
            scale: 1.0,
            identity: in_rate == out_rate && !force_active,
        }
    }

    #[inline]
    fn h(&self, x: f64) -> f64 {
        let u = x.abs() * TABLE_RES;
        let j = u as usize;
        if j + 1 >= self.table.len() {
            return 0.0;
        }
        let fr = u - j as f64;
        self.table[j] * (1.0 - fr) + self.table[j + 1] * fr
    }

    // scale>1 — выход короче, <1 — длиннее
    pub fn set_step_scale(&mut self, s: f64) {
        self.scale = s;
    }

    pub fn process(&mut self, inp: &[f32], out: &mut Vec<f32>) {
        if self.identity {
            out.extend_from_slice(inp);
            return;
        }
        self.buf.extend_from_slice(inp);
        loop {
            let hi = (self.pos + self.half).floor() as i64;
            if hi >= self.base + self.buf.len() as i64 {
                break;
            }
            let lo = (self.pos - self.half).ceil() as i64;
            let (mut acc, mut wsum) = (0.0, 0.0);
            for i in lo..=hi {
                let w = self.h(self.pos - i as f64);
                wsum += w;
                if i >= self.base {
                    acc += w * self.buf[(i - self.base) as usize] as f64;
                }
            }
            if wsum != 0.0 {
                acc /= wsum;
            }
            out.push(acc as f32);
            self.pos += self.step * self.scale;
        }
        let keep = (self.pos - self.half).ceil() as i64 - 1;
        let drop = keep - self.base;
        if drop > 0 {
            let drop = (drop as usize).min(self.buf.len());
            self.buf.drain(..drop);
            self.base += drop as i64;
        }
    }
}
