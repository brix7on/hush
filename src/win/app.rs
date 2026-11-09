use std::collections::VecDeque;
use std::fs;
use std::io::Write;
use std::sync::atomic::{AtomicBool, Ordering::Relaxed};
use std::sync::mpsc::{channel, Receiver, RecvTimeoutError, Sender};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use windows::core::BOOL;
use windows::Win32::Media::Audio::{eCapture, eRender};
use windows::Win32::System::Console::SetConsoleCtrlHandler;

use hush::dsp::agc::{Agc, AgcCtl};
use hush::dsp::amp::{Amp, AmpCtl};
use hush::dsp::denoise::{DnCtl, Denoiser};
use hush::dsp::eq::{Eq, EqCtl};
use hush::dsp::suppress::{Calib, SupShared, Suppressor};
use hush::panel::{Action, UiState};
use hush::pipe::{idx_from_qpc, Chain, Engine, EngineStats, StreamIn, RATE};
use hush::{diag, l, lf, APP, VERSION};

use super::capture::{run_capture, CapArgs, How, Mode, Ready};
use super::dev::*;
use super::ptt::{key_name, Cues, Ptt, PTT_OFF};
use super::render::{play_samples, run_render, RenderArgs};
use super::state::{Ev, G};
use super::sys::*;
use super::{ui, update};
use crate::logf;

const MAX_GAIN_DB: f64 = 45.0;
const DEFAULT_IDLE_SLEEP_MIN: i64 = 15;
const VB_CABLE_URL: &str = "https://vb-audio.com/Cable/";

fn is_false(v: &bool) -> bool {
    !*v
}
fn is_zero_i(v: &i64) -> bool {
    *v == 0
}
fn is_zero_f(v: &f64) -> bool {
    *v == 0.0
}

#[derive(Clone, Default, Serialize, Deserialize)]
pub struct Config {
    #[serde(default)]
    pub gain_db: f64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub mic: String,
    #[serde(default, rename = "ref", skip_serializing_if = "String::is_empty")]
    pub reff: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub out: String,
    #[serde(default, skip_serializing_if = "is_zero_f")]
    pub beta: f64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub noise: Option<i32>,
    #[serde(default, skip_serializing_if = "is_zero_i")]
    pub eq: i64,
    #[serde(default, skip_serializing_if = "is_false")]
    pub agc: bool,
    #[serde(default, rename = "ptt_mode", skip_serializing_if = "is_zero_i")]
    pub ptt: i64,
    #[serde(default, rename = "ptt_key", skip_serializing_if = "is_zero_i")]
    pub ptt_key: i64,
    #[serde(default, rename = "ptt_no_sound", skip_serializing_if = "is_false")]
    pub no_cue: bool,
    #[serde(default, skip_serializing_if = "is_false")]
    pub shared: bool,
    #[serde(default, rename = "mic_excl", skip_serializing_if = "is_false")]
    pub mic_ex: bool,
    #[serde(default, rename = "autostart_set", skip_serializing_if = "is_false")]
    pub auto_set: bool,
    #[serde(default, skip_serializing_if = "is_false")]
    pub no_idle_sleep: bool,
    #[serde(default, skip_serializing_if = "is_zero_i")]
    pub idle_sleep_min: i64,
}

static CFG: Mutex<Option<Config>> = Mutex::new(None);

fn load_config() -> Config {
    fs::read(data_file("config.json")).ok().and_then(|b| serde_json::from_slice(&b).ok()).unwrap_or_default()
}

fn update_config(f: impl FnOnce(&mut Config)) {
    let c = {
        let mut g = CFG.lock().unwrap();
        let c = g.get_or_insert_with(Config::default);
        f(c);
        c.clone()
    };
    if let Ok(b) = serde_json::to_vec_pretty(&c) {
        let _ = fs::write(data_file("config.json"), b);
    }
}

pub fn idle_sleep_min() -> i64 {
    let m = CFG.lock().unwrap().as_ref().map_or(0, |c| c.idle_sleep_min);
    if m <= 0 {
        DEFAULT_IDLE_SLEEP_MIN
    } else {
        m
    }
}

// ---------- флаги ----------

struct Flags {
    mic: String,
    out: String,
    reff: String,
    list: bool,
    doctor: bool,
    record: i64,
    bypass: bool,
    gain: Option<f64>,
    recal: bool,
    monoref: bool,
    delay_ms: i64,
    micexcl: Option<bool>,
    shared: Option<bool>,
    beta: Option<f64>,
    quiet: bool,
    noui: bool,
    background: bool,
    console: bool,
    after_update: bool,
    version: bool,
    help: bool,
    mic_set: bool,
    ref_set: bool,
    out_set: bool,
}

fn parse_flags() -> Flags {
    let mut f = Flags {
        mic: String::new(),
        out: "CABLE Input".into(),
        reff: String::new(),
        list: false,
        doctor: false,
        record: 0,
        bypass: false,
        gain: None,
        recal: false,
        monoref: false,
        delay_ms: 20,
        micexcl: None,
        shared: None,
        beta: None,
        quiet: false,
        noui: false,
        background: false,
        console: false,
        after_update: false,
        version: false,
        help: false,
        mic_set: false,
        ref_set: false,
        out_set: false,
    };
    let args: Vec<String> = std::env::args().skip(1).collect();
    let mut i = 0;
    while i < args.len() {
        let a = args[i].trim_start_matches('-');
        let (key, inline) = match a.split_once('=') {
            Some((k, v)) => (k.to_string(), Some(v.to_string())),
            None => (a.to_string(), None),
        };
        let mut val = || -> String {
            if let Some(v) = &inline {
                return v.clone();
            }
            i += 1;
            args.get(i).cloned().unwrap_or_default()
        };
        let flag = |v: &Option<String>| v.as_deref().map_or(true, |s| s != "false" && s != "0");
        match key.as_str() {
            "mic" => {
                f.mic = val();
                f.mic_set = true;
            }
            "out" => {
                f.out = val();
                f.out_set = true;
            }
            "ref" => {
                f.reff = val();
                f.ref_set = true;
            }
            "record" => f.record = val().parse().unwrap_or(0),
            "gain" => f.gain = val().parse().ok(),
            "delay-ms" => f.delay_ms = val().parse().unwrap_or(20),
            "beta" => f.beta = val().parse().ok(),
            "mu" | "partitions" => {
                val();
            }
            "list" => f.list = flag(&inline),
            "doctor" => f.doctor = flag(&inline),
            "bypass" => f.bypass = flag(&inline),
            "recal" => f.recal = flag(&inline),
            "monoref" => f.monoref = flag(&inline),
            "micexcl" => f.micexcl = Some(flag(&inline)),
            "shared" => f.shared = Some(flag(&inline)),
            "quiet" => f.quiet = flag(&inline),
            "noui" => f.noui = flag(&inline),
            "background" => f.background = flag(&inline),
            "console" => f.console = flag(&inline),
            "after-update" => f.after_update = flag(&inline),
            "version" => f.version = flag(&inline),
            "h" | "help" | "?" => f.help = true,
            _ => {}
        }
        i += 1;
    }
    f
}

