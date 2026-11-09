use std::sync::atomic::{AtomicBool, AtomicI64, Ordering::Relaxed};
use std::sync::mpsc::Sender;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use windows::Win32::Media::Audio::*;

use hush::audio::Format;
use hush::l;
use hush::pipe::{idx_from_qpc, mic_base_ms, Engine, Sink, StreamIn, RATE};

use super::capture::Ready;
use super::dev::*;
use super::state::{note_stall, Ev, G};
use super::sys::{now_qpc, Com, ProAudio, Tick};
use crate::logf;

pub struct RenderArgs {
    pub sub: String,
    pub force_shared: bool,
    pub eng: Arc<Mutex<Engine>>,
    pub t0: i64,
    pub stop: Arc<AtomicBool>,
    pub ready: Sender<Ready>,
    pub ev: Sender<Ev>,
    pub sess: u64,
    pub mic: Arc<StreamIn>,
    pub reff: Arc<StreamIn>,
    pub quiet: bool,
}

// шум около −90 дБ в тишине, чтобы Discord не считал микрофон неисправным
fn dither_quiet(buf: &mut [f32], state: &mut u32) {
    const LSB: f32 = 1.0 / 32767.0;
    if buf.iter().any(|&v| v > 4.0 * LSB || v < -4.0 * LSB) {
        return;
    }
    let mut x = *state;
    for v in buf.iter_mut() {
        x ^= x << 13;
        x ^= x >> 17;
        x ^= x << 5;
        let a = (x & 0xFFFF) as f32 / 65535.0;
        let b = (x >> 16) as f32 / 65535.0;
        *v += (a + b - 1.0) * LSB;
    }
    *state = x;
}

struct Prev {
    starve: usize,
    drops: usize,
    quiet: i64,
    late: usize,
    mic_rs: usize,
    ref_rs: usize,
    glitch: i64,
    st: [i64; 3],
    sup_t: Option<Instant>,
}

fn log_counters(sink: &Sink, eng: &mut Engine, a: &RenderArgs, p: &mut Prev, start: Instant) {
    let fill_ms = sink.target as f64 * 1000.0 / sink.rate() as f64;
    if sink.starves != p.starve {
        G.out_starves.store(sink.starves as i64, Relaxed);
        logf!("ПРОВАЛ звука в кабеле ×{} (данных не хватило) — запас теперь {:.0} мс", sink.starves - p.starve, fill_ms);
        p.starve = sink.starves;
    }
    if sink.drops != p.drops {
        logf!("выкинут кусок звука целиком ×{} (накопилось больше 80 мс задержки)", sink.drops - p.drops);
        p.drops = sink.drops;
    }
    if sink.quiet_drops != p.quiet {
        logf!("выкинуто {:.1} мс в паузах (лишняя задержка)", (sink.quiet_drops - p.quiet) as f64 * 1000.0 / sink.rate() as f64);
        p.quiet = sink.quiet_drops;
    }
    if eng.late_ref != p.late {
        logf!("ждали звук наушников ×{} (дольше всего {:.0} мс) — выход в это время стоял", eng.late_ref - p.late, eng.late_max_ms);
        p.late = eng.late_ref;
        eng.late_max_ms = 0.0;
    }
    let r = a.mic.resync_count();
    if r != p.mic_rs {
        logf!("микрофон: разрыв во времени ×{} (пакеты пришли не подряд)", r - p.mic_rs);
        p.mic_rs = r;
    }
    let r = a.reff.resync_count();
    if r != p.ref_rs {
        if r - p.ref_rs > 1 || start.elapsed() < Duration::from_secs(5) {
            logf!("наушники: разрыв во времени ×{} (обычно — звук в наушниках начался/кончился)", r - p.ref_rs);
        }
        p.ref_rs = r;
    }
    let stalls: [(&AtomicI64, &AtomicI64, &str); 3] = [
        (&G.mic_stalls, &G.mic_stall_max, "поток микрофона"),
        (&G.ref_stalls, &G.ref_stall_max, "поток наушников"),
        (&G.out_stalls, &G.out_stall_max, "поток вывода"),
    ];
    for (i, (n, mx, what)) in stalls.iter().enumerate() {
        let v = n.load(Relaxed);
        if v != p.st[i] {
            logf!(
                "{what} не получал процессор дольше 30 мс ×{} (самое долгое {} мс) — ПК был занят или Windows притормозила программу",
                v - p.st[i],
                mx.load(Relaxed)
            );
            p.st[i] = v;
            mx.store(0, Relaxed);
        }
    }
    let g = G.mic_glitches.load(Relaxed);
    if g != p.glitch {
        logf!("микрофон: драйвер сообщил о пропуске данных ×{}", g - p.glitch);
        p.glitch = g;
    }
    if p.sup_t.is_none_or(|t| t.elapsed() > Duration::from_secs(5)) {
        let ch = eng.chain.lock().unwrap();
        let sh = ch.sup.shared();
        let st = sh.stats();
        if st.voice && st.ref_db > -60.0 && st.reduction_db > 6.0 {
            p.sup_t = Some(Instant::now());
            logf!(
                "подавитель глушит микрофон на {:.0} дБ, пока ты говоришь и в наушниках звук (сила {:.1}) — если голос рвётся, это оно",
                st.reduction_db,
                sh.beta()
            );
        }
    }
}

