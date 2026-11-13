use hush::dsp::suppress::{Calib, Suppressor};
use hush::pipe::RATE;

struct Rng(u64);
impl Rng {
    fn u(&mut self) -> f64 {
        self.0 ^= self.0 << 13;
        self.0 ^= self.0 >> 7;
        self.0 ^= self.0 << 17;
        (self.0 >> 11) as f64 / (1u64 << 53) as f64
    }
    fn norm(&mut self) -> f64 {
        let (a, b) = (self.u().max(1e-300), self.u());
        (-2.0 * a.ln()).sqrt() * (2.0 * std::f64::consts::PI * b).cos()
    }
}

const PI2: f64 = 2.0 * std::f64::consts::PI;

// звук игры: музыка, резкие звуки, паузы
fn game_ref(n: usize, r: &mut Rng) -> Vec<f64> {
    let mut out = vec![0.0; n];
    let (mut env, mut target) = (0.05, 0.05);
    let (mut lp, mut burst, mut bamp) = (0.0, 0usize, 0.0);
    let mut quiet_until = 0usize;
    for i in 0..n {
        if i % (RATE / 8) == 0 {
            target = 0.01 + 0.15 * r.u();
            if r.u() < 0.02 {
                quiet_until = i + RATE * (1 + (r.u() * 2.0) as usize);
            }
            if r.u() < 0.15 {
                burst = RATE * (20 + (r.u() * 60.0) as usize) / 1000;
                bamp = 0.1 + 0.4 * r.u();
            }
        }
        env += 0.0005 * (target - env);
        let w = r.norm();
        lp += 0.15 * (w - lp);
        let mut v = env * (0.6 * lp * 2.5 + 0.4 * w);
        if burst > 0 {
            burst -= 1;
            v += bamp * w;
        }
        if i < quiet_until {
            v = 0.0;
        }
        out[i] = v.clamp(-1.0, 1.0);
    }
    out
}

// путь наушники → микрофон: задержка, короткий отклик с окраской
fn echo_path(delay_ms: f64, gain_db: f64, r: &mut Rng) -> (usize, Vec<f64>) {
    let d = (delay_ms * RATE as f64 / 1000.0) as usize;
    let len = 160;
    let mut h: Vec<f64> = (0..len).map(|i| r.norm() * (-(i as f64) / 25.0).exp()).collect();
    // окраска: подъём 1.5–4 кГц
    let mut y = vec![0.0; len];
    let (mut a, mut b) = (0.0, 0.0);
    for i in 0..len {
        a += 0.45 * (h[i] - a);
        b += 0.12 * (a - b);
        y[i] = a - b + 0.15 * h[i];
    }
    let e: f64 = y.iter().map(|v| v * v).sum::<f64>().sqrt();
    let g = 10f64.powf(gain_db / 20.0) / e;
    h = y.iter().map(|v| v * g).collect();
    (d, h)
}

fn conv(x: &[f64], d: usize, h: &[f64], gain_at: impl Fn(usize) -> f64) -> Vec<f64> {
    let mut y = vec![0.0; x.len()];
    for i in d..x.len() {
        let mut s = 0.0;
        let base = i - d;
        for (k, hk) in h.iter().enumerate() {
            if k > base {
                break;
            }
            s += hk * x[base - k];
        }
        y[i] = s * gain_at(i);
    }
    y
}