fn usage() {
    println!("hush {VERSION}\n");
    let rows: [(&str, &str, &str); 17] = [
        ("-mic NAME", "часть названия микрофона (по умолчанию — микрофон Windows)", "part of the microphone name (default: Windows default mic)"),
        ("-ref NAME", "часть названия наушников (по умолчанию — вывод Windows)", "part of the headphones name (default: Windows default output)"),
        ("-out NAME", "часть названия устройства вывода (виртуальный кабель)", "part of the output device name (virtual cable)"),
        ("-list", "показать устройства и выйти", "list devices and exit"),
        ("-doctor", "найти, откуда берётся эхо (проиграет короткое шипение)", "find where an echo comes from (plays a short noise)"),
        ("-record N", "записать N секунд в hush-rec.zip и выйти", "record N seconds to hush-rec.zip and exit"),
        ("-recal", "заново откалибровать при запуске", "calibrate again on start"),
        ("-gain DB", "усиление микрофона, дБ", "mic gain, dB"),
        ("-beta X", "сила подавителя (1 — мягко, 1.5 — обычно, 2 — жёстко)", "suppression strength (1 soft, 1.5 normal, 2 strong)"),
        ("-micexcl", "захватывать микрофон монопольно", "capture the mic exclusively"),
        ("-shared", "выводить в кабель обычным режимом", "shared cable output"),
        ("-monoref", "сводить наушники в одну опору", "mix headphone channels into one reference"),
        ("-bypass", "ничего не вычитать (для сравнения)", "pass the mic through unprocessed"),
        ("-console", "показывать консоль со строкой состояния", "show a console with live levels"),
        ("-noui", "без панели и значка в трее", "no panel or tray icon"),
        ("-background", "запуск в трее без панели (автозапуск)", "start in the tray without the panel (autostart)"),
        ("-version", "показать версию", "print the version"),
    ];
    for (k, ru, en) in rows {
        println!("  {k:<12} {}", l(ru, en));
    }
}

// ---------- приложение ----------

struct App {
    chain: Arc<Mutex<Chain>>,
    sup: Arc<SupShared>,
    dn: Arc<DnCtl>,
    eq: Arc<EqCtl>,
    agc: Arc<AgcCtl>,
    amp: Arc<AmpCtl>,
    names: Mutex<(String, String, String)>,
    shared: AtomicBool,
    mic_excl: AtomicBool,
    ptt: Arc<Ptt>,
    cues: Arc<Cues>,
    eng_stats: Mutex<Option<Arc<EngineStats>>>,
    bypass: bool,
    monoref: bool,
    delay_ms: i64,
    quiet: bool,
    tx: Sender<Ev>,
    backlog: Mutex<VecDeque<Ev>>,
}

static TX: Mutex<Option<Sender<Ev>>> = Mutex::new(None);

unsafe extern "system" fn ctrl_handler(_: u32) -> BOOL {
    if let Some(tx) = TX.lock().unwrap().as_ref() {
        let _ = tx.send(Ev::Quit);
    }
    BOOL(1)
}

impl App {
    fn state(&self) -> UiState {
        let (mic, reff, out) = self.names.lock().unwrap().clone();
        let u = update::state();
        let ss = self.sup.stats();
        let running = G.running.load(Relaxed);
        let lat = G.out_lat_ms.load(Relaxed);
        let mut st = UiState {
            running,
            note: G.ui_note.lock().unwrap().clone(),
            latency_ms: if lat > 0 { lat } else { G.out_buf_ms.load(Relaxed) + 16 },
            mic_period_ms: G.mic_period_us.load(Relaxed) as f64 / 1000.0,
            mic_ex_want: self.mic_excl.load(Relaxed),
            mic_ex_on: G.mic_excl_active.load(Relaxed),
            mic_ex_why: G.mic_excl_why.lock().unwrap().clone(),
            out_starves: G.out_starves.load(Relaxed),
            mic_glitches: G.mic_glitches.load(Relaxed),
            mic_own_clock: G.mic_own_clock.load(Relaxed),
            mic_loss_pct: G.mic_loss_pct.load(Relaxed) as f64 / 10.0,
            excl_want: !self.shared.load(Relaxed),
            excl_on: G.excl_active.load(Relaxed),
            gain_db: self.amp.gain_db(),
            limiting: self.amp.limiting(),
            noise: self.dn.strength(),
            eq: self.eq.preset(),
            agc: self.agc.enabled(),
            agc_gain: self.agc.gain_db(),
            ptt: self.ptt.mode.load(Relaxed),
            ptt_key: key_name(self.ptt.key.load(Relaxed)),
            ptt_open: self.ptt.open.load(Relaxed),
            binding: self.ptt.binding.load(Relaxed),
            cue: self.cues.enabled.load(Relaxed),
            upd_latest: u.latest,
            upd_busy: u.busy,
            upd_msg: u.msg,
            mic,
            reff,
            out,
            beta: self.sup.beta(),
            mic_db: ss.mic_db,
            ref_db: ss.ref_db,
            reduction: ss.reduction_db,
            voice: ss.voice,
            calibrating: ss.calibrating,
            found: ss.found,
            out_db: self.eng_stats.lock().unwrap().as_ref().map_or(-120.0, |e| e.out_db()),
            ..Default::default()
        };
        if !running {
            st.out_db = -120.0;
            st.mic_db = -120.0;
        }
        st
    }

