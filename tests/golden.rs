// сверка с Go-версией: HUSH_GOLDEN=папка с выходами tools/golden
use hush::audio::Resampler;
use hush::dsp::{agc::Agc, amp::Amp, denoise::Denoiser, eq::Eq, suppress::Suppressor};

fn rd(dir: &str, name: &str) -> Vec<f32> {
    let b = std::fs::read(format!("{dir}/{name}")).unwrap();
    b.chunks_exact(4).map(|c| f32::from_le_bytes([c[0], c[1], c[2], c[3]])).collect()
}

fn cmp(name: &str, a: &[f32], b: &[f32]) {
    assert_eq!(a.len(), b.len(), "{name}: length");
    let (mut e, mut p, mut mx) = (0f64, 0f64, 0f64);
    for i in 0..a.len() {
        let d = (a[i] - b[i]) as f64;
        e += d * d;
        p += (b[i] as f64).powi(2);
        mx = mx.max(d.abs());
    }
    let snr = 10.0 * (p / (e + 1e-30)).log10();
    eprintln!("{name}: SNR {snr:.1} dB, max diff {mx:.2e}");
    assert!(snr > 100.0, "{name}: Rust and Go differ");
}

#[test]
fn golden() {
    let Ok(dir) = std::env::var("HUSH_GOLDEN") else { return };
    let (rf, mc) = (rd(&dir, "in_ref.f32"), rd(&dir, "in_mic.f32"));
    let mut sup = Suppressor::new(48000);
    let sh = sup.shared();
    sh.set_auto_learn(false);
    let mut dn = Denoiser::new();
    dn.ctl().set_strength(3);
    let mut eq = Eq::new();
    eq.ctl().set_preset(1);
    let mut agc = Agc::new();
    agc.ctl().set_enabled(true);
    let mut amp = Amp::new(6.0);
    sh.start_calib(hush::diag::PERIOD_MS * hush::diag::PERIODS * 48000 / 1000 / 256);
    let mut out = Vec::new();
    let mut ob = vec![0f32; 256];
    let mut i = 0;
    while i + 256 <= rf.len() {
        sup.process(&rf[i..i + 256], &mc[i..i + 256], &mut ob);
        dn.ctl().voice_hint(sh.voice_level());
        dn.process(&mut ob);
        eq.process(&mut ob);
        agc.process(&mut ob);
        amp.process(&mut ob);
        out.extend_from_slice(&ob);
        i += 256;
    }
    let c = sh.calib();
    eprintln!("calib: d={} corr={:.4} leak={:.2} found={}", c.d, c.corr, c.leak_db, c.found);
    let gc: serde_json::Value = serde_json::from_str(&std::fs::read_to_string(format!("{dir}/calib.txt")).unwrap()).unwrap();
    assert_eq!(gc["delay_frames"].as_u64().unwrap() as usize, c.d);
    assert!((gc["corr"].as_f64().unwrap() - c.corr).abs() < 1e-9);
    cmp("chain", &out, &rd(&dir, "out_chain.f32"));

    let mut rs = Resampler::new(44100, 48000, true);
    let inp: Vec<f32> = (0..44100).map(|i| ((i as f64 * 0.07).sin() * 0.5) as f32).collect();
    let mut ro = Vec::new();
    for ch in inp.chunks(441) {
        rs.process(ch, &mut ro);
    }
    cmp("resample", &ro, &rd(&dir, "out_rs.f32"));
    cmp("tone", &hush::diag::tone(48000, 0.3), &rd(&dir, "out_tone.f32"));
}
