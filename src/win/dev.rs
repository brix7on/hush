use std::ffi::c_void;

use windows::core::Interface;
use windows::Win32::Devices::FunctionDiscovery::PKEY_Device_FriendlyName;
use windows::Win32::Foundation::{CloseHandle, PROPERTYKEY};
use windows::Win32::Media::Audio::*;
use windows::Win32::System::Com::StructuredStorage::{PropVariantClear, PropVariantToStringAlloc, PROPVARIANT};
use windows::Win32::System::Com::{CoCreateInstance, CoTaskMemFree, CLSCTX_ALL, STGM_READ};
use windows::Win32::System::Threading::{OpenProcess, QueryFullProcessImageNameW, PROCESS_NAME_WIN32, PROCESS_QUERY_LIMITED_INFORMATION};
use windows::Win32::System::Variant::VT_BLOB;

use hush::audio::Format;
use hush::l;

pub const HR_UNSUPPORTED: u32 = 0x8889_0008;
pub const HR_IN_USE: u32 = 0x8889_000A;
pub const HR_EXCL_NOT_ALLOWED: u32 = 0x8889_000E;
pub const HR_NOT_ALIGNED: u32 = 0x8889_0019;
pub const HR_BAD_PERIOD: u32 = 0x8889_0020;

pub fn hr(e: &windows::core::Error) -> u32 {
    e.code().0 as u32
}

// понятное название частых ошибок WASAPI
pub fn audio_err(e: &windows::core::Error) -> String {
    match hr(e) {
        HR_UNSUPPORTED => l("формат не подходит", "format not supported").into(),
        HR_BAD_PERIOD => l("период не подходит", "period not supported").into(),
        HR_IN_USE => l("устройство занято", "device in use").into(),
        HR_EXCL_NOT_ALLOWED => l("монопольный режим запрещён", "exclusive mode disabled").into(),
        HR_NOT_ALIGNED => l("период не выровнен", "period not aligned").into(),
        _ => first_line(&e.to_string()).to_string(),
    }
}

pub fn first_line(s: &str) -> &str {
    s.split('\n').next().unwrap_or(s)
}

pub fn enumerator() -> windows::core::Result<IMMDeviceEnumerator> {
    unsafe { CoCreateInstance(&MMDeviceEnumerator, None, CLSCTX_ALL) }
}

fn prop(dev: &IMMDevice, key: &PROPERTYKEY) -> Option<PROPVARIANT> {
    unsafe {
        let ps = dev.OpenPropertyStore(STGM_READ).ok()?;
        ps.GetValue(key).ok()
    }
}

pub fn friendly_name(dev: &IMMDevice) -> String {
    let Some(mut pv) = prop(dev, &PKEY_Device_FriendlyName) else { return "?".into() };
    let s = unsafe {
        match PropVariantToStringAlloc(&pv) {
            Ok(p) => {
                let s = p.to_string().unwrap_or_else(|_| "?".into());
                CoTaskMemFree(Some(p.0 as *const c_void));
                s
            }
            Err(_) => "?".into(),
        }
    };
    unsafe {
        let _ = PropVariantClear(&mut pv);
    }
    s
}

pub fn device_id(dev: &IMMDevice) -> String {
    unsafe {
        match dev.GetId() {
            Ok(p) => {
                let s = p.to_string().unwrap_or_default();
                CoTaskMemFree(Some(p.0 as *const c_void));
                s
            }
            Err(_) => String::new(),
        }
    }
}

pub fn list_devices(flow: EDataFlow) -> windows::core::Result<Vec<(IMMDevice, String)>> {
    let e = enumerator()?;
    let dc = unsafe { e.EnumAudioEndpoints(flow, DEVICE_STATE_ACTIVE)? };
    let n = unsafe { dc.GetCount()? };
    let mut res = Vec::new();
    for i in 0..n {
        if let Ok(d) = unsafe { dc.Item(i) } {
            let name = friendly_name(&d);
            res.push((d, name));
        }
    }
    Ok(res)
}

