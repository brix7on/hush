use hush::dsp::suppress::Suppressor;
use hush::pipe::{Chain, Engine, Sink, StreamIn, RATE};
use std::sync::{Arc, Mutex};

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

fn pw(x: &[f64], a: usize, b: usize) -> f64 {
    x[a..b].iter().map(|v| v * v).sum::<f64>() / (b - a) as f64
}

fn music_like(n: usize, r: &mut Rng) -> Vec<f64> {
    let (mut env, mut target) = (0.1, 0.1);
    (0..n)
        .map(|i| {
            if i % (RATE / 8) == 0 {
                target = 0.02 + 0.3 * r.u();
            }
            env += 0.0005 * (target - env);
            env * r.norm()
        })
        .collect()
}

fn env_leak(rf: &[f64], delay: usize, gain: f64, r: &mut Rng) -> Vec<f64> {
    let mut e = 0.0;
    let env: Vec<f64> = rf.iter().map(|v| { e += 0.004 * (v.abs() - e); e }).collect();
    let (mut lp1, mut lp2) = (0.0, 0.0);
    (0..rf.len())
        .map(|i| {
            let w = r.norm();
            lp1 += 0.3 * (w - lp1);
            lp2 += 0.05 * (lp1 - lp2);
            if i >= delay { gain * env[i - delay] * (lp1 - lp2) * 6.0 } else { 0.0 }
        })
        .collect()
}

fn run_sup(s: &mut Suppressor, rf: &[f64], mic: &[f64]) -> Vec<f64> {
    let h = s.h;
    let lat = s.n - s.h;
    let mut out = vec![0.0; mic.len()];
    let (mut rb, mut mb, mut ob) = (vec![0f32; h], vec![0f32; h], vec![0f32; h]);
    let mut st = 0;
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
        // фоновый пересчёт успевает, как и в реальном времени
        while s.auto_busy() {
            std::thread::sleep(std::time::Duration::from_millis(1));
        }
        st += h;
    }
    out
}

#[test]
fn autolearn_without_calibration() {
    let mut r = Rng(3);
    let n = 20 * RATE;
    let rf = music_like(n, &mut r);
    let leak = env_leak(&rf, RATE * 50 / 1000, 0.05, &mut r);
    let mic: Vec<f64> = leak.iter().map(|l| l + 2e-4 * r.norm()).collect();
    let mut s = Suppressor::new(RATE);
    let out = run_sup(&mut s, &rf, &mic);
    let c = s.shared().calib();
    assert!(c.found, "model not learned");
    let d = (c.d * s.h) as f64 * 1000.0 / RATE as f64;
    assert!((d - 50.0).abs() <= 12.0, "delay {d} ms");
    let red = 10.0 * (pw(&mic, 15 * RATE, 20 * RATE) / pw(&out, 15 * RATE, 20 * RATE)).log10();
    eprintln!("autolearn: delay {d:.0} ms, reduction {red:.1} dB");
    assert!(red >= 8.0);
}

#[test]
fn autolearn_keeps_voice() {
    let mut r = Rng(4);
    let n = 20 * RATE;
    let rf = music_like(n, &mut r);
    let leak = env_leak(&rf, RATE * 50 / 1000, 0.05, &mut r);
    let mic: Vec<f64> = (0..n)
        .map(|i| {
            let mut v = leak[i] + 2e-4 * r.norm();
            if i > 16 * RATE && i < 18 * RATE {
                let t = i as f64 / RATE as f64;
                v += 0.1 * (2.0 * std::f64::consts::PI * 220.0 * t).sin() * (1.0 + 0.5 * (2.0 * std::f64::consts::PI * 3.0 * t).sin());
            }
            v
        })
        .collect();
    let mut s = Suppressor::new(RATE);
    let out = run_sup(&mut s, &rf, &mic);
    let (a, b) = (16 * RATE + RATE / 4, 18 * RATE - RATE / 4);
    let loss = 10.0 * (pw(&mic, a, b) / pw(&out, a, b)).log10();
    eprintln!("voice loss {loss:.1} dB");
    assert!(loss <= 3.0);
}

fn chain(sup: Suppressor) -> Arc<Mutex<Chain>> {
    Arc::new(Mutex::new(Chain {
        sup,
        dn: hush::dsp::denoise::Denoiser::new(),
        eq: hush::dsp::eq::Eq::new(),
        agc: hush::dsp::agc::Agc::new(),
        amp: hush::dsp::amp::Amp::new(0.0),
    }))
}

