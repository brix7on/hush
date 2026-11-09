pub mod audio;
pub mod diag;
pub mod dsp;
pub mod glyphs;
pub mod lang;
pub mod panel;
pub mod pipe;

pub const APP: &str = "hush";

pub const VERSION: &str = match option_env!("HUSH_VERSION") {
    Some(v) => v,
    None => "dev",
};

pub use lang::l;

// «1.2.3» и «1.2.3-rc.1»: пред-релиз старше прошлых версий, но младше своего релиза
fn parse_ver(v: &str) -> (Vec<u64>, Option<Vec<u64>>) {
    let v = v.trim_start_matches('v');
    let (main, pre) = match v.split_once('-') {
        Some((m, p)) => (m, Some(p)),
        None => (v, None),
    };
    let nums = |s: &str| s.split('.').map(|x| x.chars().filter(char::is_ascii_digit).collect::<String>().parse().unwrap_or(0)).collect();
    (nums(main), pre.map(nums))
}

pub fn newer_version(a: &str, b: &str) -> bool {
    let ((ma, pa), (mb, pb)) = (parse_ver(a), parse_ver(b));
    for i in 0..ma.len().max(mb.len()) {
        let (x, y) = (ma.get(i).copied().unwrap_or(0), mb.get(i).copied().unwrap_or(0));
        if x != y {
            return x > y;
        }
    }
    match (pa, pb) {
        (None, Some(_)) => true,
        (Some(x), Some(y)) => x > y,
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::newer_version;

    #[test]
    fn versions() {
        assert!(newer_version("v1.0.5", "1.0.4"));
        assert!(newer_version("2.0.0-rc.1", "1.0.5"));
        assert!(newer_version("2.0.0", "2.0.0-rc.1"));
        assert!(newer_version("2.0.0-rc.2", "2.0.0-rc.1"));
        assert!(!newer_version("1.0.5", "1.0.5"));
        assert!(!newer_version("2.0.0-rc.1", "2.0.0"));
    }
}