// речь: гармоники с формантами, слоги, фразы
fn voice(n: usize, r: &mut Rng, level: f64, talk: f64) -> (Vec<f64>, Vec<bool>) {
    let mut v = vec![0.0; n];
    let mut on = vec![false; n];
    let mut i = RATE * 3;
    while i < n {
        let len = RATE / 2 + (r.u() * 2.0 * RATE as f64) as usize;
        let f0 = 100.0 + 80.0 * r.u();
        let (f1, f2) = (400.0 + 500.0 * r.u(), 1200.0 + 1200.0 * r.u());
        let mut ph = 0.0f64;
        for j in 0..len.min(n - i) {
            let t = j as f64 / RATE as f64;
            let syl = (0.5 - 0.5 * (PI2 * 4.5 * t).cos()).powf(0.7);
            let edge = ((j.min(len - j)) as f64 / (RATE as f64 * 0.03)).min(1.0);
            let f = f0 * (1.0 + 0.08 * (PI2 * 0.7 * t).sin());
            ph += PI2 * f / RATE as f64;
            let mut s = 0.0;
            for k in 1..40 {
                let fk = f * k as f64;
                if fk > 7000.0 {
                    break;
                }
                let a = 1.0 / (1.0 + ((fk - f1) / 150.0).powi(2)) + 0.6 / (1.0 + ((fk - f2) / 250.0).powi(2)) + 0.03;
                s += a * (ph * k as f64).sin();
            }
            v[i + j] = level * 0.5 * syl * edge * s;
            on[i + j] = true;
        }
        i += len + ((0.5 + r.u() * 3.0) * talk * RATE as f64) as usize;
    }
    (v, on)
}

fn run(s: &mut Suppressor, rf: &[f64], mic: &[f64]) -> (Vec<f64>, f64) {
    let h = s.h;
    let lat = s.n - s.h;
    let mut out = vec![0.0; mic.len()];
    let (mut rb, mut mb, mut ob) = (vec![0f32; h], vec![0f32; h], vec![0f32; h]);
    let mut st = 0;
    let mut found_at = -1.0;
    while st + h <= mic.len() {
        for i in 0..h {
            rb[i] = rf[st + i] as f32;
            mb[i] = mic[st + i] as f32;
        }
        s.process(&rb, &mb, &mut ob);
        for i in 0..h {
            if st + i >= lat {
                out[st + i - lat] = ob[i] as f64;
            }
        }
        while s.auto_busy() {
            std::thread::sleep(std::time::Duration::from_millis(1));
        }
        if found_at < 0.0 && s.shared().calib().found {
            found_at = st as f64 / RATE as f64;
        }
        st += h;
    }
    (out, found_at)
}

struct Scene {
    rf: Vec<f64>,
    echo: Vec<f64>,
    noise: Vec<f64>,
    voice: Vec<f64>,
    von: Vec<bool>,
}

fn scene(seed: u64, secs: usize, delay_ms: f64, gain_db: f64, voice_db: f64, change: Option<(usize, f64)>, talk: f64) -> Scene {
    let mut r = Rng(seed);
    let n = secs * RATE;
    let rf = game_ref(n, &mut r);
    let (d, h) = echo_path(delay_ms, gain_db, &mut r);
    let echo = conv(&rf, d, &h, |i| match change {
        Some((at, db)) if i >= at * RATE => 10f64.powf(db / 20.0),
        _ => 1.0,
    });
    let noise: Vec<f64> = (0..n).map(|_| 1.5e-4 * r.norm()).collect();
    let (voice, von) = voice(n, &mut r, 10f64.powf(voice_db / 20.0), talk);
    Scene { rf, echo, noise, voice, von }
}

fn db(x: f64) -> f64 {
    10.0 * (x + 1e-20).log10()
}

// подавление наводки там, где голоса нет, и потеря голоса, дБ
fn score(sc: &Scene, out: &[f64], from: usize, to: usize) -> (f64, f64) {
    let (mut ei, mut eo, mut vi, mut vo) = (0.0, 0.0, 0.0, 0.0);
    let guard = RATE / 10;
    for i in from * RATE..to * RATE {
        let near = sc.von[i.saturating_sub(guard)..(i + guard).min(sc.von.len())].iter().any(|&v| v);
        if !near {
            if sc.rf[i.saturating_sub(RATE / 20)..i + 1].iter().any(|v| v.abs() > 1e-4) {
                ei += (sc.echo[i] + sc.noise[i]).powi(2);
                eo += out[i] * out[i];
            }
        } else if sc.von[i] {
            vi += sc.voice[i] * sc.voice[i];
            vo += out[i] * out[i];
        }
    }
    (db(ei) - db(eo), db(vi) - db(vo))
}

