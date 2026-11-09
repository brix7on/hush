#![cfg_attr(windows, windows_subsystem = "windows")]

#[cfg(windows)]
mod win;

#[cfg(windows)]
fn main() {
    win::app::main();
}

#[cfg(not(windows))]
fn main() {
    eprintln!("hush runs on Windows");
}
