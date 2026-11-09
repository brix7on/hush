use std::f64::consts::PI;
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering::Relaxed};
use std::sync::mpsc::{sync_channel, SyncSender, TrySendError};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use windows::Win32::Media::Audio::eRender;
use windows::Win32::UI::Input::KeyboardAndMouse::{GetAsyncKeyState, GetKeyNameTextW, MapVirtualKeyW, MAPVK_VK_TO_VSC};

use hush::l;

use super::dev::pick_device;
use super::render::play_samples;
use super::sys::Com;

pub const PTT_OFF: i32 = 0;
pub const PTT_HOLD: i32 = 1;
pub const PTT_TOGGLE: i32 = 2;
const PTT_TAIL: Duration = Duration::from_millis(250);

type Cb = Box<dyn Fn(i32) + Send + Sync>;
type CbB = Box<dyn Fn(bool) + Send + Sync>;

pub struct Ptt {
    pub mode: AtomicI32,
    pub key: AtomicI32,
    pub open: Arc<AtomicBool>,
    pub binding: AtomicBool,
    on_bind: Mutex<Option<Cb>>,
    on_change: Mutex<Option<CbB>>,
}

fn key_down(vk: i32) -> bool {
    unsafe { GetAsyncKeyState(vk) as u16 & 0x8000 != 0 }
}

impl Ptt {
    pub fn start(mode: i32, key: i32) -> Arc<Ptt> {
        let p = Arc::new(Ptt {
            mode: AtomicI32::new(mode),
            key: AtomicI32::new(key),
            open: Arc::new(AtomicBool::new(!(mode == PTT_HOLD && key != 0))),
            binding: AtomicBool::new(false),
            on_bind: Mutex::new(None),
            on_change: Mutex::new(None),
        });
        let q = p.clone();
        std::thread::spawn(move || q.run());
        p
    }

    pub fn on_bind(&self, f: impl Fn(i32) + Send + Sync + 'static) {
        *self.on_bind.lock().unwrap() = Some(Box::new(f));
    }

    pub fn on_change(&self, f: impl Fn(bool) + Send + Sync + 'static) {
        *self.on_change.lock().unwrap() = Some(Box::new(f));
    }

    fn set_open(&self, v: bool, notify: bool) {
        if self.open.swap(v, Relaxed) != v && notify {
            if let Some(f) = self.on_change.lock().unwrap().as_ref() {
                f(v);
            }
        }
    }

    fn run(&self) {
        let mut prev = [false; 256];
        let mut last_down = Instant::now() - Duration::from_secs(10);
        let mut toggled = true;
        let mut last_mode = -1;
        let mut was_binding = false;
        loop {
            std::thread::sleep(Duration::from_millis(10));
            let mode = self.mode.load(Relaxed);
            if mode != last_mode {
                toggled = true;
                last_mode = mode;
            }
            if self.binding.load(Relaxed) {
                if !was_binding {
                    for (vk, p) in prev.iter_mut().enumerate() {
                        *p = key_down(vk as i32);
                    }
                    was_binding = true;
                    continue;
                }
                // 1 и 2 — левая и правая кнопки мыши; общие Shift/Ctrl/Alt не берём
                for vk in 3..255 {
                    if vk == 0x10 || vk == 0x11 || vk == 0x12 {
                        continue;
                    }
                    let d = key_down(vk);
                    if d && !prev[vk as usize] {
                        self.key.store(vk, Relaxed);
                        self.binding.store(false, Relaxed);
                        if let Some(f) = self.on_bind.lock().unwrap().as_ref() {
                            f(vk);
                        }
                        break;
                    }
                    prev[vk as usize] = d;
                }
                continue;
            }
            if was_binding {
                was_binding = false;
                let vk = self.key.load(Relaxed);
                if vk > 0 && vk < 256 {
                    prev[vk as usize] = key_down(vk);
                }
            }
            let vk = self.key.load(Relaxed);
            if mode == PTT_OFF || vk <= 0 || vk >= 256 {
                self.set_open(true, false);
                continue;
            }
            let d = key_down(vk);
            match mode {
                PTT_HOLD => {
                    if d {
                        last_down = Instant::now();
                        self.set_open(true, true);
                    } else if last_down.elapsed() > PTT_TAIL {
                        self.set_open(false, true);
                    }
                }
                PTT_TOGGLE => {
                    if d && !prev[vk as usize] {
                        toggled = !toggled;
                    }
                    self.set_open(toggled, true);
                }
                _ => {}
            }
            prev[vk as usize] = d;
        }
    }
}

