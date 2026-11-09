use std::ffi::OsStr;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::windows::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, OnceLock};
use std::time::Duration;

use windows::core::{HSTRING, PCWSTR};
use windows::Win32::Foundation::{CloseHandle, GENERIC_READ, GENERIC_WRITE, ERROR_ALREADY_EXISTS, GetLastError, HANDLE, HWND, LPARAM, WPARAM};
use windows::Win32::Media::timeBeginPeriod;
use windows::Win32::System::Com::{CoInitializeEx, CoUninitialize, COINIT_MULTITHREADED};
use windows::Win32::System::Console::{AllocConsole, AttachConsole, SetConsoleOutputCP, ATTACH_PARENT_PROCESS};
use windows::Win32::System::Performance::{QueryPerformanceCounter, QueryPerformanceFrequency};
use windows::Win32::System::Registry::*;
use windows::Win32::System::Threading::*;
use windows::Win32::UI::Shell::ShellExecuteW;
use windows::Win32::UI::WindowsAndMessaging::{FindWindowW, MessageBoxW, PostMessageW, MESSAGEBOX_STYLE, SW_SHOWNORMAL};

use hush::{l, APP};

pub fn wide(s: &str) -> Vec<u16> {
    OsStr::new(s).encode_wide().chain(std::iter::once(0)).collect()
}

// ---------- COM ----------

pub struct Com;

impl Com {
    pub fn init() -> Com {
        unsafe {
            let _ = CoInitializeEx(None, COINIT_MULTITHREADED);
        }
        Com
    }
}

impl Drop for Com {
    fn drop(&mut self) {
        unsafe { CoUninitialize() };
    }
}

// ---------- время ----------

static QPC_FREQ: OnceLock<i64> = OnceLock::new();

// текущее время в единицах меток WASAPI (100 нс)
pub fn now_qpc() -> i64 {
    let f = *QPC_FREQ.get_or_init(|| {
        let mut f = 0i64;
        unsafe {
            let _ = QueryPerformanceFrequency(&mut f);
        }
        f.max(1)
    });
    let mut c = 0i64;
    unsafe {
        let _ = QueryPerformanceCounter(&mut c);
    }
    c / f * 10_000_000 + (c % f) * 10_000_000 / f
}

// точная пауза ~1 мс
pub struct Tick(HANDLE);

impl Tick {
    pub fn new() -> Tick {
        let h = unsafe {
            CreateWaitableTimerExW(None, PCWSTR::null(), CREATE_WAITABLE_TIMER_HIGH_RESOLUTION, 0x1F0003)
                .unwrap_or_default()
        };
        Tick(h)
    }

    pub fn wait(&self) {
        if self.0.is_invalid() {
            std::thread::sleep(Duration::from_millis(1));
            return;
        }
        let due = -10_000i64;
        unsafe {
            if SetWaitableTimer(self.0, &due, 0, None, None, false).is_err() {
                std::thread::sleep(Duration::from_millis(1));
                return;
            }
            WaitForSingleObject(self.0, 50);
        }
    }
}

impl Drop for Tick {
    fn drop(&mut self) {
        if !self.0.is_invalid() {
            unsafe {
                let _ = CloseHandle(self.0);
            }
        }
    }
}

// приоритет потока через MMCSS «Pro Audio»
pub struct ProAudio(Option<HANDLE>);

impl ProAudio {
    pub fn enter() -> ProAudio {
        let mut idx = 0u32;
        let h = unsafe { AvSetMmThreadCharacteristicsW(windows::core::w!("Pro Audio"), &mut idx) };
        ProAudio(h.ok())
    }
}

impl Drop for ProAudio {
    fn drop(&mut self) {
        if let Some(h) = self.0 {
            unsafe {
                let _ = AvRevertMmThreadCharacteristics(h);
            }
        }
    }
}

// приоритет процесса чуть выше обычного и без EcoQoS
pub fn process_for_audio() {
    unsafe {
        timeBeginPeriod(1);
        let me = GetCurrentProcess();
        let _ = SetPriorityClass(me, ABOVE_NORMAL_PRIORITY_CLASS);
        let st = PROCESS_POWER_THROTTLING_STATE {
            Version: PROCESS_POWER_THROTTLING_CURRENT_VERSION,
            ControlMask: PROCESS_POWER_THROTTLING_EXECUTION_SPEED | PROCESS_POWER_THROTTLING_IGNORE_TIMER_RESOLUTION,
            StateMask: 0,
        };
        let _ = SetProcessInformation(
            me,
            ProcessPowerThrottling,
            &st as *const _ as *const _,
            std::mem::size_of::<PROCESS_POWER_THROTTLING_STATE>() as u32,
        );
    }
}