pub fn device_names(flow: EDataFlow) -> Vec<String> {
    list_devices(flow).map(|l| l.into_iter().map(|(_, n)| n).collect()).unwrap_or_default()
}

// виртуальный кабель нельзя брать ни микрофоном, ни наушниками
pub fn is_cable(name: &str) -> bool {
    let n = name.to_lowercase();
    n.contains("cable") || n.contains("vb-audio")
}

pub fn contains_fold(s: &str, sub: &str) -> bool {
    !sub.is_empty() && s.to_lowercase().contains(&sub.to_lowercase())
}

// по части названия; пусто или не нашлось — устройство Windows по умолчанию
pub fn pick_device(flow: EDataFlow, sub: &str, what: &str, required: bool) -> Result<(IMMDevice, String), String> {
    let auto = || -> Result<(IMMDevice, String), String> {
        let e = enumerator().map_err(|e| e.to_string())?;
        if let Ok(d) = unsafe { e.GetDefaultAudioEndpoint(flow, eConsole) } {
            let n = friendly_name(&d);
            if !is_cable(&n) {
                return Ok((d, n));
            }
        }
        let list = list_devices(flow).map_err(|e| e.to_string())?;
        list.into_iter().find(|(_, n)| !is_cable(n)).ok_or_else(|| {
            if hush::lang::is_ru() {
                format!("не найдено ни одного устройства ({what}), кроме виртуального кабеля")
            } else {
                format!("no device found ({what}) other than the virtual cable")
            }
        })
    };
    if sub.is_empty() {
        return auto();
    }
    let list = list_devices(flow).map_err(|e| e.to_string())?;
    let names: Vec<String> = list.iter().map(|(_, n)| format!("  • {n}")).collect();
    if let Some(found) = list.into_iter().find(|(_, n)| contains_fold(n, sub)) {
        return Ok(found);
    }
    if !required {
        if let Ok((d, n)) = auto() {
            println!("({what}: «{sub}» не найден — беру «{n}»)");
            return Ok((d, n));
        }
    }
    Err(if hush::lang::is_ru() {
        format!("{what}: не найдено устройство «{sub}». Доступные:\n{}", names.join("\n"))
    } else {
        format!("{what}: device \"{sub}\" not found. Available:\n{}", names.join("\n"))
    })
}

// ---------- форматы ----------

pub struct MixFormat(pub *mut WAVEFORMATEX);

impl MixFormat {
    pub fn bytes(&self) -> &[u8] {
        unsafe { std::slice::from_raw_parts(self.0 as *const u8, 18 + (*self.0).cbSize as usize) }
    }
}

impl Drop for MixFormat {
    fn drop(&mut self) {
        unsafe { CoTaskMemFree(Some(self.0 as *const c_void)) };
    }
}

pub fn activate(dev: &IMMDevice) -> Result<IAudioClient, String> {
    unsafe { dev.Activate::<IAudioClient>(CLSCTX_ALL, None) }.map_err(|e| format!("Activate: {e}"))
}

pub fn open_client(dev: &IMMDevice) -> Result<(IAudioClient, MixFormat, Format), String> {
    let ac = activate(dev)?;
    let mf = MixFormat(unsafe { ac.GetMixFormat() }.map_err(|e| format!("GetMixFormat: {e}"))?);
    let f = Format::parse(mf.bytes())?;
    Ok((ac, mf, f))
}

// формат, в котором устройство работает в Windows
pub fn device_format(dev: &IMMDevice) -> Option<Vec<u8>> {
    let mut pv = prop(dev, &PKEY_AudioEngine_DeviceFormat)?;
    let out = unsafe {
        let a = &pv.Anonymous.Anonymous;
        if a.vt != VT_BLOB {
            None
        } else {
            let b = a.Anonymous.blob;
            if b.pBlobData.is_null() || b.cbSize < 16 || b.cbSize > 1024 {
                None
            } else {
                Some(std::slice::from_raw_parts(b.pBlobData, b.cbSize as usize).to_vec())
            }
        }
    };
    unsafe {
        let _ = PropVariantClear(&mut pv);
    }
    out
}