    // true — пересобрать цепочку
    fn handle(&self, a: &Action, quit: &mut bool, recal_next: &mut bool) -> bool {
        match a.kind {
            "gain" => {
                let g = (self.amp.gain_db() + a.f).clamp(-10.0, MAX_GAIN_DB);
                self.amp.set_gain_db(g);
                update_config(|c| c.gain_db = g);
            }
            "beta" => {
                self.sup.set_beta(a.f);
                update_config(|c| c.beta = a.f);
            }
            "noise" => {
                let lv = a.f as i32;
                self.dn.set_strength(lv);
                update_config(|c| c.noise = Some(lv));
            }
            "eq" => {
                self.eq.set_preset(a.f as i32);
                let p = self.eq.preset() as i64;
                update_config(|c| c.eq = p);
            }
            "agc" => {
                let on = match a.f as i32 {
                    0 => false,
                    1 => true,
                    _ => !self.agc.enabled(),
                };
                self.agc.set_enabled(on);
                update_config(|c| c.agc = on);
            }
            "ptt" => {
                let m = a.f as i32;
                self.ptt.mode.store(m, Relaxed);
                update_config(|c| c.ptt = m as i64);
                if m != PTT_OFF && self.ptt.key.load(Relaxed) == 0 {
                    self.ptt.binding.store(true, Relaxed);
                    ui::balloon(APP, l("Нажми клавишу или кнопку мыши для микрофона", "Press a key or mouse button for the mic"));
                }
            }
            "bind" => self.ptt.binding.store(true, Relaxed),
            "excl" => {
                let want_shared = a.f != 0.0;
                if want_shared == self.shared.load(Relaxed) {
                    return false;
                }
                self.shared.store(want_shared, Relaxed);
                update_config(|c| c.shared = want_shared);
                return true;
            }
            "micexcl" => {
                let want = a.f == 1.0;
                if want == self.mic_excl.load(Relaxed) {
                    return false;
                }
                self.mic_excl.store(want, Relaxed);
                G.mic_excl_min_p.store(0, Relaxed);
                G.mic_excl_shown.lock().unwrap().clear();
                update_config(|c| c.mic_ex = want);
                if want {
                    ui::balloon(APP, l("Микрофон захватывается монопольно. Если наводка вернётся, перекалибруй.", "The mic is now captured exclusively. If the bleed comes back, recalibrate."));
                }
                return true;
            }
            "cue" => {
                let on = !self.cues.enabled.load(Relaxed);
                self.cues.enabled.store(on, Relaxed);
                update_config(|c| c.no_cue = !on);
                if on {
                    self.cues.play(true);
                }
            }
            "mic" | "ref" | "out" => {
                let s = a.s.clone();
                {
                    let mut n = self.names.lock().unwrap();
                    match a.kind {
                        "mic" => n.0 = s.clone(),
                        "ref" => n.1 = s.clone(),
                        _ => n.2 = s.clone(),
                    }
                }
                update_config(|c| match a.kind {
                    "mic" => c.mic = s,
                    "ref" => c.reff = s,
                    _ => c.out = s,
                });
                return true;
            }
            "recal" => {
                *recal_next = true;
                return true;
            }
            "resume" => {
                log_event("выход из сна — переподключаю устройства");
                std::thread::sleep(Duration::from_secs(2));
                return true;
            }
            "autostart" => match set_autostart(!autostart_on()) {
                Err(e) => ui::balloon(APP, &format!("{}{e}", l("Автозапуск: ", "Autostart: "))),
                Ok(()) if autostart_on() => ui::balloon(APP, l("Запуск вместе с Windows включён", "Start with Windows is on")),
                Ok(()) => ui::balloon(APP, l("Запуск вместе с Windows выключен", "Start with Windows is off")),
            },
            "idlesleep" => {
                let on = !G.idle_sleep_on.load(Relaxed);
                G.idle_sleep_on.store(on, Relaxed);
                update_config(|c| c.no_idle_sleep = !on);
                if on {
                    ui::balloon(APP, &lf!("ПК уснёт после {} мин тишины без мыши и клавиатуры", "The PC will sleep after {} min of silence and no input", idle_sleep_min()));
                } else {
                    ui::balloon(APP, l("Сон при тишине выключен", "Sleep on silence is off"));
                }
            }
            "log" => open_url(&data_file("log.txt").to_string_lossy()),
            "folder" => open_url(&data_dir().to_string_lossy()),
            "update" => {
                let tx = self.tx.clone();
                std::thread::spawn(move || {
                    let fail = |e: String| {
                        logf!("обновление: {e}");
                        ui::balloon(l("Обновление не удалось", "Update failed"), &e);
                    };
                    match update::install(data_dir()) {
                        Err(e) => fail(e),
                        Ok(exe) => {
                            logf!("обновлено до {}, перезапуск", update::state().latest);
                            match update::restart_after_update(&exe) {
                                Err(e) => fail(e),
                                Ok(()) => {
                                    let _ = tx.send(Ev::Quit);
                                }
                            }
                        }
                    }
                });
            }
            "checkupd" => {
                std::thread::spawn(|| match update::check() {
                    Err(e) => ui::balloon(APP, &format!("{}{e}", l("Не удалось проверить обновления: ", "Update check failed: "))),
                    Ok((tag, _)) if !tag.is_empty() => ui::balloon(APP, &lf!("Доступна версия {}", "Version {} is available", tag)),
                    Ok(_) => ui::balloon(APP, &lf!("Установлена последняя версия ({})", "You have the latest version ({})", VERSION)),
                });
            }
            "quit" => {
                *quit = true;
                return true;
            }
            _ => {}
        }
        false
    }
}