pub fn system_russian() -> bool {
    let id = unsafe { windows::Win32::Globalization::GetUserDefaultUILanguage() };
    matches!(id & 0x3ff, 0x19 | 0x22 | 0x23 | 0x3f)
}

// ---------- файлы ----------

pub fn exe_path() -> PathBuf {
    std::env::current_exe().unwrap_or_else(|_| PathBuf::from("hush.exe"))
}

// рядом с exe, а если туда нельзя писать — %LOCALAPPDATA%\hush
pub fn data_dir() -> &'static Path {
    static DIR: OnceLock<PathBuf> = OnceLock::new();
    DIR.get_or_init(|| {
        let mut d = exe_path().parent().map(Path::to_path_buf).unwrap_or_else(|| PathBuf::from("."));
        let probe = d.join(".hush-write-test");
        if fs::write(&probe, b"").is_ok() {
            let _ = fs::remove_file(&probe);
        } else if let Ok(base) = std::env::var("LOCALAPPDATA") {
            d = PathBuf::from(base).join(APP);
            let _ = fs::create_dir_all(&d);
        }
        for n in ["config.json", "calib.json"] {
            let (old, cur) = (d.join(format!("micaec-{n}")), d.join(format!("{APP}-{n}")));
            if !cur.exists() {
                if let Ok(b) = fs::read(&old) {
                    let _ = fs::write(&cur, b);
                }
            }
        }
        d
    })
}

pub fn data_file(name: &str) -> PathBuf {
    data_dir().join(format!("{APP}-{name}"))
}

pub fn rotate(path: &Path, limit: u64) {
    if fs::metadata(path).map(|m| m.len() > limit).unwrap_or(false) {
        let old = path.with_extension("txt.old");
        let _ = fs::remove_file(&old);
        let _ = fs::rename(path, old);
    }
}

pub fn timestamp() -> String {
    let t = unsafe { windows::Win32::System::SystemInformation::GetLocalTime() };
    format!(
        "{:04}-{:02}-{:02} {:02}:{:02}:{:02}.{:03}",
        t.wYear, t.wMonth, t.wDay, t.wHour, t.wMinute, t.wSecond, t.wMilliseconds
    )
}

static LOG_MU: Mutex<()> = Mutex::new(());

pub fn log_event(s: &str) {
    let _g = LOG_MU.lock();
    let path = data_file("log.txt");
    rotate(&path, 512 * 1024);
    if let Ok(mut f) = OpenOptions::new().create(true).append(true).open(&path) {
        let _ = write!(f, "{}  {}\r\n", timestamp(), s);
    }
}

#[macro_export]
macro_rules! logf {
    ($($a:tt)*) => { $crate::win::sys::log_event(&format!($($a)*)) };
}

// ---------- консоль ----------

static CONSOLE: OnceLock<bool> = OnceLock::new();

// родительская консоль, если запущено из терминала, иначе новая
pub fn use_console(allocate: bool) -> bool {
    *CONSOLE.get_or_init(|| unsafe {
        if AttachConsole(ATTACH_PARENT_PROCESS).is_err() && (!allocate || AllocConsole().is_err()) {
            return false;
        }
        use windows::Win32::Storage::FileSystem::*;
        use windows::Win32::System::Console::{GetStdHandle, SetStdHandle, STD_ERROR_HANDLE, STD_HANDLE, STD_INPUT_HANDLE, STD_OUTPUT_HANDLE};
        let open = |n: &str| {
            CreateFileW(&HSTRING::from(n), (GENERIC_READ | GENERIC_WRITE).0, FILE_SHARE_READ | FILE_SHARE_WRITE, None, OPEN_EXISTING, FILE_FLAGS_AND_ATTRIBUTES(0), None)
        };
        // перенаправленный в файл или пайп вывод не трогаем
        let redirected = |k: STD_HANDLE| match GetStdHandle(k) {
            Ok(h) if !h.is_invalid() && !h.0.is_null() => matches!(GetFileType(h), FILE_TYPE_DISK | FILE_TYPE_PIPE),
            _ => false,
        };
        if let Ok(h) = open("CONOUT$") {
            for k in [STD_OUTPUT_HANDLE, STD_ERROR_HANDLE] {
                if !redirected(k) {
                    let _ = SetStdHandle(k, h);
                }
            }
        }
        if !redirected(STD_INPUT_HANDLE) {
            if let Ok(h) = open("CONIN$") {
                let _ = SetStdHandle(STD_INPUT_HANDLE, h);
            }
        }
        let _ = SetConsoleOutputCP(65001);
        true
    })
}

pub fn has_console() -> bool {
    CONSOLE.get().copied().unwrap_or(false)
}

