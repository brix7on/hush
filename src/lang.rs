use std::sync::OnceLock;

static RU: OnceLock<bool> = OnceLock::new();

// язык интерфейса: HUSH_LANG, иначе язык системы
pub fn init(system_ru: bool) {
    let ru = match std::env::var("HUSH_LANG") {
        Ok(v) if !v.is_empty() => v.to_lowercase().starts_with("ru"),
        _ => system_ru,
    };
    let _ = RU.set(ru);
}

pub fn is_ru() -> bool {
    *RU.get_or_init(|| std::env::var("HUSH_LANG").map(|v| v.to_lowercase().starts_with("ru")).unwrap_or(false))
}

pub fn l(ru: &'static str, en: &'static str) -> &'static str {
    if is_ru() {
        ru
    } else {
        en
    }
}

// строка с подстановками под язык системы
#[macro_export]
macro_rules! lf {
    ($ru:literal, $en:literal $(, $a:expr)* $(,)?) => {
        if $crate::lang::is_ru() { format!($ru $(, $a)*) } else { format!($en $(, $a)*) }
    };
}