fn calib_path() -> std::path::PathBuf {
    data_file("calib.json")
}

fn save_calib(c: &Calib) -> std::io::Result<()> {
    let b = serde_json::to_vec_pretty(c).map_err(std::io::Error::other)?;
    fs::write(calib_path(), b)
}

fn load_calib() -> Option<Calib> {
    fs::read(calib_path()).ok().and_then(|b| serde_json::from_slice::<Calib>(&b).ok()).filter(|c| c.found)
}

// загружает калибровку из файла или проводит новую
fn calibrate(app: &App, rx: &Receiver<Ev>, sess: u64, force: bool) -> Result<(), String> {
    let sup = &app.sup;
    if !force {
        if let Some(c) = load_calib() {
            let d = c.d;
            if sup.set_calib(c) {
                println!("{}", lf!("Калибровка загружена: наводка с задержкой {:.0} мс", "Calibration loaded: bleed delay {:.0} ms", (d * sup.hop()) as f64 * 1000.0 / sup.rate() as f64));
            }
        }
        return Ok(());
    }
    let reff = app.names.lock().unwrap().1.clone();
    let (dev, name) = pick_device(eRender, &reff, l("наушники", "headphones"), false)?;
    println!("{}", l("КАЛИБРОВКА: молчи ~7 секунд, в наушниках будет шипение.", "CALIBRATION: stay quiet for ~7 seconds, you will hear noise in the headphones."));
    ui::balloon(l("Калибровка", "Calibration"), l("Помолчи 7 секунд: в наушниках будет шипение.", "Stay quiet for 7 seconds: you will hear noise in the headphones."));
    std::thread::sleep(Duration::from_millis(1200));
    let frames = (diag::PERIOD_MS * diag::PERIODS + 1500) * sup.rate() / 1000 / sup.hop();
    sup.start_calib(frames);
    std::thread::sleep(Duration::from_millis(200));
    play_samples(&dev, |r| diag::tone(r, 0.1)).map_err(|e| lf!("не удалось проиграть шипение в «{}»: {}", "could not play the test noise on \"{}\": {}", name, e))?;
    let deadline = Instant::now() + Duration::from_secs(15);
    let c = loop {
        if let Some(c) = sup.wait_calib(Duration::from_millis(100)) {
            break c;
        }
        while let Ok(ev) = rx.try_recv() {
            match ev {
                Ev::Err { sess: s, msg, .. } if s == sess => return Err(msg),
                Ev::Err { .. } => {}
                other => app.backlog.lock().unwrap().push_back(other),
            }
        }
        if Instant::now() > deadline {
            return Err(l("калибровка не завершилась: нет звука с микрофона или наушников", "calibration did not finish: no audio from the mic or headphones").into());
        }
    };
    if c.found {
        let mut msg = lf!(
            "Наводка найдена: задержка {:.0} мс, на {:.0} дБ громче шума.",
            "Bleed found: {:.0} ms delay, {:.0} dB above the noise.",
            (c.d * sup.hop()) as f64 * 1000.0 / sup.rate() as f64,
            c.leak_db
        );
        if c.leak_db < 6.0 {
            msg += l(" Слабо: перекалибруй при обычной громкости наушников.", " Weak: recalibrate at your usual headphone volume.");
        }
        println!("{msg}");
        ui::balloon(l("Калибровка готова", "Calibration done"), &msg);
        if let Err(e) = save_calib(&c) {
            println!("({}: {e})", l("не смог сохранить калибровку", "could not save the calibration"));
        }
        return Ok(());
    }
    // прошлую рабочую калибровку неудачной не затираем
    if let Some(old) = load_calib() {
        if sup.set_calib(old) {
            let msg = lf!("Наводка не найдена (шипение играло в «{}»). Оставлена прошлая калибровка.", "No bleed detected (noise played on \"{}\"). The previous calibration is kept.", name);
            println!("{msg}");
            ui::balloon(l("Калибровка не удалась", "Calibration failed"), &msg);
            return Ok(());
        }
    }
    let msg = lf!(
        "Наводка не найдена (шипение играло в «{}»). Если собеседники слышат себя, поставь обычную громкость и перекалибруй.",
        "No bleed detected (noise played on \"{}\"). If others hear themselves, set your usual volume and recalibrate.",
        name
    );
    println!("{msg}");
    ui::balloon(l("Наводка не найдена", "No bleed detected"), &msg);
    Ok(())
}

fn next_ev(app: &App, rx: &Receiver<Ev>, timeout: Duration) -> Result<Ev, RecvTimeoutError> {
    if let Some(e) = app.backlog.lock().unwrap().pop_front() {
        return Ok(e);
    }
    rx.recv_timeout(timeout)
}