pub fn run_render(a: RenderArgs) {
    let _com = Com::init();
    let _pa = ProAudio::enter();
    let fail = |e: String| {
        let _ = a.ready.send(Ready { label: "out".into(), name: String::new(), f: Format::default(), err: Some(format!("{}{e}", l("вывод: ", "output: "))) });
    };
    let (dev, mut name) = match pick_device(eRender, &a.sub, l("вывод", "output"), true) {
        Ok(v) => v,
        Err(e) => {
            let _ = a.ready.send(Ready { label: "out".into(), name: String::new(), f: Format::default(), err: Some(e) });
            return;
        }
    };
    let mut exclusive = false;
    let mut client: Option<(IAudioClient, Format)> = None;
    if !a.force_shared {
        match open_exclusive_render(&dev) {
            Ok(v) => {
                client = Some(v);
                exclusive = true;
            }
            Err(e) => println!("\n({}: {e})", l("монопольный режим кабеля не вышел — работаю обычным", "exclusive cable mode failed, using shared")),
        }
    }
    let (ac, f) = match client {
        Some(v) => v,
        None => {
            let (c, mf, ff) = match open_client(&dev) {
                Ok(v) => v,
                Err(e) => return fail(e),
            };
            if let Err(e) = unsafe { c.Initialize(AUDCLNT_SHAREMODE_SHARED, 0, 2_000_000, 0, mf.0, None) } {
                return fail(format!("Initialize: {e}"));
            }
            (c, ff)
        }
    };
    let buf_frames = match unsafe { ac.GetBufferSize() } {
        Ok(v) => v,
        Err(e) => return fail(e.to_string()),
    };
    let arc: IAudioRenderClient = match unsafe { ac.GetService() } {
        Ok(v) => v,
        Err(e) => return fail(format!("GetService: {e}")),
    };
    let mic_base = || {
        let us = G.mic_period_us.load(Relaxed);
        mic_base_ms(if us > 0 { us as f64 / 1000.0 } else { 10.0 })
    };
    let mut cur_base = mic_base();
    let sink_ms = cur_base.ceil() as usize;
    G.excl_active.store(exclusive, Relaxed);
    if exclusive {
        name += &format!(" [{}]", l("монопольно", "exclusive"));
    }
    let mut sink = Sink::new(f.rate, sink_ms);
    sink.set_base_ms(cur_base);
    sink.quiet_slack = (f.rate * 4 / 1000) as i64;
    let pre = sink.target as u32;
    unsafe {
        if let Err(e) = arc.GetBuffer(pre) {
            return fail(format!("GetBuffer: {e}"));
        }
        let _ = arc.ReleaseBuffer(pre, AUDCLNT_BUFFERFLAGS_SILENT.0 as u32);
        if let Err(e) = ac.Start() {
            return fail(format!("Start: {e}"));
        }
    }
    let _ = a.ready.send(Ready { label: l("вывод", "output").into(), name: name.clone(), f, err: None });

    let mut out: Vec<f32> = Vec::new();
    let mut last_stat = Instant::now();
    let start = Instant::now();
    G.out_buf_ms.store(sink_ms as i64, Relaxed);
    G.out_lat_ms.store(0, Relaxed);
    let (mut lat_sum, mut lat_n, mut lat_t) = (0.0f64, 0usize, Instant::now());
    G.out_starves.store(0, Relaxed);
    let mut ev_t = Instant::now();
    let mut prev = Prev { starve: 0, drops: 0, quiet: 0, late: 0, mic_rs: 0, ref_rs: 0, glitch: 0, st: [0; 3], sup_t: None };
    logf!("старт: вывод «{name}» [{f}], запас {cur_base:.0} мс, микрофон пачками по {:.1} мс", G.mic_period_us.load(Relaxed) as f64 / 1000.0);
    let tk = Tick::new();
    G.out_stalls.store(0, Relaxed);
    G.out_stall_max.store(0, Relaxed);
    let mut last_wake = Instant::now();
    let mut dither = 2_463_534_242u32;
    let mut enc: Vec<u8> = Vec::new();
    while !a.stop.load(Relaxed) {
        tk.wait();
        note_stall(&G.out_stalls, &G.out_stall_max, last_wake.elapsed());
        last_wake = Instant::now();
        let padding = match unsafe { ac.GetCurrentPadding() } {
            Ok(v) => v,
            Err(e) => {
                let _ = a.ev.send(Ev::Err { sess: a.sess, msg: format!("output: GetCurrentPadding: {e} ({})", l("устройство отключено?", "device unplugged?")), drops: false });
                break;
            }
        };
        let idx = idx_from_qpc(now_qpc(), a.t0);
        let mut eng = a.eng.lock().unwrap();
        eng.hold = sink.hold_for(padding as usize, RATE as i64 * 31 / 1000);
        out.clear();
        eng.step(idx, &mut out);
        sink.feed(&out);
        let b = mic_base();
        if b != cur_base {
            cur_base = b;
            sink.set_base_ms(b);
        }
        sink.adapt(padding as usize, start.elapsed());
        G.out_buf_ms.store(sink.target * 1000 / f.rate as i64, Relaxed);
        let chunk_len;
        {
            let chunk = sink.take(padding as usize, (buf_frames - padding) as usize);
            chunk_len = chunk.len();
            if chunk_len > 0 {
                let mut c = chunk.to_vec();
                dither_quiet(&mut c, &mut dither);
                enc.resize(chunk_len * f.block_align, 0);
                f.encode_mono(&c, &mut enc);
                match unsafe { arc.GetBuffer(chunk_len as u32) } {
                    Ok(dst) => unsafe {
                        std::ptr::copy_nonoverlapping(enc.as_ptr(), dst, enc.len());
                        let _ = arc.ReleaseBuffer(chunk_len as u32, 0);
                    },
                    Err(e) => {
                        let _ = a.ev.send(Ev::Err { sess: a.sess, msg: format!("output: GetBuffer: {e}"), drops: false });
                        break;
                    }
                }
            }
        }
        if let Some(nx) = eng.next() {
            let fill = (padding as usize + chunk_len + sink.queued()) as f64 * RATE as f64 / f.rate as f64;
            lat_sum += ((idx - (nx - 1) + eng.algo_delay()) as f64 + fill) * 1000.0 / RATE as f64;
            lat_n += 1;
        }
        if lat_t.elapsed() >= Duration::from_millis(500) {
            if lat_n > 0 {
                G.out_lat_ms.store((lat_sum / lat_n as f64 + 0.5) as i64, Relaxed);
            }
            lat_sum = 0.0;
            lat_n = 0;
            lat_t = Instant::now();
        }
        if ev_t.elapsed() >= Duration::from_secs(1) {
            ev_t = Instant::now();
            log_counters(&sink, &mut eng, &a, &mut prev, start);
        }
        if !a.quiet && super::sys::has_console() && last_stat.elapsed() >= Duration::from_secs(1) {
            last_stat = Instant::now();
            let ch = eng.chain.lock().unwrap();
            let st = ch.sup.shared().stats();
            let state = if st.calibrating {
                l("КАЛИБРОВКА — молчи", "CALIBRATING, stay quiet").to_string()
            } else if !st.found {
                l("наводка не найдена, микрофон как есть", "no bleed model yet, mic passes as is").to_string()
            } else if st.ref_db < -80.0 {
                l("в наушниках тишина", "headphones are silent").to_string()
            } else {
                format!("{} {:4.1} dB", l("наводка убрана на", "bleed reduced by"), st.reduction_db)
            };
            let amp = ch.amp.ctl();
            let mut g = format!(" | gain {:+.0} dB", amp.gain_db());
            if amp.limiting() > 1.0 {
                g += &format!(" (lim -{:.0})", amp.limiting());
            }
            let line = format!("mic {:4.0} phones {:4.0} out {:4.0} dB | {state}{g}", st.mic_db, st.ref_db, st.out_db);
            print!("\r{line:<110}");
            use std::io::Write;
            let _ = std::io::stdout().flush();
        }
    }
    G.out_lat_ms.store(0, Relaxed);
    crate::win::sys::log_event("стоп вывода");
    unsafe {
        let _ = ac.Stop();
    }
}

