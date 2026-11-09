use super::{StreamIn, RATE};

fn rms_db(x: &[f32]) -> f64 {
    let s: f64 = x.iter().map(|&v| v as f64 * v as f64).sum();
    10.0 * (s / x.len() as f64 + 1e-12).log10()
}

// общий отрезок двух потоков: [0] — микрофон, дальше каналы наушников
pub fn extract(mic: &StreamIn, reff: &StreamIn, now: i64) -> Result<(Vec<Vec<f32>>, Vec<String>), String> {
    let m_first = mic.first().ok_or("no packets came from the microphone")?;
    let r_first = reff.first().ok_or("no sound came from the headphones: play music or a video and record again")?;
    reff.pad_to(now);
    mic.pad_to(now);
    let a = m_first.max(r_first) + RATE as i64;
    let b = mic.end().unwrap_or(0).min(reff.end().unwrap_or(0)) - RATE as i64 / 5;
    let n = b - a;
    if n < RATE as i64 * 3 {
        return Err(format!("too little data ({n} samples)"));
    }
    let n = n as usize;
    let mut m = vec![0.0; n];
    mic.read_at(0, a, &mut m);
    let mut notes = vec![format!("channel 0 = microphone ({:.1} dB)", rms_db(&m))];
    let mut chans = vec![m];
    for c in 0..reff.channels() {
        let mut x = vec![0.0; n];
        reff.read_at(c, a, &mut x);
        let mx = x.iter().fold(0f32, |m, v| m.max(v.abs()));
        if mx < 1e-6 {
            notes.push(format!("headphones, source channel {c}: silence (not recorded)"));
            continue;
        }
        let db = rms_db(&x);
        chans.push(x);
        notes.push(format!("channel {} = headphones, source channel {c} ({db:.1} dB)", chans.len() - 1));
    }
    notes.push(format!(
        "length {:.1} s, resyncs: mic {}, headphones {}",
        n as f64 / RATE as f64,
        mic.resync_count(),
        reff.resync_count()
    ));
    Ok((chans, notes))
}