// одна сессия: захват, вывод, обработка; Ok(true) — выход
fn session(app: &Arc<App>, rx: &Receiver<Ev>, sess: u64, first: bool, do_cal: bool, force_cal: bool, recal_next: &mut bool) -> Result<bool, String> {
    G.sessions.fetch_add(1, Relaxed);
    let t0 = now_qpc();
    let mic_s = Arc::new(StreamIn::new(RATE * 2, t0, 0));
    let ref_s = Arc::new(StreamIn::new(RATE * 2, t0, app.delay_ms * RATE as i64 / 1000));
    let refs = if app.monoref { 1 } else { 2 };
    let mut eng = Engine::new(mic_s.clone(), ref_s.clone(), app.chain.clone(), refs);
    eng.bypass = app.bypass;
    eng.open = Some(app.ptt.open.clone());
    *app.eng_stats.lock().unwrap() = Some(eng.stats.clone());
    let eng = Arc::new(Mutex::new(eng));

    let stop = Arc::new(AtomicBool::new(false));
    let (rtx, rrx) = channel::<Ready>();
    let (mic, reff, out) = app.names.lock().unwrap().clone();
    let mic_how = if app.mic_excl.load(Relaxed) { How::ExclLow } else { How::LowLat };
    let ref_mode = if app.monoref { Mode::Mono } else { Mode::Lr };
    let mut hs = Vec::new();
    for (label, sub, loopback, mode, stream, how) in [
        (l("микрофон", "microphone"), mic.clone(), false, Mode::Mono, mic_s.clone(), mic_how),
        (l("наушники (loopback)", "headphones (loopback)"), reff.clone(), true, ref_mode, ref_s.clone(), How::Normal),
    ] {
        let a = CapArgs { label: label.into(), sub, loopback, mode, stream, stop: stop.clone(), ready: rtx.clone(), ev: app.tx.clone(), sess, how };
        hs.push(std::thread::spawn(move || run_capture(a)));
    }
    let ra = RenderArgs {
        sub: out.clone(),
        force_shared: app.shared.load(Relaxed),
        eng,
        t0,
        stop: stop.clone(),
        ready: rtx.clone(),
        ev: app.tx.clone(),
        sess,
        mic: mic_s.clone(),
        reff: ref_s.clone(),
        quiet: app.quiet,
    };
    hs.push(std::thread::spawn(move || run_render(ra)));
    drop(rtx);
    let stop_all = |hs: Vec<std::thread::JoinHandle<()>>| {
        stop.store(true, Relaxed);
        for h in hs {
            let _ = h.join();
        }
    };

    if first {
        println!("{}", l("Устройства:", "Devices:"));
    }
    let mut start_err = None;
    for _ in 0..3 {
        match rrx.recv_timeout(Duration::from_secs(20)) {
            Ok(r) => match r.err {
                Some(e) => start_err = Some(e),
                None if first => println!("  • {}: {} [{}]", r.label, r.name, r.f),
                None => {}
            },
            Err(_) => start_err = Some(l("устройство не ответило", "a device did not respond").into()),
        }
    }
    if let Some(e) = start_err {
        stop_all(hs);
        return Err(e);
    }
    G.running.store(true, Relaxed);
    if first {
        println!("{}", l("В нужной программе выбери микрофоном «CABLE Output».", "Pick \"CABLE Output\" as the microphone in your program."));
        if app.bypass {
            println!("BYPASS");
        }
        println!();
    }
    if do_cal && !app.bypass {
        if let Err(e) = calibrate(app, rx, sess, force_cal) {
            G.running.store(false, Relaxed);
            stop_all(hs);
            if first {
                fatal(&e);
            }
            ui::balloon(APP, &format!("{}{}", l("Калибровка не удалась: ", "Calibration failed: "), first_line(&e)));
            return Ok(false);
        }
    }

    // сон, пропавший микрофон, смена устройства по умолчанию, сохранение модели
    let done = Arc::new(AtomicBool::new(false));
    let mon = {
        let (done, tx, sup, mic_s) = (done.clone(), app.tx.clone(), app.sup.clone(), mic_s.clone());
        let ref_now = reff.clone();
        std::thread::spawn(move || {
            let _com = Com::init();
            let def_name = if ref_now.is_empty() { pick_device(eRender, "", "headphones", false).map(|(_, n)| n).ok() } else { None };
            let send = |m: String| {
                let _ = tx.send(Ev::Err { sess, msg: m, drops: false });
            };
            let mut ticks = 0u64;
            let mut last_end = None;
            let mut stall = 0;
            let mut last_wall = std::time::SystemTime::now();
            'outer: loop {
                for _ in 0..20 {
                    std::thread::sleep(Duration::from_millis(100));
                    if done.load(Relaxed) {
                        break 'outer;
                    }
                }
                ticks += 1;
                let now = std::time::SystemTime::now();
                let gap = now.duration_since(last_wall).unwrap_or_default();
                if gap > Duration::from_secs(8) {
                    logf!("компьютер вышел из сна (пауза {:.0} с) — переподключаю устройства", gap.as_secs_f64());
                    send(l("компьютер вышел из сна", "the PC woke from sleep").into());
                    return;
                }
                last_wall = now;
                if let Some(e) = mic_s.end() {
                    if Some(e) == last_end {
                        stall += 1;
                    } else {
                        stall = 0;
                        last_end = Some(e);
                    }
                    if stall >= 5 {
                        send(l("микрофон перестал присылать звук", "the mic stopped sending audio").into());
                        return;
                    }
                }
                if let Some(dn) = &def_name {
                    if let Ok((_, n)) = pick_device(eRender, "", "headphones", false) {
                        if &n != dn && !n.to_lowercase().contains("cable") {
                            send(lf!("устройство вывода по умолчанию сменилось: «{}» → «{}»", "default output changed: \"{}\" → \"{}\"", dn, n));
                            return;
                        }
                    }
                }
                if ticks % 45 == 0 && sup.take_adapted() {
                    let c = sup.calib();
                    if c.found {
                        let _ = save_calib(&c);
                    }
                }
            }
        })
    };

    let mut quit = false;
    loop {
        let ev = match next_ev(app, rx, Duration::from_millis(500)) {
            Ok(ev) => ev,
            Err(RecvTimeoutError::Timeout) => continue,
            Err(_) => {
                quit = true;
                break;
            }
        };
        match ev {
            Ev::Quit => {
                quit = true;
                break;
            }
            Ev::Err { sess: s, .. } if s != sess => {}
            Ev::Err { msg, drops, .. } => {
                println!("\n[{}] {msg}", timestamp());
                logf!("перезапуск цепочки: {msg}");
                if drops {
                    ui::balloon(APP, first_line(&msg));
                } else {
                    ui::balloon(APP, l("Устройство отключилось, переподключаюсь", "A device was lost, reconnecting"));
                }
                break;
            }
            Ev::Act(a) => {
                if app.handle(&a, &mut quit, recal_next) {
                    break;
                }
            }
        }
    }
    done.store(true, Relaxed);
    let _ = mon.join();
    G.running.store(false, Relaxed);
    stop_all(hs);
    Ok(quit)
}

