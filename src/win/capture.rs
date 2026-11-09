use std::sync::atomic::{AtomicBool, Ordering::Relaxed};
use std::sync::mpsc::Sender;
use std::sync::Arc;
use std::time::{Duration, Instant};

use windows::core::PCWSTR;
use windows::Win32::Foundation::{CloseHandle, HANDLE, WAIT_OBJECT_0};
use windows::Win32::Media::Audio::*;
use windows::Win32::System::Threading::{CreateEventW, WaitForSingleObject};

use hush::audio::{mix_lr, mono_of, Format, Resampler};
use hush::l;
use hush::pipe::{StreamIn, RATE};

use super::dev::*;
use super::state::{note_stall, Ev, G};
use super::sys::{now_qpc, Com, ProAudio, Tick};

pub struct Ready {
    pub label: String,
    pub name: String,
    pub f: Format,
    pub err: Option<String>,
}

#[derive(Clone, Copy, PartialEq)]
pub enum Mode {
    Mono,
    Lr,
    Raw,
}

#[derive(Clone, Copy, PartialEq)]
pub enum How {
    Normal,
    LowLat,
    ExclLow,
}

pub struct CapArgs {
    pub label: String,
    pub sub: String,
    pub loopback: bool,
    pub mode: Mode,
    pub stream: Arc<StreamIn>,
    pub stop: Arc<AtomicBool>,
    pub ready: Sender<Ready>,
    pub ev: Sender<Ev>,
    pub sess: u64,
    pub how: How,
}

struct Event(HANDLE);
impl Drop for Event {
    fn drop(&mut self) {
        unsafe {
            let _ = CloseHandle(self.0);
        }
    }
}

// почему микрофон не открылся монопольно и что делать
fn mic_excl_fail(dev: &IMMDevice, mic_name: &str, err: &ExclErr) {
    let ru = hush::lang::is_ru();
    let (short, long) = match err {
        ExclErr::Win(e) if hr(e) == HR_IN_USE => {
            let me = super::sys::exe_path().file_name().map(|s| s.to_string_lossy().to_lowercase()).unwrap_or_default();
            let mut act = Vec::new();
            let mut idle = Vec::new();
            for s in device_sessions(dev) {
                if act.contains(&s.exe) || idle.contains(&s.exe) || s.exe.to_lowercase() == me {
                    continue;
                }
                if s.active {
                    act.push(s.exe);
                } else {
                    idle.push(s.exe);
                }
            }
            let who: Vec<String> = act.into_iter().chain(idle).collect();
            let (short, mut long) = if who.is_empty() {
                (
                    l("микрофон занят", "mic is busy").to_string(),
                    l("Микрофон открыт другой программой, монопольно его не взять.", "Another program has the mic open, so it can't be captured exclusively.").to_string(),
                )
            } else {
                let mut first: String = who[0].chars().take(18).collect();
                if who[0].chars().count() > 18 {
                    first = who[0].chars().take(17).collect::<String>() + "…";
                }
                let mut s = format!("{}{}", l("занят: ", "used by "), first);
                if who.len() > 1 {
                    s += &if ru { format!(" и ещё {}", who.len() - 1) } else { format!(" and {} more", who.len() - 1) };
                }
                (s, format!("{}{}.", l("Микрофон держат: ", "The mic is held by: "), who.join(", ")))
            };
            long += l(
                " Закрой их или выбери у них микрофоном «CABLE Output» и включи монопольный захват снова.",
                " Close them or set their mic to \"CABLE Output\", then turn exclusive capture on again.",
            );
            (short, long)
        }
        ExclErr::Loses => (
            l("теряет звук", "drops audio").to_string(),
            l("В монопольном режиме этот микрофон теряет звук, оставлен обычный режим.", "This mic drops audio in exclusive mode, shared mode is kept.").to_string(),
        ),
        ExclErr::Win(e) if hr(e) == HR_EXCL_NOT_ALLOWED => (
            l("запрещено в Windows", "disabled in Windows").to_string(),
            if ru {
                format!("В Windows запрещён монопольный режим. Звук → Запись → «{mic_name}» → Свойства → Дополнительно → «Разрешить приложениям использовать устройство в монопольном режиме».")
            } else {
                format!("Exclusive mode is disabled in Windows. Sound → Recording → \"{mic_name}\" → Properties → Advanced → \"Allow applications to take exclusive control of this device\".")
            },
        ),
        ExclErr::Win(e) => (
            l("драйвер не дал", "driver refused").to_string(),
            format!("{}{}{}", l("Драйвер микрофона не принял монопольный режим (", "The mic driver refused exclusive mode ("), audio_err(e), l("). Работаю в обычном.", "). Using shared mode.")),
        ),
        ExclErr::Other(s) => (
            l("драйвер не дал", "driver refused").to_string(),
            format!("{}{}{}", l("Драйвер микрофона не принял монопольный режим (", "The mic driver refused exclusive mode ("), first_line(s), l("). Работаю в обычном.", "). Using shared mode.")),
        ),
    };
    *G.mic_excl_why.lock().unwrap() = short;
    println!("\n({}: {long})", l("монопольный режим микрофона", "exclusive mic mode"));
    let mut shown = G.mic_excl_shown.lock().unwrap();
    if *shown != long {
        *shown = long.clone();
        super::ui::balloon(l("Монопольный захват не вышел", "Exclusive capture failed"), &long);
    }
}

