use std::sync::atomic::{AtomicBool, Ordering::Relaxed};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use windows::Win32::Media::Audio::*;

use hush::diag;
use hush::{l, lf};

use super::dev::*;
use super::render::play_samples;
use super::sys::{fatal, wait_enter, Com};

struct Endpoint {
    dev: IMMDevice,
    name: String,
    def_cons: bool,
    def_comm: bool,
    sess: Vec<Sess>,
}

impl Endpoint {
    fn has(&self, sub: &str) -> bool {
        self.sess.iter().any(|s| s.exe.to_lowercase().contains(sub))
    }
    fn apps(&self) -> String {
        let mut v: Vec<&str> = Vec::new();
        for s in &self.sess {
            if !v.contains(&s.exe.as_str()) {
                v.push(&s.exe);
            }
        }
        v.join(", ")
    }
}

fn endpoints(flow: EDataFlow) -> Vec<Endpoint> {
    let Ok(list) = list_devices(flow) else { return Vec::new() };
    let def_id = |role: ERole| -> String {
        enumerator().ok().and_then(|e| unsafe { e.GetDefaultAudioEndpoint(flow, role) }.ok()).map(|d| device_id(&d)).unwrap_or_default()
    };
    let (dc, dm) = (def_id(eConsole), def_id(eCommunications));
    list.into_iter()
        .map(|(dev, name)| {
            let id = device_id(&dev);
            let sess = device_sessions(&dev);
            Endpoint { def_cons: id == dc, def_comm: id == dm, sess, dev, name }
        })
        .collect()
}

#[derive(Default)]
struct CapRes {
    data: Vec<f32>,
    rate: usize,
    start: Option<Instant>,
    err: Option<String>,
}

fn capture_mono(id: String, stop: Arc<AtomicBool>, res: Arc<Mutex<CapRes>>, started: std::sync::mpsc::Sender<()>) {
    let r = (|| -> Result<(), String> {
        let dev = unsafe { enumerator().and_then(|e| e.GetDevice(&windows::core::HSTRING::from(id.as_str()))) }.map_err(|e| e.to_string())?;
        let (ac, mf, f) = open_client(&dev)?;
        unsafe { ac.Initialize(AUDCLNT_SHAREMODE_SHARED, 0, 1_000_000, 0, mf.0, None) }.map_err(|e| format!("Initialize: {e}"))?;
        let acc: IAudioCaptureClient = unsafe { ac.GetService() }.map_err(|e| e.to_string())?;
        unsafe { ac.Start() }.map_err(|e| e.to_string())?;
        {
            let mut r = res.lock().unwrap();
            r.rate = f.rate;
            r.start = Some(Instant::now());
        }
        let _ = started.send(());
        while !stop.load(Relaxed) {
            std::thread::sleep(Duration::from_millis(3));
            loop {
                let n = unsafe { acc.GetNextPacketSize() }.unwrap_or(0);
                if n == 0 {
                    break;
                }
                let mut data: *mut u8 = std::ptr::null_mut();
                let (mut frames, mut fl) = (0u32, 0u32);
                if unsafe { acc.GetBuffer(&mut data, &mut frames, &mut fl, None, None) }.is_err() {
                    break;
                }
                let mut r = res.lock().unwrap();
                if fl & AUDCLNT_BUFFERFLAGS_SILENT.0 as u32 != 0 || data.is_null() {
                    let n = r.data.len() + frames as usize;
                    r.data.resize(n, 0.0);
                } else {
                    let b = unsafe { std::slice::from_raw_parts(data, frames as usize * f.block_align) };
                    f.decode_mono(b, frames as usize, &mut r.data);
                }
                unsafe {
                    let _ = acc.ReleaseBuffer(frames);
                }
            }
        }
        unsafe {
            let _ = ac.Stop();
        }
        Ok(())
    })();
    if let Err(e) = r {
        res.lock().unwrap().err = Some(e);
    }
    let _ = started.send(());
}

fn short(n: &str) -> String {
    format!("«{n}»")
}