// виртуальный кабель установлен?
fn wait_for_cable(app: &App, rx: &Receiver<Ev>, out: &str) -> bool {
    let has = || {
        let names = device_names(eRender);
        names.is_empty() || names.iter().any(|n| contains_fold(n, out))
    };
    if has() {
        return true;
    }
    *G.ui_note.lock().unwrap() = l("жду VB-Cable", "waiting for VB-Cable").into();
    println!("{}", lf!("Не найдено устройство «{}» — виртуальный кабель не установлен.", "Device \"{}\" not found: the virtual cable is not installed.", out));
    println!("{}{VB_CABLE_URL}", l("Нужен бесплатный драйвер VB-Cable: ", "The free VB-Cable driver is required: "));
    if out.eq_ignore_ascii_case("CABLE Input") {
        let msg = l(
            "Не найден виртуальный кабель VB-Cable.\n\nЧерез него другие программы получают очищенный микрофон. Это бесплатный драйвер: запусти VBCABLE_Setup_x64.exe от имени администратора и нажми Install Driver. Иногда после установки нужна перезагрузка.\n\nОткрыть страницу загрузки?",
            "VB-Cable virtual cable not found.\n\nOther programs receive the cleaned mic through it. It is a free driver: run VBCABLE_Setup_x64.exe as administrator and click Install Driver. A reboot may be required.\n\nOpen the download page?",
        );
        if message_box(msg, 0x4 | 0x40) == 6 {
            open_url(VB_CABLE_URL);
        }
    }
    let ok = loop {
        match next_ev(app, rx, Duration::from_secs(3)) {
            Ok(Ev::Quit) => break false,
            Ok(Ev::Act(a)) if a.kind == "quit" => break false,
            Ok(_) | Err(RecvTimeoutError::Timeout) => {}
            Err(_) => break false,
        }
        if has() {
            println!("{}", l("Кабель найден, запускаюсь.", "Cable found, starting."));
            break true;
        }
    };
    G.ui_note.lock().unwrap().clear();
    ok
}

fn start_idle_sleep(sup: Arc<SupShared>) {
    use windows::Win32::System::Power::SetSuspendState;
    use windows::Win32::System::SystemInformation::GetTickCount;
    use windows::Win32::UI::Input::KeyboardAndMouse::{GetLastInputInfo, LASTINPUTINFO};
    let input_idle = || unsafe {
        let mut li = LASTINPUTINFO { cbSize: std::mem::size_of::<LASTINPUTINFO>() as u32, dwTime: 0 };
        if !GetLastInputInfo(&mut li).as_bool() {
            return Duration::ZERO;
        }
        Duration::from_millis(GetTickCount().wrapping_sub(li.dwTime) as u64)
    };
    std::thread::spawn(move || {
        let mut last = Instant::now();
        let mut prev = std::time::SystemTime::now();
        loop {
            std::thread::sleep(Duration::from_secs(5));
            let after = Duration::from_secs(idle_sleep_min() as u64 * 60);
            let now = std::time::SystemTime::now();
            if now.duration_since(prev).unwrap_or_default() > Duration::from_secs(30) {
                last = Instant::now();
            }
            prev = now;
            if !G.running.load(Relaxed) {
                last = Instant::now();
                continue;
            }
            let st = sup.stats();
            if st.ref_db > -70.0 || (st.voice && st.mic_db > -60.0) {
                last = Instant::now();
            }
            if !G.idle_sleep_on.load(Relaxed) || last.elapsed() < after || input_idle() < after {
                continue;
            }
            logf!("тишина {:.0} мин, мышь и клавиатура не тронуты — отправляю ПК в сон", last.elapsed().as_secs_f64() / 60.0);
            last = Instant::now();
            unsafe {
                let _ = SetSuspendState(false, false, false);
            }
        }
    });
}

