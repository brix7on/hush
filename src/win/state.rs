use std::sync::atomic::{AtomicBool, AtomicI64, AtomicIsize, AtomicU64, Ordering::Relaxed};
use std::sync::{LazyLock, Mutex};
use std::time::Duration;

// счётчики и флаги, которые читает панель и журнал
pub struct Globals {
    pub running: AtomicBool,
    pub out_buf_ms: AtomicI64,
    pub out_lat_ms: AtomicI64,
    pub out_starves: AtomicI64,
    pub excl_active: AtomicBool,

    pub mic_period_us: AtomicI64,
    pub mic_excl_active: AtomicBool,
    pub mic_excl_why: Mutex<String>,
    pub mic_excl_shown: Mutex<String>,
    pub mic_excl_min_p: AtomicI64,
    pub mic_glitches: AtomicI64,
    pub mic_own_clock: AtomicBool,
    pub mic_loss_pct: AtomicI64,

    pub mic_stalls: AtomicI64,
    pub mic_stall_max: AtomicI64,
    pub ref_stalls: AtomicI64,
    pub ref_stall_max: AtomicI64,
    pub out_stalls: AtomicI64,
    pub out_stall_max: AtomicI64,

    pub ui_note: Mutex<String>,
    pub ui_hwnd: AtomicIsize,
    pub idle_sleep_on: AtomicBool,
    pub sessions: AtomicU64,
}

pub static G: LazyLock<Globals> = LazyLock::new(|| Globals {
    running: AtomicBool::new(false),
    out_buf_ms: AtomicI64::new(0),
    out_lat_ms: AtomicI64::new(0),
    out_starves: AtomicI64::new(0),
    excl_active: AtomicBool::new(false),
    mic_period_us: AtomicI64::new(0),
    mic_excl_active: AtomicBool::new(false),
    mic_excl_why: Mutex::new(String::new()),
    mic_excl_shown: Mutex::new(String::new()),
    mic_excl_min_p: AtomicI64::new(0),
    mic_glitches: AtomicI64::new(0),
    mic_own_clock: AtomicBool::new(false),
    mic_loss_pct: AtomicI64::new(0),
    mic_stalls: AtomicI64::new(0),
    mic_stall_max: AtomicI64::new(0),
    ref_stalls: AtomicI64::new(0),
    ref_stall_max: AtomicI64::new(0),
    out_stalls: AtomicI64::new(0),
    out_stall_max: AtomicI64::new(0),
    ui_note: Mutex::new(String::new()),
    ui_hwnd: AtomicIsize::new(0),
    idle_sleep_on: AtomicBool::new(true),
    sessions: AtomicU64::new(0),
});

// поток не получал процессор дольше 30 мс
pub fn note_stall(n: &AtomicI64, mx: &AtomicI64, gap: Duration) {
    if gap <= Duration::from_millis(30) {
        return;
    }
    n.fetch_add(1, Relaxed);
    let ms = gap.as_millis() as i64;
    if ms > mx.load(Relaxed) {
        mx.store(ms, Relaxed);
    }
}

// события для главного цикла
pub enum Ev {
    Err { sess: u64, msg: String, drops: bool },
    Act(hush::panel::Action),
    Quit,
}