// эталон: калибровка на шуме без голоса тем же путём
fn oracle(seed: u64, secs: usize, delay_ms: f64, gain_db: f64) -> Calib {
    let mut r = Rng(seed);
    let n = secs * RATE;
    let _ = game_ref(n, &mut r);
    let (d, h) = echo_path(delay_ms, gain_db, &mut r);
    let m = 10 * RATE;
    let mut r2 = Rng(99);
    let rf: Vec<f64> = (0..m).map(|i| if i > RATE && i < 9 * RATE && (i * 1000 / RATE) % 800 < 400 { 0.1 * r2.norm() } else { 0.0 }).collect();
    let echo = conv(&rf, d, &h, |_| 1.0);
    let mic: Vec<f64> = echo.iter().map(|e| e + 1.5e-4 * r2.norm()).collect();
    let mut s = Suppressor::new(RATE);
    s.shared().set_auto_learn(false);
    let hp = s.h;
    let (mut rb, mut mb, mut ob) = (vec![0f32; hp], vec![0f32; hp], vec![0f32; hp]);
    let mut st = 0;
    let mut started = false;
    while st + hp <= m {
        for i in 0..hp {
            rb[i] = rf[st + i] as f32;
            mb[i] = mic[st + i] as f32;
        }
        if !started && st > RATE / 2 {
            s.shared().start_calib(9 * RATE / hp);
            started = true;
        }
        s.process(&rb, &mb, &mut ob);
        st += hp;
    }
    s.shared().calib()
}

fn learned(name: &str, sc: &Scene, pre: Option<Calib>, auto: bool, from: usize, to: usize) -> (f64, f64) {
    let mic: Vec<f64> = (0..sc.rf.len()).map(|i| sc.echo[i] + sc.noise[i] + sc.voice[i]).collect();
    let mut s = Suppressor::new(RATE);
    s.shared().set_auto_learn(auto);
    if let Some(c) = pre {
        assert!(s.shared().set_calib(c));
    }
    let (out, found_at) = run(&mut s, &sc.rf, &mic);
    let (e, v) = score(sc, &out, from, to);
    eprintln!("{name}: model at {found_at:.1} s, bleed -{e:.1} dB, voice -{v:.1} dB");
    (e, v)
}

// обучение на обычном звуке с разговором: не портит верную модель и исправляет неверную
#[test]
fn learning_on_game_sound() {
    let (seed, delay, gain, vdb) = (11, 30.0, -38.0, -28.0);
    let good = oracle(seed, 45, delay, gain);
    let sc = scene(seed, 45, delay, gain, vdb, None, 1.0);
    let (base, _) = learned("fixed model", &sc, Some(good.clone()), false, 20, 44);
    let check = |name: &str, sc: &Scene, pre: Option<Calib>, floor: f64| {
        let (e, v) = learned(name, sc, pre, true, 20, 44);
        assert!(e >= floor, "{name}: bleed -{e:.1} dB");
        assert!(v <= 1.5, "{name}: voice -{v:.1} dB");
    };
    check("good model", &sc, Some(good.clone()), base - 2.0);
    check("no model", &sc, None, base - 2.0);
    let mut low = good.clone();
    low.w.iter_mut().for_each(|v| *v *= 0.1);
    low.w0 = low.w.clone();
    check("model 10 dB low", &sc, Some(low), base - 2.0);
    let mut late = good.clone();
    late.d += 5;
    check("wrong delay", &sc, Some(late), base - 2.0);
    let talk = scene(seed, 45, delay, gain, vdb, None, 0.08);
    check("talking most of the time", &talk, Some(good.clone()), 10.0);
    let moved = scene(seed, 45, delay, gain, vdb, Some((12, 8.0)), 1.0);
    check("headset moved", &moved, Some(good), base - 2.0);
}