// микрофон и все каналы наушников в hush-rec.zip
fn record_mode(secs: i64, mic: &str, reff: &str) {
    let t0 = now_qpc();
    let size = RATE * (secs as usize + 8);
    let mic_s = Arc::new(StreamIn::new(size, t0, 0));
    let ref_s = Arc::new(StreamIn::new(size, t0, 0));
    let stop = Arc::new(AtomicBool::new(false));
    let (rtx, rrx) = channel::<Ready>();
    let (etx, erx) = channel::<Ev>();
    let mut hs = Vec::new();
    for (label, sub, loopback, mode, stream) in [
        (l("микрофон", "microphone"), mic, false, Mode::Mono, mic_s.clone()),
        (l("наушники (loopback)", "headphones (loopback)"), reff, true, Mode::Raw, ref_s.clone()),
    ] {
        let a = CapArgs { label: label.into(), sub: sub.into(), loopback, mode, stream, stop: stop.clone(), ready: rtx.clone(), ev: etx.clone(), sess: 0, how: How::Normal };
        hs.push(std::thread::spawn(move || run_capture(a)));
    }
    let stop_all = |hs: Vec<std::thread::JoinHandle<()>>| {
        stop.store(true, Relaxed);
        for h in hs {
            let _ = h.join();
        }
    };
    let mut info = format!("hush {VERSION}, {secs} s\n");
    for _ in 0..2 {
        let r = rrx.recv().unwrap_or(Ready { label: String::new(), name: String::new(), f: Default::default(), err: Some("no device".into()) });
        if let Some(e) = r.err {
            stop_all(hs);
            fatal(&e);
        }
        println!("  • {}: {} [{}]", r.label, r.name, r.f);
        info += &format!("{}: {} [{}]\n", r.label, r.name, r.f);
    }
    let half = secs / 2;
    println!();
    println!("{}", lf!("СЕЙЧАС (первые {} с): включи музыку/видео ГРОМКО, как обычно играешь, и МОЛЧИ.", "NOW (first {} s): play music or a video at your usual volume and STAY QUIET.", half));
    let start = Instant::now();
    let mut phase2 = false;
    while start.elapsed() < Duration::from_secs(secs as u64) {
        if let Ok(Ev::Err { msg, .. }) = erx.recv_timeout(Duration::from_millis(200)) {
            stop_all(hs);
            fatal(&msg);
        }
        let el = start.elapsed().as_secs() as i64;
        if !phase2 && el >= half {
            phase2 = true;
            println!(
                "{}",
                lf!(
                    "\nТЕПЕРЬ (ещё {} с): музыка продолжает играть, а ты ГОВОРИ в микрофон — читай что-нибудь вслух.",
                    "\nNOW (another {} s): keep the music playing and TALK into the mic, read something aloud.",
                    secs - half
                )
            );
        }
        print!("\r  {el:2} / {secs} s   ");
        let _ = std::io::stdout().flush();
    }
    stop_all(hs);
    println!("{}", l("\nГотово, сохраняю...", "\nDone, saving..."));
    let (chans, notes) = hush::pipe::extract(&mic_s, &ref_s, idx_from_qpc(now_qpc(), t0)).unwrap_or_else(|e| fatal(&e));
    for n in notes {
        info += &n;
        info.push('\n');
    }
    let path = data_file("rec.zip");
    let r = (|| -> std::io::Result<()> {
        let f = fs::File::create(&path)?;
        let mut z = zip::ZipWriter::new(f);
        let opt = zip::write::SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);
        z.start_file("rec.wav", opt)?;
        hush::audio::write_wav24(&mut z, RATE, &chans)?;
        z.start_file("info.txt", opt)?;
        z.write_all(info.as_bytes())?;
        z.finish()?;
        Ok(())
    })();
    if let Err(e) = r {
        fatal(&format!("{}: {e}", path.display()));
    }
    let size = fs::metadata(&path).map(|m| m.len()).unwrap_or(0);
    println!("\n{info}");
    println!(
        "{}",
        lf!(
            "\nФайл: {} ({:.1} МБ)\nПриложи его к issue на GitHub. Нажми Enter, чтобы закрыть окно...",
            "\nFile: {} ({:.1} MB)\nAttach it to a GitHub issue. Press Enter to close...",
            path.display(),
            size as f64 / 1e6
        )
    );
    wait_enter();
}