// WAVEFORMATEXTENSIBLE: container — бит на отсчёт в памяти, valid — значащих
pub fn wave_ext(rate: usize, ch: usize, container: usize, valid: usize, float: bool) -> Vec<u8> {
    let mut b = vec![0u8; 40];
    let align = ch * container / 8;
    b[0..2].copy_from_slice(&0xFFFEu16.to_le_bytes());
    b[2..4].copy_from_slice(&(ch as u16).to_le_bytes());
    b[4..8].copy_from_slice(&(rate as u32).to_le_bytes());
    b[8..12].copy_from_slice(&((rate * align) as u32).to_le_bytes());
    b[12..14].copy_from_slice(&(align as u16).to_le_bytes());
    b[14..16].copy_from_slice(&(container as u16).to_le_bytes());
    b[16..18].copy_from_slice(&22u16.to_le_bytes());
    b[18..20].copy_from_slice(&(valid as u16).to_le_bytes());
    let mask: u32 = if ch == 2 { 0x3 } else { 0x4 };
    b[20..24].copy_from_slice(&mask.to_le_bytes());
    let sub: u32 = if float { 3 } else { 1 };
    b[24..28].copy_from_slice(&sub.to_le_bytes());
    b[28..30].copy_from_slice(&0u16.to_le_bytes());
    b[30..32].copy_from_slice(&0x0010u16.to_le_bytes());
    b[32..40].copy_from_slice(&[0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71]);
    b
}

pub fn wave_ex(rate: usize, ch: usize, bits: usize) -> Vec<u8> {
    let mut b = vec![0u8; 18];
    let align = ch * bits / 8;
    b[0..2].copy_from_slice(&1u16.to_le_bytes());
    b[2..4].copy_from_slice(&(ch as u16).to_le_bytes());
    b[4..8].copy_from_slice(&(rate as u32).to_le_bytes());
    b[8..12].copy_from_slice(&((rate * align) as u32).to_le_bytes());
    b[12..14].copy_from_slice(&(align as u16).to_le_bytes());
    b[14..16].copy_from_slice(&(bits as u16).to_le_bytes());
    b
}

fn wfx(raw: &[u8]) -> *const WAVEFORMATEX {
    raw.as_ptr() as *const WAVEFORMATEX
}

// монопольный вывод: звук не идёт через микшер и не попадает в демонстрацию экрана
pub fn open_exclusive_render(dev: &IMMDevice) -> Result<(IAudioClient, Format), String> {
    let mut last = String::from("exclusive");
    for rate in [48000usize, 44100, 96000] {
        for bits in [16usize, 24, 32] {
            for ch in [2usize, 1] {
                let raw = wave_ex(rate, ch, bits);
                let f = Format { rate, channels: ch, bits, float: false, block_align: ch * bits / 8 };
                for dur in [1_000_000i64, 500_000, 200_000] {
                    let ac = activate(dev)?;
                    let mut r = unsafe { ac.Initialize(AUDCLNT_SHAREMODE_EXCLUSIVE, 0, dur, dur, wfx(&raw), None) };
                    if let Err(e) = &r {
                        if hr(e) == HR_NOT_ALIGNED {
                            if let Ok(frames) = unsafe { ac.GetBufferSize() } {
                                if frames > 0 {
                                    let ac2 = activate(dev)?;
                                    let d2 = (10_000_000.0 * frames as f64 / rate as f64 + 0.5) as i64;
                                    r = unsafe { ac2.Initialize(AUDCLNT_SHAREMODE_EXCLUSIVE, 0, d2, d2, wfx(&raw), None) };
                                    if r.is_ok() {
                                        return Ok((ac2, f));
                                    }
                                }
                            }
                        }
                    }
                    match r {
                        Ok(()) => return Ok((ac, f)),
                        Err(e) => last = audio_err(&e),
                    }
                }
            }
        }
    }
    Err(last)
}

