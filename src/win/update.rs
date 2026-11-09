use std::ffi::c_void;
use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use sha2::{Digest, Sha256};
use windows::core::HSTRING;
use windows::Win32::Networking::WinInet::*;

use hush::{l, newer_version, APP, VERSION};

// ---------- самообновление из релизов GitHub ----------

const SELF_REPO: &str = "brix7on/hush";

#[derive(Clone, Default)]
pub struct UpdInfo {
    pub latest: String,
    pub busy: bool,
    pub msg: String,
}

struct Upd {
    st: UpdInfo,
    exe_url: String,
    sums_url: String,
    told: String,
}

static UPD: Mutex<Upd> = Mutex::new(Upd {
    st: UpdInfo { latest: String::new(), busy: false, msg: String::new() },
    exe_url: String::new(),
    sums_url: String::new(),
    told: String::new(),
});

pub fn state() -> UpdInfo {
    UPD.lock().unwrap().st.clone()
}

fn set_msg(m: String) {
    UPD.lock().unwrap().st.msg = m;
}

struct Net(*mut c_void);
impl Drop for Net {
    fn drop(&mut self) {
        if !self.0.is_null() {
            unsafe {
                let _ = InternetCloseHandle(self.0);
            }
        }
    }
}

fn http_get(url: &str, mut sink: impl FnMut(&[u8], u64) -> Result<(), String>) -> Result<(), String> {
    unsafe {
        let agent = HSTRING::from(format!("{APP}/{VERSION}"));
        let inet = Net(InternetOpenW(&agent, INTERNET_OPEN_TYPE_PRECONFIG.0, None, None, 0));
        if inet.0.is_null() {
            return Err("InternetOpen".into());
        }
        let hdr: Vec<u16> = "Accept: */*\r\n".encode_utf16().collect();
        let flags = INTERNET_FLAG_RELOAD | INTERNET_FLAG_NO_CACHE_WRITE | INTERNET_FLAG_SECURE | INTERNET_FLAG_NO_UI;
        let h = Net(InternetOpenUrlW(inet.0, &HSTRING::from(url), Some(&hdr), flags, None));
        if h.0.is_null() {
            return Err(l("нет связи с GitHub", "cannot reach GitHub").into());
        }
        let mut code = 0u32;
        let mut len = 4u32;
        let _ = HttpQueryInfoW(h.0, HTTP_QUERY_STATUS_CODE | HTTP_QUERY_FLAG_NUMBER, Some(&mut code as *mut _ as *mut c_void), &mut len, None);
        if code != 200 {
            return Err(format!("HTTP {code}"));
        }
        let mut total = 0u32;
        let mut len = 4u32;
        let _ = HttpQueryInfoW(h.0, HTTP_QUERY_CONTENT_LENGTH | HTTP_QUERY_FLAG_NUMBER, Some(&mut total as *mut _ as *mut c_void), &mut len, None);
        let mut buf = vec![0u8; 64 << 10];
        loop {
            let mut n = 0u32;
            InternetReadFile(h.0, buf.as_mut_ptr() as *mut c_void, buf.len() as u32, &mut n).map_err(|e| e.to_string())?;
            if n == 0 {
                break;
            }
            sink(&buf[..n as usize], total as u64)?;
        }
    }
    Ok(())
}

fn get_text(url: &str) -> Result<String, String> {
    let mut v = Vec::new();
    http_get(url, |b, _| {
        v.extend_from_slice(b);
        Ok(())
    })?;
    String::from_utf8(v).map_err(|e| e.to_string())
}

// спрашивает GitHub о последнем релизе; второе значение — о версии ещё не сообщали
pub fn check() -> Result<(String, bool), String> {
    if VERSION == "dev" {
        return Ok((String::new(), false));
    }
    let body = get_text(&format!("https://api.github.com/repos/{SELF_REPO}/releases/latest"))?;
    let rel: serde_json::Value = serde_json::from_str(&body).map_err(|e| e.to_string())?;
    let tag = rel["tag_name"].as_str().unwrap_or("");
    if !newer_version(tag, VERSION) {
        UPD.lock().unwrap().st.latest.clear();
        return Ok((String::new(), false));
    }
    let (mut exe, mut sums) = (String::new(), String::new());
    for a in rel["assets"].as_array().into_iter().flatten() {
        let url = a["browser_download_url"].as_str().unwrap_or("").to_string();
        match a["name"].as_str() {
            Some("hush.exe") => exe = url,
            Some("SHA256SUMS") => sums = url,
            _ => {}
        }
    }
    if exe.is_empty() || sums.is_empty() {
        return Ok((String::new(), false));
    }
    let tag = tag.trim_start_matches('v').to_string();
    let mut u = UPD.lock().unwrap();
    u.st.latest = tag.clone();
    u.exe_url = exe;
    u.sums_url = sums;
    let fresh = u.told != tag;
    u.told = tag.clone();
    Ok((tag, fresh))
}