pub fn main() {
    hush::lang::init(system_russian());
    let fl = parse_flags();
    let exe = exe_path().file_name().map(|s| s.to_string_lossy().to_lowercase()).unwrap_or_default();
    let no_args = std::env::args().len() == 1;
    let record = if fl.record == 0 && no_args && exe.contains("record") { 24 } else { fl.record };
    let doctor = fl.doctor || (no_args && exe.contains("doctor"));

    if fl.version || fl.help {
        use_console(true);
        if fl.help {
            usage();
        } else {
            println!("hush {VERSION}");
        }
        return;
    }
    if fl.list || doctor || record > 0 || fl.noui || fl.console {
        use_console(true);
    }
    if !fl.list && !doctor && record == 0 && std::env::var("HUSH_UIDEMO").is_err() && !single_instance(!fl.background, fl.after_update) {
        return;
    }
    process_for_audio();
    let crash = data_file("crash.txt");
    rotate(&crash, 256 * 1024);
    std::panic::set_hook(Box::new(move |p| {
        if let Ok(mut f) = fs::OpenOptions::new().create(true).append(true).open(data_file("crash.txt")) {
            let _ = write!(f, "\r\nPANIC {}: {p}\r\n{}\r\n", timestamp(), std::backtrace::Backtrace::force_capture());
        }
    }));
    std::thread::spawn(update::cleanup_old_exe);
    let _com = Com::init();

    if fl.list {
        for (title, flow) in [(l("Микрофоны", "Microphones"), eCapture), (l("Устройства вывода", "Output devices"), eRender)] {
            println!("{title}:");
            for n in device_names(flow) {
                println!("  • {n}");
            }
        }
        return;
    }
    if doctor {
        super::doctor::doctor_mode();
        return;
    }
    logf!("hush {VERSION} запущен");
    println!("hush {VERSION}");
    if record > 0 {
        if !(8..=60).contains(&record) {
            fatal(l("-record: от 8 до 60 секунд", "-record: 8 to 60 seconds"));
        }
        record_mode(record, &fl.mic, &fl.reff);
        return;
    }

    let mut cfg = load_config();
    if migrate_autostart() {
        cfg.auto_set = true;
    }
    if !cfg.auto_set {
        if set_autostart(true).is_ok() {
            cfg.auto_set = true;
        }
    } else if autostart_on() {
        let _ = set_autostart(true);
    }
    if let Some(g) = fl.gain {
        cfg.gain_db = g;
    }
    let mic = if !fl.mic_set && !cfg.mic.is_empty() { cfg.mic.clone() } else { fl.mic.clone() };
    let reff = if !fl.ref_set && !cfg.reff.is_empty() { cfg.reff.clone() } else { fl.reff.clone() };
    let out = if !fl.out_set && !cfg.out.is_empty() { cfg.out.clone() } else { fl.out.clone() };
    let beta = fl.beta.unwrap_or(if cfg.beta != 0.0 { cfg.beta } else { 1.5 });
    let shared = fl.shared.unwrap_or(cfg.shared);
    let mic_excl = fl.micexcl.unwrap_or(cfg.mic_ex);
    *CFG.lock().unwrap() = Some(cfg.clone());
    update_config(|_| {});

    let sup = Suppressor::new(RATE);
    let sup_sh = sup.shared();
    sup_sh.set_beta(beta);
    let dn = Denoiser::new();
    dn.ctl().set_strength(cfg.noise.unwrap_or(1));
    let eq = Eq::new();
    eq.ctl().set_preset(cfg.eq as i32);
    let agc = Agc::new();
    agc.ctl().set_enabled(cfg.agc);
    let amp = Amp::new(cfg.gain_db.clamp(-10.0, MAX_GAIN_DB));
    let (dn_c, eq_c, agc_c, amp_c) = (dn.ctl(), eq.ctl(), agc.ctl(), amp.ctl());
    let chain = Arc::new(Mutex::new(Chain { sup, dn, eq, agc, amp }));

    let ptt = Ptt::start(cfg.ptt as i32, cfg.ptt_key as i32);
    ptt.on_bind(|vk| {
        update_config(|c| c.ptt_key = vk as i64);
        ui::balloon(APP, &format!("{}{}", l("Кнопка микрофона: ", "Mic key: "), key_name(vk)));
    });
    let names = Arc::new(Mutex::new(String::new()));
    *names.lock().unwrap() = reff.clone();
    let (tx, rx) = channel::<Ev>();
    *TX.lock().unwrap() = Some(tx.clone());
    unsafe {
        let _ = SetConsoleCtrlHandler(Some(ctrl_handler), true);
    }

    let app_cell: Arc<Mutex<Option<Arc<App>>>> = Arc::new(Mutex::new(None));
    let cue_ref = app_cell.clone();
    let cues = Cues::start(move || cue_ref.lock().unwrap().as_ref().map(|a| a.names.lock().unwrap().1.clone()).unwrap_or_default());
    cues.enabled.store(!cfg.no_cue, Relaxed);
    {
        let c = cues.clone();
        ptt.on_change(move |open| {
            log_event(if open { "микрофон по кнопке: ВКЛ" } else { "микрофон по кнопке: ВЫКЛ" });
            c.play(open);
        });
    }
    let app = Arc::new(App {
        chain,
        sup: sup_sh.clone(),
        dn: dn_c,
        eq: eq_c,
        agc: agc_c,
        amp: amp_c,
        names: Mutex::new((mic, reff, out.clone())),
        shared: AtomicBool::new(shared),
        mic_excl: AtomicBool::new(mic_excl),
        ptt,
        cues,
        eng_stats: Mutex::new(None),
        bypass: fl.bypass,
        monoref: fl.monoref,
        delay_ms: fl.delay_ms,
        quiet: fl.quiet,
        tx: tx.clone(),
        backlog: Mutex::new(VecDeque::new()),
    });
    *app_cell.lock().unwrap() = Some(app.clone());

    G.idle_sleep_on.store(!cfg.no_idle_sleep, Relaxed);
    start_idle_sleep(sup_sh);

    if std::env::var("HUSH_UIDEMO").is_ok() {
        let t = Mutex::new(0.0f64);
        ui::start(
            Box::new(move || {
                let mut t = t.lock().unwrap();
                *t += 0.12;
                let t = *t;
                UiState {
                    running: true, found: true, voice: t.sin() > 0.0, mic_db: -50.0 + 25.0 * t.sin(), ref_db: -30.0 + 20.0 * (t * 0.7).sin(),
                    out_db: -20.0 + 12.0 * t.sin(), reduction: 9.4, gain_db: 6.0, beta: 1.5, noise: 2, eq: 1, agc: true, agc_gain: 7.0, ptt: 1,
                    ptt_key: key_name(5), ptt_open: (t * 0.3).sin() > 0.0, cue: true, latency_ms: 28, excl_want: true, excl_on: true,
                    mic: "Headset".into(), out: "CABLE Input".into(), ..Default::default()
                }
            }),
            tx.clone(),
            true,
        );
        while let Ok(ev) = rx.recv() {
            match ev {
                Ev::Act(a) if a.kind == "quit" => break,
                Ev::Quit => break,
                Ev::Act(a) => println!("action: {a:?}"),
                _ => {}
            }
        }
        ui::close();
        return;
    }

    if !fl.noui {
        let a = app.clone();
        ui::start(Box::new(move || a.state()), tx.clone(), !fl.background);
        std::thread::spawn(|| {
            std::thread::sleep(Duration::from_secs(20));
            loop {
                if let Ok((tag, true)) = update::check() {
                    logf!("доступна версия {tag}");
                    ui::balloon(APP, &lf!("Доступна версия {}. Обновить можно на панели или в меню.", "Version {} is available. Update from the panel or the menu.", tag));
                }
                std::thread::sleep(Duration::from_secs(3600));
            }
        });
    }

    if !wait_for_cable(&app, &rx, &out) {
        ui::close();
        return;
    }

    let mut first = true;
    let mut recal_next = false;
    let mut sess = 0u64;
    loop {
        let (mut do_cal, mut force) = (first, first && fl.recal);
        if recal_next {
            do_cal = true;
            force = true;
            recal_next = false;
        }
        sess += 1;
        let quit = match session(&app, &rx, sess, first, do_cal, force, &mut recal_next) {
            Err(e) => {
                if first {
                    fatal(&e);
                }
                println!("[{}] {}: {}", timestamp(), l("жду устройства", "waiting for devices"), first_line(&e));
                matches!(next_ev(&app, &rx, Duration::from_secs(3)), Ok(Ev::Quit) | Ok(Ev::Act(Action { kind: "quit", .. })))
            }
            Ok(true) => true,
            Ok(false) => {
                println!("{}", l("Переподключаюсь...", "Reconnecting..."));
                let mut q = false;
                let until = Instant::now() + Duration::from_millis(1500);
                while Instant::now() < until {
                    match next_ev(&app, &rx, Duration::from_millis(100)) {
                        Ok(Ev::Quit) | Ok(Ev::Act(Action { kind: "quit", .. })) => q = true,
                        Ok(Ev::Act(a)) => {
                            let mut qq = false;
                            if app.handle(&a, &mut qq, &mut recal_next) && qq {
                                q = true;
                            }
                        }
                        _ => {}
                    }
                }
                q
            }
        };
        first = false;
        if quit {
            break;
        }
    }
    ui::close();
    std::thread::sleep(Duration::from_millis(200));
    println!("\n{}", l("Остановлено.", "Stopped."));
}