pub fn run_capture(a: CapArgs) {
    let _com = Com::init();
    let _pa = ProAudio::enter();
    let report = a.how != How::Normal;
    if report {
        G.mic_period_us.store(0, Relaxed);
        G.mic_excl_active.store(false, Relaxed);
        G.mic_excl_why.lock().unwrap().clear();
        G.mic_glitches.store(0, Relaxed);
        G.mic_own_clock.store(false, Relaxed);
        G.mic_loss_pct.store(0, Relaxed);
    }
    let fail = |e: String| {
        let _ = a.ready.send(Ready { label: a.label.clone(), name: String::new(), f: Format::default(), err: Some(format!("{}: {e}", a.label)) });
    };
    let flow = if a.loopback { eRender } else { eCapture };
    let (dev, name) = match pick_device(flow, &a.sub, &a.label, false) {
        Ok(v) => v,
        Err(e) => {
            let _ = a.ready.send(Ready { label: a.label.clone(), name: String::new(), f: Format::default(), err: Some(e) });
            return;
        }
    };
    if a.loopback && is_cable(&name) {
        fail(if hush::lang::is_ru() {
            format!("устройство вывода по умолчанию — «{name}». Выбери наушники в меню hush или сделай их устройством по умолчанию")
        } else {
            format!("the default output device is \"{name}\". Pick your headphones in the hush menu or make them the default device")
        });
        return;
    }

    let mut ac: Option<IAudioClient> = None;
    let mut f = Format::default();
    let mut event: Option<Event> = None;
    let mut period_frames = 0u32;
    let mut note = String::new();
    if !a.loopback && a.how != How::Normal {
        let open_event = |c: &IAudioClient| -> Option<Event> {
            unsafe {
                let h = CreateEventW(None, false, false, PCWSTR::null()).ok()?;
                let ev = Event(h);
                c.SetEventHandle(h).ok()?;
                Some(ev)
            }
        };
        if a.how == How::ExclLow {
            match open_capture_exclusive(&dev, G.mic_excl_min_p.load(Relaxed)) {
                Ok((c, ff, pf)) => {
                    if let Some(e) = open_event(&c) {
                        event = Some(e);
                        ac = Some(c);
                        f = ff;
                        period_frames = pf;
                        G.mic_excl_active.store(true, Relaxed);
                        note = format!(" [{}]", l("монопольно", "exclusive"));
                    }
                }
                Err(e) => mic_excl_fail(&dev, &name, &e),
            }
        }
        if ac.is_none() {
            if let Ok((c, ff, pf)) = open_capture_low_lat(&dev) {
                if let Some(e) = open_event(&c) {
                    event = Some(e);
                    ac = Some(c);
                    f = ff;
                    period_frames = pf;
                }
            }
        }
    }
    let ac = match ac {
        Some(c) => c,
        None => {
            let (c, mf, ff) = match open_client(&dev) {
                Ok(v) => v,
                Err(e) => return fail(e),
            };
            let flags = if a.loopback { AUDCLNT_STREAMFLAGS_LOOPBACK } else { 0 };
            if let Err(e) = unsafe { c.Initialize(AUDCLNT_SHAREMODE_SHARED, flags, 1_000_000, 0, mf.0, None) } {
                return fail(format!("Initialize: {e}"));
            }
            f = ff;
            let (mut def_p, mut min_p) = (0i64, 0i64);
            period_frames = if unsafe { c.GetDevicePeriod(Some(&mut def_p), Some(&mut min_p)) }.is_ok() && def_p > 0 {
                (def_p * f.rate as i64 / 10_000_000) as u32
            } else {
                (f.rate / 100) as u32
            };
            c
        }
    };
    if f.rate > 0 && period_frames > 0 {
        let us = period_frames as i64 * 1_000_000 / f.rate as i64;
        if report {
            G.mic_period_us.store(us, Relaxed);
        }
        note += &format!(" [{} {:.1} {}]", l("пачки", "packets"), us as f64 / 1000.0, l("мс", "ms"));
    }
    let acc: IAudioCaptureClient = match unsafe { ac.GetService() } {
        Ok(v) => v,
        Err(e) => return fail(format!("GetService: {e}")),
    };
    if let Err(e) = unsafe { ac.Start() } {
        return fail(format!("Start: {e}"));
    }
    let _ = a.ready.send(Ready { label: a.label.clone(), name: name.clone() + &note, f, err: None });

    let nout = match a.mode {
        Mode::Mono => 1,
        Mode::Lr => 2,
        Mode::Raw => f.channels,
    };
    let mut rss: Vec<Resampler> = (0..nout).map(|_| Resampler::new(f.rate, RATE, false)).collect();
    let mut chs: Vec<Vec<f32>> = vec![Vec::new(); f.channels];
    let mut mixed: Vec<Vec<f32>> = vec![Vec::new(); nout];
    let mut outs: Vec<Vec<f32>> = vec![Vec::new(); nout];
    let (mut last_q, mut last_dur) = (0i64, 0i64);
    let mut bad_ts = 0;
    let (mut glitch_n, mut glitch_t, mut started) = (0, Instant::now(), false);
    let mut rate_got = 0i64;
    let mut first_pkt = Instant::now();
    let mut measuring = false;
    let tk = Tick::new();
    let (st_n, st_max) = if a.loopback { (&G.ref_stalls, &G.ref_stall_max) } else { (&G.mic_stalls, &G.mic_stall_max) };
    if report || a.loopback {
        st_n.store(0, Relaxed);
        st_max.store(0, Relaxed);
    }
    let err = |m: String, drops: bool| {
        let _ = a.ev.send(Ev::Err { sess: a.sess, msg: m, drops });
    };
    let mut last_wake = Instant::now();
    while !a.stop.load(Relaxed) {
        let mut timed_out = false;
        if let Some(e) = &event {
            timed_out = unsafe { WaitForSingleObject(e.0, 100) } != WAIT_OBJECT_0;
        } else {
            tk.wait();
        }
        if (report || a.loopback) && !timed_out {
            note_stall(st_n, st_max, last_wake.elapsed());
        }
        last_wake = Instant::now();
        loop {
            let n = match unsafe { acc.GetNextPacketSize() } {
                Ok(n) => n,
                Err(e) => {
                    let _ = unsafe { ac.Stop() };
                    return err(format!("{}: GetNextPacketSize: {e} ({})", a.label, l("устройство отключено?", "device unplugged?")), false);
                }
            };
            if n == 0 {
                break;
            }
            let mut data: *mut u8 = std::ptr::null_mut();
            let (mut frames, mut bflags) = (0u32, 0u32);
            let (mut dev_pos, mut qpc) = (0u64, 0u64);
            if let Err(e) = unsafe { acc.GetBuffer(&mut data, &mut frames, &mut bflags, Some(&mut dev_pos), Some(&mut qpc)) } {
                let _ = unsafe { ac.Stop() };
                return err(format!("{}: GetBuffer: {e}", a.label), false);
            }
            if bflags & AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY.0 as u32 != 0 && started {
                glitch_n += 1;
                if report {
                    G.mic_glitches.fetch_add(1, Relaxed);
                }
            }
            if !started {
                first_pkt = Instant::now();
            }
            started = true;

            let excl = G.mic_excl_active.load(Relaxed) && report;
            if excl && !measuring && first_pkt.elapsed() > Duration::from_secs(1) {
                measuring = true;
                glitch_n = 0;
                glitch_t = Instant::now();
                rate_got = 0;
            } else if excl && measuring {
                rate_got += frames as i64;
                let el = glitch_t.elapsed();
                if el > Duration::from_secs(3) {
                    let want = el.as_secs_f64() * f.rate as f64;
                    let loss = (1.0 - rate_got as f64 / want).max(0.0);
                    G.mic_loss_pct.store((loss * 1000.0 + 0.5) as i64, Relaxed);
                    if loss > 0.005 || glitch_n >= 10 {
                        let cur = period_frames as i64 * 10_000_000 / f.rate as i64;
                        let next = cur + 20000;
                        let ru = hush::lang::is_ru();
                        let why = if next > 100_000 {
                            G.mic_excl_min_p.store(-1, Relaxed);
                            if ru {
                                format!("монопольный микрофон теряет звук: {:.0}% даже пачками по {:.1} мс, включён обычный захват", loss * 100.0, cur as f64 / 10000.0)
                            } else {
                                format!("exclusive mic drops audio: {:.0}% even with {:.1} ms packets, switched to shared capture", loss * 100.0, cur as f64 / 10000.0)
                            }
                        } else {
                            G.mic_excl_min_p.store(next, Relaxed);
                            if ru {
                                format!("монопольный микрофон теряет звук: {:.0}%, сбоев {glitch_n}, пробую пачки по {:.1} мс", loss * 100.0, next as f64 / 10000.0)
                            } else {
                                format!("exclusive mic drops audio: {:.0}%, {glitch_n} glitches, trying {:.1} ms packets", loss * 100.0, next as f64 / 10000.0)
                            }
                        };
                        unsafe {
                            let _ = acc.ReleaseBuffer(frames);
                            let _ = ac.Stop();
                        }
                        return err(why, true);
                    }
                    glitch_n = 0;
                    glitch_t = Instant::now();
                    rate_got = 0;
                }
            }
            for c in chs.iter_mut() {
                c.clear();
            }
            if bflags & AUDCLNT_BUFFERFLAGS_SILENT.0 as u32 != 0 || data.is_null() {
                for c in chs.iter_mut() {
                    c.resize(frames as usize, 0.0);
                }
            } else {
                let buf = unsafe { std::slice::from_raw_parts(data, frames as usize * f.block_align) };
                f.decode_channels(buf, frames as usize, &mut chs);
            }
            unsafe {
                let _ = acc.ReleaseBuffer(frames);
            }

            match a.mode {
                Mode::Mono => {
                    mixed[0].clear();
                    mono_of(&chs, &mut mixed[0]);
                }
                Mode::Lr => {
                    let (l0, r0) = mixed.split_at_mut(1);
                    l0[0].clear();
                    r0[0].clear();
                    mix_lr(&chs, &mut l0[0], &mut r0[0]);
                }
                Mode::Raw => {
                    for (m, c) in mixed.iter_mut().zip(&chs) {
                        m.clear();
                        m.extend_from_slice(c);
                    }
                }
            }

            let dur = frames as i64 * 10_000_000 / f.rate as i64;
            let now = now_qpc();
            let obs = now - dur;
            let mut q = qpc as i64;
            if bflags & AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR.0 as u32 != 0 || q == 0 || q > now + 20000 || now - q > dur + 500_000 {
                bad_ts += 1;
                q = obs;
                if last_q != 0 {
                    let exp = last_q + last_dur;
                    let d = obs - exp;
                    if !(-300_000..=300_000).contains(&d) {
                    } else if d < 0 {
                        q = obs;
                    } else {
                        q = exp + d / 200;
                    }
                }
            }
            last_q = q;
            last_dur = dur;
            if report && bad_ts == 50 {
                G.mic_own_clock.store(true, Relaxed);
            }
            for i in 0..nout {
                outs[i].clear();
                rss[i].process(&mixed[i], &mut outs[i]);
            }
            a.stream.push(&outs, q);
        }
    }
    unsafe {
        let _ = ac.Stop();
    }
}