fn sum_for(sums: &str, name: &str) -> String {
    for line in sums.lines() {
        let f: Vec<&str> = line.split_whitespace().collect();
        if f.len() == 2 && f[1].trim_start_matches('*') == name {
            return f[0].to_lowercase();
        }
    }
    String::new()
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

// скачивает новый exe, сверяет сумму и подменяет текущий файл
pub fn install(work: &Path) -> Result<PathBuf, String> {
    let (exe_url, sums_url) = {
        let mut u = UPD.lock().unwrap();
        if u.st.busy || u.st.latest.is_empty() {
            return Err(l("обновлений нет", "no updates").into());
        }
        u.st.busy = true;
        u.st.msg = l("загрузка", "downloading").into();
        (u.exe_url.clone(), u.sums_url.clone())
    };
    let res = (|| {
        let exe = std::env::current_exe().map_err(|e| e.to_string())?;
        let exe = fs::canonicalize(&exe).map(|p| {
            let s = p.to_string_lossy().to_string();
            PathBuf::from(s.strip_prefix(r"\\?\").unwrap_or(&s))
        }).unwrap_or(exe);
        let dir = work.join("update");
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
        let np = dir.join("hush.exe");
        let r = (|| {
            let sums = get_text(&sums_url)?;
            let mut file = fs::File::create(&np).map_err(|e| e.to_string())?;
            let mut h = Sha256::new();
            let mut done = 0u64;
            let ru = hush::lang::is_ru();
            http_get(&exe_url, |b, total| {
                file.write_all(b).map_err(|e| e.to_string())?;
                h.update(b);
                done += b.len() as u64;
                if total > 0 {
                    let p = done * 100 / total;
                    set_msg(if ru { format!("загрузка {p}%") } else { format!("downloading {p}%") });
                }
                Ok(())
            })?;
            drop(file);
            let want = sum_for(&sums, "hush.exe");
            if want.is_empty() || want != hex(&h.finalize()) {
                return Err(l("контрольная сумма не совпала", "checksum mismatch").to_string());
            }
            let old = exe.with_extension("exe.old");
            let _ = fs::remove_file(&old);
            let bad = |e: std::io::Error| {
                if ru {
                    format!("не удалось заменить файл: {e}")
                } else {
                    format!("cannot replace the file: {e}")
                }
            };
            fs::rename(&exe, &old).map_err(bad)?;
            if let Err(e) = fs::copy(&np, &exe) {
                let _ = fs::rename(&old, &exe);
                return Err(bad(e));
            }
            set_msg(l("перезапуск", "restarting").into());
            Ok(exe.clone())
        })();
        let _ = fs::remove_dir_all(&dir);
        r
    })();
    UPD.lock().unwrap().st.busy = false;
    res
}

// новый exe с теми же ключами
pub fn restart_after_update(exe: &Path) -> Result<(), String> {
    use std::os::windows::process::CommandExt;
    let mut args = vec!["-after-update".to_string()];
    for a in std::env::args().skip(1) {
        let t = a.trim_start_matches('-');
        if t == "background" || t == "after-update" {
            continue;
        }
        args.push(a);
    }
    std::process::Command::new(exe).args(args).creation_flags(0x0000_0008).spawn().map(|_| ()).map_err(|e| e.to_string())
}

// удаляет exe, оставшийся от прошлого обновления
pub fn cleanup_old_exe() {
    let old = super::sys::exe_path().with_extension("exe.old");
    for _ in 0..20 {
        match fs::remove_file(&old) {
            Ok(()) => return,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return,
            Err(_) => std::thread::sleep(std::time::Duration::from_secs(1)),
        }
    }
}