// проигрывает моно-звук, сгенерированный под частоту устройства
pub fn play_samples(dev: &IMMDevice, gen: impl Fn(usize) -> Vec<f32>) -> Result<Instant, String> {
    let (ac, mf, f) = open_client(dev)?;
    unsafe { ac.Initialize(AUDCLNT_SHAREMODE_SHARED, 0, 2_000_000, 0, mf.0, None) }.map_err(|e| format!("Initialize: {e}"))?;
    let buf_frames = unsafe { ac.GetBufferSize() }.unwrap_or(0) as usize;
    let arc: IAudioRenderClient = unsafe { ac.GetService() }.map_err(|e| e.to_string())?;
    let tone = gen(f.rate);
    let mut pos = 0usize;
    let mut enc = Vec::new();
    let mut write = |pos: &mut usize| -> Result<(), String> {
        let pad = unsafe { ac.GetCurrentPadding() }.map_err(|e| e.to_string())? as usize;
        let n = buf_frames.saturating_sub(pad).min(tone.len() - *pos);
        if n == 0 {
            return Ok(());
        }
        enc.resize(n * f.block_align, 0);
        f.encode_mono(&tone[*pos..*pos + n], &mut enc);
        unsafe {
            let dst = arc.GetBuffer(n as u32).map_err(|e| e.to_string())?;
            std::ptr::copy_nonoverlapping(enc.as_ptr(), dst, enc.len());
            let _ = arc.ReleaseBuffer(n as u32, 0);
        }
        *pos += n;
        Ok(())
    };
    write(&mut pos)?;
    unsafe { ac.Start() }.map_err(|e| e.to_string())?;
    let t0 = Instant::now();
    while pos < tone.len() {
        std::thread::sleep(Duration::from_millis(5));
        if let Err(e) = write(&mut pos) {
            unsafe {
                let _ = ac.Stop();
            }
            return Err(e);
        }
    }
    std::thread::sleep(Duration::from_secs_f64(buf_frames as f64 / f.rate as f64) + Duration::from_millis(100));
    unsafe {
        let _ = ac.Stop();
    }
    Ok(t0)
}