// общий режим с минимальным периодом (IAudioClient3, Windows 10+)
pub fn open_capture_low_lat(dev: &IMMDevice) -> Result<(IAudioClient, Format, u32), String> {
    let ac3 = unsafe { dev.Activate::<IAudioClient3>(CLSCTX_ALL, None) }.map_err(|e| format!("IAudioClient3: {e}"))?;
    let mf = MixFormat(unsafe { ac3.GetMixFormat() }.map_err(|e| format!("GetMixFormat: {e}"))?);
    let f = Format::parse(mf.bytes())?;
    let (mut def, mut fund, mut mn, mut mx) = (0u32, 0u32, 0u32, 0u32);
    unsafe { ac3.GetSharedModeEnginePeriod(mf.0, &mut def, &mut fund, &mut mn, &mut mx) }
        .map_err(|e| format!("GetSharedModeEnginePeriod: {e}"))?;
    if mn == 0 || mn >= def {
        return Err(format!("driver has no period below {def} frames"));
    }
    unsafe { ac3.InitializeSharedAudioStream(AUDCLNT_STREAMFLAGS_EVENTCALLBACK, mn, mf.0, None) }
        .map_err(|e| format!("InitializeSharedAudioStream: {e}"))?;
    let ac: IAudioClient = ac3.cast().map_err(|e| e.to_string())?;
    Ok((ac, f, mn))
}

pub enum ExclErr {
    Win(windows::core::Error),
    Loses,
    Other(String),
}

// монопольный захват с малым периодом, по событию; min_p — нижняя граница периода (100 нс), <0 — не пробовать
pub fn open_capture_exclusive(dev: &IMMDevice, min_floor: i64) -> Result<(IAudioClient, Format, u32), ExclErr> {
    let probe = activate(dev).map_err(ExclErr::Other)?;
    let (mut def_p, mut min_p) = (0i64, 0i64);
    if unsafe { probe.GetDevicePeriod(Some(&mut def_p), Some(&mut min_p)) }.is_err() || min_p <= 0 {
        min_p = 30000;
    }
    min_p = min_p.max(20000);
    if min_floor < 0 {
        return Err(ExclErr::Loses);
    }
    min_p = min_p.max(min_floor);
    let mut periods = vec![min_p];
    for p in [30000i64, 50000, 100000] {
        if p > min_p {
            periods.push(p);
        }
    }

    let mut cands: Vec<Vec<u8>> = Vec::new();
    let dev_fmt = device_format(dev);
    let mut dev_desc = l("неизвестен", "unknown").to_string();
    let (mut rates, mut chans) = (Vec::new(), Vec::new());
    if let Some(df) = dev_fmt {
        if let Ok(f) = Format::parse(&df) {
            dev_desc = f.to_string();
            rates.push(f.rate);
            chans.push(f.channels);
        }
        cands.push(df);
    }
    if let Ok(p) = unsafe { probe.GetMixFormat() } {
        let mf = MixFormat(p);
        if let Ok(f) = Format::parse(mf.bytes()) {
            rates.push(f.rate);
            chans.push(f.channels);
        }
    }
    rates.extend([48000, 44100, 96000, 32000, 16000]);
    chans.extend([1, 2]);
    let mut seen_r = Vec::new();
    for &r in &rates {
        if seen_r.contains(&r) {
            continue;
        }
        seen_r.push(r);
        let mut seen_c = Vec::new();
        for &c in &chans {
            if seen_c.contains(&c) || !(1..=2).contains(&c) {
                continue;
            }
            seen_c.push(c);
            cands.extend([
                wave_ext(r, c, 16, 16, false),
                wave_ex(r, c, 16),
                wave_ext(r, c, 24, 24, false),
                wave_ext(r, c, 32, 24, false),
                wave_ext(r, c, 32, 32, false),
                wave_ext(r, c, 32, 32, true),
                wave_ex(r, c, 24),
            ]);
        }
    }

    let mut last = format!("no format fits (device format: {dev_desc})");
    for raw in &cands {
        let Ok(f) = Format::parse(raw) else { continue };
        let hrs = unsafe { probe.IsFormatSupported(AUDCLNT_SHAREMODE_EXCLUSIVE, wfx(raw), None) };
        if hrs.is_err() {
            let c = hrs.0 as u32;
            if c == HR_IN_USE || c == HR_EXCL_NOT_ALLOWED {
                return Err(ExclErr::Win(windows::core::Error::from(hrs)));
            }
            continue;
        }
        for &per in &periods {
            let ac = activate(dev).map_err(ExclErr::Other)?;
            let mut res = unsafe {
                ac.Initialize(AUDCLNT_SHAREMODE_EXCLUSIVE, AUDCLNT_STREAMFLAGS_EVENTCALLBACK, per, per, wfx(raw), None)
            }
            .map(|_| ac.clone());
            if let Err(e) = &res {
                if hr(e) == HR_NOT_ALIGNED {
                    if let Ok(frames) = unsafe { ac.GetBufferSize() } {
                        if frames > 0 {
                            let ac2 = activate(dev).map_err(ExclErr::Other)?;
                            let d2 = (10_000_000.0 * frames as f64 / f.rate as f64 + 0.5) as i64;
                            res = unsafe {
                                ac2.Initialize(AUDCLNT_SHAREMODE_EXCLUSIVE, AUDCLNT_STREAMFLAGS_EVENTCALLBACK, d2, d2, wfx(raw), None)
                            }
                            .map(|_| ac2);
                        }
                    }
                }
            }
            match res {
                Ok(ac) => {
                    let frames = unsafe { ac.GetBufferSize() }
                        .ok()
                        .filter(|&v| v > 0)
                        .unwrap_or((per * f.rate as i64 / 10_000_000) as u32);
                    return Ok((ac, f, frames));
                }
                Err(e) => {
                    let c = hr(&e);
                    if c == HR_IN_USE || c == HR_EXCL_NOT_ALLOWED {
                        return Err(ExclErr::Win(e));
                    }
                    last = format!("{f}: {} (device format: {dev_desc})", audio_err(&e));
                }
            }
        }
    }
    Err(ExclErr::Other(last))
}