// название клавиши как в играх
pub fn key_name(vk: i32) -> String {
    match vk {
        0 => return l("не назначена", "not set").into(),
        4 => return "Mouse 3".into(),
        5 => return "Mouse 4".into(),
        6 => return "Mouse 5".into(),
        0xA0 => return "LShift".into(),
        0xA1 => return "RShift".into(),
        0xA2 => return "LCtrl".into(),
        0xA3 => return "RCtrl".into(),
        0xA4 => return "LAlt".into(),
        0xA5 => return "RAlt".into(),
        0x14 => return "Caps Lock".into(),
        0x70..=0x87 => return format!("F{}", vk - 0x6F),
        _ => {}
    }
    let sc = unsafe { MapVirtualKeyW(vk as u32, MAPVK_VK_TO_VSC) };
    if sc != 0 {
        let mut lp = (sc << 16) as i32;
        if matches!(vk, 0x21..=0x28 | 0x2D | 0x2E | 0x6F | 0x90) {
            lp |= 1 << 24;
        }
        let mut buf = [0u16; 64];
        let n = unsafe { GetKeyNameTextW(lp, &mut buf) };
        if n > 0 {
            return String::from_utf16_lossy(&buf[..n as usize]);
        }
    }
    format!("Key {vk}")
}

// включение — «бульк» вверх, выключение — «бульк» вниз
pub fn cue_samples(rate: usize, open: bool) -> Vec<f32> {
    let (f0, f1, ms, bright) = if open { (400.0, 900.0, 130.0, 0.25) } else { (820.0, 380.0, 150.0, 0.2) };
    let r = rate as f64;
    let mut out = vec![0f32; ((ms + 25.0) * r / 1000.0) as usize];
    let n = (ms * r / 1000.0) as usize;
    let mut ph = 0.0f64;
    for i in 0..n.min(out.len()) {
        let t = i as f64 / n as f64;
        let f = f0 * (f1 / f0 as f64).powf(t);
        ph += 2.0 * PI * f / r;
        let sec = i as f64 / r;
        let mut env = (sec / 0.003).min(1.0) * (-sec / (ms / 1000.0 * 0.45)).exp();
        let tail = (n - i) as f64 / (0.004 * r);
        if tail < 1.0 {
            env *= tail;
        }
        let v = ph.sin() + bright * (0.6 * (2.0 * ph).sin() + 0.3 * (3.0 * ph).sin());
        out[i] += (0.22 * env * v) as f32;
    }
    out
}

// сигналы в наушники в своём потоке, лишние пропускает
pub struct Cues {
    tx: SyncSender<bool>,
    pub enabled: AtomicBool,
}

impl Cues {
    pub fn start(device: impl Fn() -> String + Send + 'static) -> Arc<Cues> {
        let (tx, rx) = sync_channel::<bool>(1);
        std::thread::spawn(move || {
            let _com = Com::init();
            while let Ok(open) = rx.recv() {
                if let Ok((dev, _)) = pick_device(eRender, &device(), "headphones", false) {
                    let _ = play_samples(&dev, |r| cue_samples(r, open));
                }
            }
        });
        Arc::new(Cues { tx, enabled: AtomicBool::new(true) })
    }

    pub fn play(&self, open: bool) {
        if !self.enabled.load(Relaxed) {
            return;
        }
        if let Err(TrySendError::Full(_)) = self.tx.try_send(open) {
            std::thread::sleep(Duration::from_millis(1));
            let _ = self.tx.try_send(open);
        }
    }
}
