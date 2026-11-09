mod fft;
pub mod agc;
pub mod amp;
pub mod denoise;
pub mod eq;
pub mod suppress;

pub use fft::{Fft, C64};

pub const RATE: usize = 48000;

pub fn db(p: f64) -> f64 {
    10.0 * p.max(1e-12).log10()
}

pub fn f64_cell(v: f64) -> std::sync::atomic::AtomicU64 {
    std::sync::atomic::AtomicU64::new(v.to_bits())
}