// ---------- сессии устройства ----------

pub fn exe_name(pid: u32) -> String {
    if pid == 0 {
        return l("Системные звуки", "System sounds").into();
    }
    unsafe {
        let Ok(h) = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid) else {
            return format!("process {pid}");
        };
        let mut buf = [0u16; 1024];
        let mut n = buf.len() as u32;
        let r = QueryFullProcessImageNameW(h, PROCESS_NAME_WIN32, windows::core::PWSTR(buf.as_mut_ptr()), &mut n);
        let _ = CloseHandle(h);
        if r.is_err() {
            return format!("process {pid}");
        }
        let full = String::from_utf16_lossy(&buf[..n as usize]);
        full.rsplit(['\\', '/']).next().unwrap_or(&full).to_string()
    }
}

pub struct Sess {
    pub exe: String,
    pub active: bool,
}

pub fn device_sessions(dev: &IMMDevice) -> Vec<Sess> {
    let mut out = Vec::new();
    unsafe {
        let Ok(mgr) = dev.Activate::<IAudioSessionManager2>(CLSCTX_ALL, None) else { return out };
        let Ok(en) = mgr.GetSessionEnumerator() else { return out };
        let n = en.GetCount().unwrap_or(0);
        for i in 0..n {
            let Ok(sc) = en.GetSession(i) else { continue };
            let st = sc.GetState().unwrap_or(AudioSessionStateExpired);
            if st == AudioSessionStateExpired {
                continue;
            }
            if let Ok(sc2) = sc.cast::<IAudioSessionControl2>() {
                let pid = sc2.GetProcessId().unwrap_or(0);
                out.push(Sess { exe: exe_name(pid), active: st == AudioSessionStateActive });
            }
        }
    }
    out
}

