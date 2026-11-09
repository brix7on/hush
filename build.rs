use std::env;
use std::fs;
use std::path::PathBuf;

fn main() {
    println!("cargo:rerun-if-changed=assets");
    println!("cargo:rerun-if-env-changed=HUSH_VERSION");
    if env::var("CARGO_CFG_TARGET_OS").as_deref() != Ok("windows") {
        return;
    }
    let ver = env::var("HUSH_VERSION").unwrap_or_else(|_| "0.0.0".into());
    let nums: Vec<u32> = ver.split(['.', '-']).take(3).map(|v| v.parse().unwrap_or(0)).collect();
    let n = |i: usize| nums.get(i).copied().unwrap_or(0);
    let root = PathBuf::from(env::var("CARGO_MANIFEST_DIR").unwrap()).join("assets");
    let p = |f: &str| root.join(f).display().to_string().replace('\\', "/");
    let rc = format!(
        r#"#include <winver.h>
APP ICON "{ico}"
1 24 "{man}"
1 VERSIONINFO
FILEVERSION {a},{b},{c},0
PRODUCTVERSION {a},{b},{c},0
FILEOS VOS_NT_WINDOWS32
FILETYPE VFT_APP
BEGIN
  BLOCK "StringFileInfo"
  BEGIN
    BLOCK "040904b0"
    BEGIN
      VALUE "CompanyName", "brix7on"
      VALUE "FileDescription", "hush - headset bleed removal"
      VALUE "FileVersion", "{ver}"
      VALUE "LegalCopyright", "(c) 2026 brix7on, MIT"
      VALUE "ProductName", "hush"
      VALUE "ProductVersion", "{ver}"
      VALUE "OriginalFilename", "hush.exe"
    END
  END
  BLOCK "VarFileInfo"
  BEGIN
    VALUE "Translation", 0x409, 1200
  END
END
"#,
        ico = p("hush.ico"),
        man = p("hush.manifest"),
        a = n(0),
        b = n(1),
        c = n(2),
    );
    let out = PathBuf::from(env::var("OUT_DIR").unwrap()).join("hush.rc");
    fs::write(&out, rc).unwrap();
    embed_resource::compile(&out, embed_resource::NONE).manifest_required().unwrap();
}