// откуда берётся эхо
pub fn doctor_mode() {
    let _com = Com::init();
    println!("{}", l("Диагностика эха. Зайди в голосовой канал Дискорда (можно одному) и не закрывай его.", "Echo diagnostics. Join a Discord voice channel (alone is fine) and keep it open."));
    println!("{}", l("Через пару секунд в наушниках будет короткое шипение (~5 с) — это нормально, молчи.", "In a few seconds you will hear a short noise (~5 s) in the headphones. Stay quiet."));
    println!();
    std::thread::sleep(Duration::from_millis(1500));

    let renders = endpoints(eRender);
    let captures = endpoints(eCapture);
    if renders.is_empty() || captures.is_empty() {
        fatal(l("не удалось получить список звуковых устройств", "cannot list audio devices"));
    }
    let disc_out: Vec<usize> = (0..renders.len()).filter(|&i| renders[i].has("discord")).collect();
    let disc_in: Vec<usize> = (0..captures.len()).filter(|&i| captures[i].has("discord")).collect();
    let mark = |e: &Endpoint| {
        let mut m = String::new();
        if e.def_cons {
            m += l(" [по умолчанию]", " [default]");
        }
        if e.def_comm {
            m += l(" [связь по умолчанию]", " [default communications]");
        }
        m
    };
    let apps = |e: &Endpoint| {
        let a = e.apps();
        if a.is_empty() {
            "—".to_string()
        } else {
            a
        }
    };
    println!("{}", l("Устройства вывода:", "Output devices:"));
    for e in &renders {
        println!("{}", lf!("  • {}{} — играют: {}", "  • {}{} — playing: {}", e.name, mark(e), apps(e)));
    }
    println!("{}", l("Устройства записи:", "Recording devices:"));
    for e in &captures {
        println!("{}", lf!("  • {}{} — записывают: {}", "  • {}{} — recording: {}", e.name, mark(e), apps(e)));
    }
    println!();

    let target = if let Some(&i) = disc_out.first() {
        i
    } else {
        println!("{}", l("Дискорд не найден среди воспроизводящих программ — шипение пойдёт на устройство по умолчанию.", "Discord is not playing audio, the noise goes to the default device."));
        renders.iter().position(|e| e.def_cons).unwrap_or(0)
    };
    let tname = renders[target].name.clone();
    println!("{}", lf!("Проигрываю шипение в {} и слушаю все входы...", "Playing noise on {} and listening to all inputs...", short(&tname)));

    let stop = Arc::new(AtomicBool::new(false));
    let res: Vec<Arc<Mutex<CapRes>>> = captures.iter().map(|_| Arc::new(Mutex::new(CapRes::default()))).collect();
    let (stx, srx) = std::sync::mpsc::channel();
    let mut hs = Vec::new();
    for (i, c) in captures.iter().enumerate() {
        let (d, s, r, t) = (device_id(&c.dev), stop.clone(), res[i].clone(), stx.clone());
        hs.push(std::thread::spawn(move || {
            let _com = Com::init();
            capture_mono(d, s, r, t)
        }));
    }
    for _ in 0..captures.len() {
        let _ = srx.recv_timeout(Duration::from_secs(5));
    }
    std::thread::sleep(Duration::from_millis(500));
    let played = play_samples(&renders[target].dev, |r| diag::tone(r, 0.1));
    std::thread::sleep(Duration::from_millis(900));
    stop.store(true, Relaxed);
    for h in hs {
        let _ = h.join();
    }
    let tone_start = match played {
        Ok(t) => t,
        Err(e) => fatal(&lf!("не удалось проиграть шипение: {}", "cannot play the noise: {}", e)),
    };

    println!();
    println!("{}", l("Результат:", "Result:"));
    let mut heard = Vec::new();
    for (i, e) in captures.iter().enumerate() {
        let r = res[i].lock().unwrap();
        if let Some(err) = &r.err {
            println!("{}", lf!("  • {}: не удалось открыть ({})", "  • {}: cannot open ({})", e.name, err));
            continue;
        }
        let Some(st) = r.start else { continue };
        let at = tone_start.saturating_duration_since(st).as_millis() as usize;
        let d = diag::detect(&r.data, r.rate, at);
        if d.hears {
            heard.push(i);
            println!("{}", lf!("  • {}: СЛЫШИТ шипение (+{:.0} дБ, задержка {} мс)", "  • {}: HEARS the noise (+{:.0} dB, delay {} ms)", e.name, d.diff_db, d.lag_ms));
        } else {
            println!("{}", lf!("  • {}: не слышит", "  • {}: does not hear it", e.name));
        }
    }
    println!();

    let real_mic = captures.iter().enumerate().find(|(i, e)| {
        let ln = e.name.to_lowercase();
        !heard.contains(i) && res[*i].lock().unwrap().err.is_none() && !ln.contains("cable") && !ln.contains("микшер") && !ln.contains("stereo mix")
    });
    let real_mic = real_mic.map(|(_, e)| e.name.clone()).unwrap_or_default();

    println!("{}", l("════════ ВЫВОД ════════", "════════ VERDICT ════════"));
    if disc_in.is_empty() {
        println!("{}", l("Не вижу, с какого устройства записывает Дискорд — ты точно в голосовом канале?", "Cannot see which device Discord records from. Are you in a voice channel?"));
    } else {
        let names: Vec<String> = disc_in.iter().map(|&i| short(&captures[i].name)).collect();
        println!("{} {}", l("Дискорд записывает с:", "Discord records from:"), names.join(", "));
    }
    let mut found = false;
    for &i in &heard {
        let e = &captures[i];
        if !e.has("discord") {
            continue;
        }
        found = true;
        let ln = e.name.to_lowercase();
        println!(
            "{}",
            lf!(
                "\nПРИЧИНА: Дискорд берёт звук с {}, а туда попадает всё, что играет в {}.",
                "\nCAUSE: Discord records from {}, and everything played on {} ends up there.",
                short(&e.name),
                short(&tname)
            )
        );
        println!("{}", l("Поэтому голос друга возвращается к нему.", "That is why others hear themselves."));
        if ln.contains("cable") {
            let mut writers = Vec::new();
            for r in renders.iter().filter(|r| r.name.to_lowercase().contains("cable")) {
                for s in &r.sess {
                    if !s.exe.to_lowercase().contains(hush::APP) {
                        writers.push(s.exe.clone());
                    }
                }
            }
            if !writers.is_empty() {
                println!("{}", lf!("В виртуальный кабель выводят звук: {}.", "Programs playing into the virtual cable: {}.", writers.join(", ")));
                println!("{}", l("ИСПРАВИТЬ: Параметры Windows → Система → Звук → Микшер громкости → у этих программ", "FIX: Windows Settings → System → Sound → Volume mixer → for these programs"));
                println!("{}", l("           поставь «Устройство вывода» = «По умолчанию».", "     set \"Output device\" to \"Default\"."));
            }
            if !real_mic.is_empty() {
                println!("{}", lf!("ИЛИ проще: в Дискорде выбери «Устройство ввода» = {}.", "OR simpler: in Discord set \"Input device\" to {}.", short(&real_mic)));
            }
        } else if ln.contains("микшер") || ln.contains("stereo mix") || ln.contains("what u hear") {
            println!("{}", l("Это «Стерео микшер» — он записывает всё, что играет на компьютере.", "This is \"Stereo Mix\": it records everything played on the PC."));
            if !real_mic.is_empty() {
                println!("{}", lf!("ИСПРАВИТЬ: в Дискорде выбери «Устройство ввода» = {}.", "FIX: in Discord set \"Input device\" to {}.", short(&real_mic)));
            }
        } else {
            println!("{}", l("Это сам микрофон: Windows или драйвер подмешивают в него звук наушников.", "It is the mic itself: Windows or the driver mixes headphone audio into it."));
            println!("{}", l("ИСПРАВИТЬ: Панель управления → Звук → Запись → этот микрофон → Свойства:", "FIX: Control Panel → Sound → Recording → this mic → Properties:"));
            println!("{}", l("  • вкладка «Прослушать» — сними «Прослушивать с данного устройства»;", "  • Listen tab: uncheck \"Listen to this device\";"));
            println!("{}", l("  • вкладка «Улучшения»/«Дополнительно» — отключи все эффекты;", "  • Enhancements/Advanced tab: disable all effects;"));
            println!("{}", l("  • в программе гарнитуры (если стоит) — выключи «Мониторинг микрофона»/«Sidetone».", "  • in the headset software, if any: turn off \"Mic monitoring\"/\"Sidetone\"."));
        }
    }
    if !found {
        if !heard.is_empty() {
            println!("{}", l("\nШипение слышат другие входы, но не тот, с которого пишет Дискорд.", "\nOther inputs hear the noise, but not the one Discord records from."));
        }
        println!("{}", l("\nWindows эхо не создаёт: звук, который играет Дискорд, не возвращается в его вход.", "\nWindows does not create the echo: what Discord plays does not return to its input."));
        println!("{}", l("Пришли мне скрин этого окна целиком — разберу, что осталось.", "If the echo persists, attach a screenshot of this window to an issue on GitHub."));
    }
    println!("{}", l("\nНажми Enter, чтобы закрыть окно...", "\nPress Enter to close..."));
    wait_enter();
}