pub fn wait_enter() {
    let mut s = String::new();
    let _ = std::io::stdin().read_line(&mut s);
}

// ---------- сообщения ----------

pub fn message_box(text: &str, flags: u32) -> i32 {
    unsafe { MessageBoxW(None, &HSTRING::from(text), &HSTRING::from(APP), MESSAGEBOX_STYLE(flags)).0 }
}

pub fn fatal(text: &str) -> ! {
    log_event(&format!("ошибка: {text}"));
    if has_console() {
        println!("\n{}: {}", l("ОШИБКА", "ERROR"), text);
        println!("{}", l("\nНажми Enter, чтобы закрыть окно...", "\nPress Enter to close..."));
        wait_enter();
    } else {
        message_box(text, 0x10);
    }
    std::process::exit(1);
}

pub fn open_url(u: &str) {
    unsafe {
        ShellExecuteW(None, &HSTRING::from("open"), &HSTRING::from(u), PCWSTR::null(), PCWSTR::null(), SW_SHOWNORMAL);
    }
}

// ---------- один экземпляр ----------

pub const PANEL_CLASS: &str = "HushPanel";
pub const WM_SHOW_PANEL: u32 = 0x0400 + 4;

// false — hush уже запущен; тогда просим его показать панель
pub fn single_instance(show_other: bool, wait: bool) -> bool {
    let name = HSTRING::from(r"Local\hush-single-instance");
    let open = || unsafe {
        let h = CreateMutexW(None, false, &name).ok();
        (h, GetLastError() == ERROR_ALREADY_EXISTS)
    };
    let (mut h, mut exists) = open();
    let mut i = 0;
    while wait && exists && i < 50 {
        if let Some(x) = h {
            unsafe {
                let _ = CloseHandle(x);
            }
        }
        std::thread::sleep(Duration::from_millis(200));
        (h, exists) = open();
        i += 1;
    }
    if h.is_none() || !exists {
        return true;
    }
    if show_other {
        unsafe {
            if let Ok(w) = FindWindowW(&HSTRING::from(PANEL_CLASS), PCWSTR::null()) {
                let _ = PostMessageW(Some(w), WM_SHOW_PANEL, WPARAM(0), LPARAM(0));
            }
        }
    }
    false
}

pub fn hwnd_from(v: isize) -> HWND {
    HWND(v as *mut _)
}

// ---------- автозапуск ----------

const RUN_KEY: &str = r"Software\Microsoft\Windows\CurrentVersion\Run";

fn run_key(access: REG_SAM_FLAGS) -> Option<HKEY> {
    let mut k = HKEY::default();
    let r = unsafe { RegOpenKeyExW(HKEY_CURRENT_USER, &HSTRING::from(RUN_KEY), None, access, &mut k) };
    r.is_ok().then_some(k)
}

fn reg_has(k: HKEY, name: &str) -> bool {
    unsafe { RegQueryValueExW(k, &HSTRING::from(name), None, None, None, None).is_ok() }
}

pub fn autostart_on() -> bool {
    let Some(k) = run_key(KEY_QUERY_VALUE) else { return false };
    let on = reg_has(k, APP);
    unsafe {
        let _ = RegCloseKey(k);
    }
    on
}

pub fn set_autostart(on: bool) -> Result<(), String> {
    let k = run_key(KEY_SET_VALUE).ok_or_else(|| "registry".to_string())?;
    let r = unsafe {
        if on {
            let v = format!("\"{}\" -background", exe_path().display());
            let w = wide(&v);
            let bytes = std::slice::from_raw_parts(w.as_ptr() as *const u8, w.len() * 2);
            RegSetValueExW(k, &HSTRING::from(APP), None, REG_SZ, Some(bytes))
        } else {
            let r = RegDeleteValueW(k, &HSTRING::from(APP));
            if r == windows::Win32::Foundation::ERROR_FILE_NOT_FOUND {
                windows::Win32::Foundation::ERROR_SUCCESS
            } else {
                r
            }
        }
    };
    unsafe {
        let _ = RegCloseKey(k);
    }
    if r.is_ok() {
        Ok(())
    } else {
        Err(format!("registry error {}", r.0))
    }
}

// автозапуск прежней версии (micaec) заменяется на hush; true — он был
pub fn migrate_autostart() -> bool {
    let Some(k) = run_key(KEY_QUERY_VALUE | KEY_SET_VALUE) else { return false };
    let had = reg_has(k, "micaec");
    if had {
        unsafe {
            let _ = RegDeleteValueW(k, &HSTRING::from("micaec"));
        }
    }
    unsafe {
        let _ = RegCloseKey(k);
    }
    if had {
        let _ = set_autostart(true);
    }
    had
}