#[test]
fn engine_with_calibration() {
    let mut r = Rng(17);
    let (cal_n, run_n) = (8 * RATE, 16 * RATE);
    let n = cal_n + run_n;
    let mut rf = vec![0.0; n];
    for (i, v) in rf.iter_mut().enumerate().take(cal_n - RATE).skip(RATE) {
        if (i * 1000 / RATE) % 800 < 400 {
            *v = 0.05 * r.norm();
        }
    }
    let fr = music_like(run_n, &mut r);
    rf[cal_n..].copy_from_slice(&fr);
    let leak = env_leak(&rf, 55 * RATE / 1000, 0.02, &mut r);
    let mic: Vec<f64> = leak.iter().map(|l| l + 0.0003 * r.norm()).collect();

    let mic_s = Arc::new(StreamIn::new(RATE * 2, 0, 0));
    let ref_s = Arc::new(StreamIn::new(RATE * 2, 0, 960));
    let sup = Suppressor::new(RATE);
    let sh = sup.shared();
    sh.set_auto_learn(false);
    let (sn, shp) = (sup.n, sup.h);
    let ch = chain(sup);
    ch.lock().unwrap().dn.ctl().set_strength(0);
    let mut eng = Engine::new(mic_s.clone(), ref_s.clone(), ch, 2);
    let stamp = |a: usize| a as i64 * 10_000_000 / RATE as i64 + 1;
    let mut out = Vec::new();
    let mut cal_started = false;
    let mut a = 0;
    while a + 480 <= n {
        let m: Vec<f32> = mic[a..a + 480].iter().map(|&v| v as f32).collect();
        let l: Vec<f32> = rf[a..a + 480].iter().map(|&v| v as f32).collect();
        mic_s.push(&[m], stamp(a));
        ref_s.push(&[l.clone(), l], stamp(a));
        if !cal_started && a >= RATE / 2 {
            sh.start_calib((cal_n - RATE) / shp);
            cal_started = true;
        }
        eng.step((a + 480) as i64, &mut out);
        a += 480;
    }
    let c = sh.calib();
    let mut o = vec![0.0; n];
    for (i, &v) in out.iter().enumerate() {
        let idx = eng.first as i64 + i as i64 - (sn - shp) as i64;
        if idx >= 0 && (idx as usize) < n {
            o[idx as usize] = v as f64;
        }
    }
    let red = 10.0 * (pw(&mic, cal_n + 2 * RATE, n) / pw(&o, cal_n + 2 * RATE, n)).log10();
    eprintln!("engine: found={} delay={:.0} ms reduction {red:.1} dB", c.found, (c.d * shp) as f64 * 1000.0 / RATE as f64);
    assert!(c.found && red >= 8.0);
}

#[test]
fn sink_low_latency() {
    for tgt in [10usize, 12, 15] {
        for drift in [300e-6, -300e-6] {
            let mut s = Sink::new(48000, tgt);
            let mut padding = 0.0f64;
            let mut under = 0;
            let burst = vec![0f32; 480];
            for ms in 0..60000 {
                if ms % 10 == 0 {
                    s.feed(&burst);
                }
                let p = padding as usize;
                let got = s.take(p, 4800 - p).len();
                padding += got as f64;
                padding -= 48.0 * (1.0 + drift);
                if padding < 0.0 {
                    if ms > 1000 {
                        under += 1;
                    }
                    padding = 0.0;
                }
            }
            assert_eq!(under, 0, "target {tgt} drift {drift}");
        }
    }
}

#[test]
fn engine_with_clock_drift() {
    for ppm in [1000.0, -1000.0] {
        clock_drift(ppm);
    }
}

fn clock_drift(ppm: f64) {
    let mut r = Rng(23);
    let (cal_n, run_n) = (8 * RATE, 72 * RATE);
    let n = cal_n + run_n;
    let mut rf = vec![0.0; n];
    for (i, v) in rf.iter_mut().enumerate().take(cal_n - RATE).skip(RATE) {
        if (i * 1000 / RATE) % 800 < 400 {
            *v = 0.05 * r.norm();
        }
    }
    // резкие короткие звуки: на них сдвиг шкал заметнее всего
    let mut amp = 0.0;
    for (i, v) in rf.iter_mut().enumerate().skip(cal_n) {
        if i % (RATE / 5) == 0 {
            amp = 0.05 + 0.25 * r.u();
        }
        if i % (RATE / 5) < RATE * 40 / 1000 {
            *v = amp * r.norm();
        }
    }
    let leak = env_leak(&rf, 30 * RATE / 1000, 0.02, &mut r);
    let mic: Vec<f64> = leak.iter().map(|l| l + 0.0003 * r.norm()).collect();

    let mic_s = Arc::new(StreamIn::new(RATE * 2, 0, 0));
    let ref_s = Arc::new(StreamIn::new(RATE * 2, 0, 0));
    let sup = Suppressor::new(RATE);
    let sh = sup.shared();
    sh.set_auto_learn(false);
    let shp = sup.h;
    let ch = chain(sup);
    ch.lock().unwrap().dn.ctl().set_strength(0);
    let mut eng = Engine::new(mic_s.clone(), ref_s.clone(), ch, 1);
    let stamp = |a: usize| a as i64 * 10_000_000 / RATE as i64 + 1;
    let mut out = Vec::new();
    let mut cal_started = false;
    let step = 480.0 * (1.0 + ppm * 1e-6);
    let mut mt = 0.0f64;
    let from = cal_n + 4 * RATE;
    let (mut e_in, mut out_from) = (0.0, 0usize);
    let mut a = 0;
    while a + 480 <= n {
        let l: Vec<f32> = rf[a..a + 480].iter().map(|&v| v as f32).collect();
        ref_s.push(&[l], stamp(a));
        while mt + step <= (a + 480) as f64 {
            let p = mt as usize;
            let m: Vec<f32> = mic[p..p + 480].iter().map(|&v| v as f32).collect();
            if p >= from {
                e_in += m.iter().map(|&v| v as f64 * v as f64).sum::<f64>();
            } else {
                out_from = out.len();
            }
            mic_s.push(&[m], stamp(p));
            mt += step;
        }
        if !cal_started && a >= RATE / 2 {
            sh.start_calib((cal_n - RATE) / shp);
            cal_started = true;
        }
        eng.step((a + 480) as i64, &mut out);
        a += 480;
    }
    let e_out: f64 = out[out_from..].iter().map(|&v| v as f64 * v as f64).sum();
    let red = 10.0 * (e_in / e_out).log10();
    eprintln!("drift {ppm:+.0} ppm: reduction {red:.1} dB");
    assert!(red >= 9.0);
}
