use std::io::{self, Write};

// многоканальный 24-битный WAV (WAVE_FORMAT_EXTENSIBLE)
pub fn write_wav24<W: Write>(w: &mut W, rate: usize, chans: &[Vec<f32>]) -> io::Result<()> {
    let nch = chans.len();
    let n = chans.first().map_or(0, |c| c.len());
    let data_len = (n * nch * 3) as u32;
    let mut h = Vec::with_capacity(68);
    h.extend_from_slice(b"RIFF");
    h.extend_from_slice(&(4 + 8 + 40 + 8 + data_len).to_le_bytes());
    h.extend_from_slice(b"WAVEfmt ");
    h.extend_from_slice(&40u32.to_le_bytes());
    h.extend_from_slice(&0xFFFEu16.to_le_bytes());
    h.extend_from_slice(&(nch as u16).to_le_bytes());
    h.extend_from_slice(&(rate as u32).to_le_bytes());
    h.extend_from_slice(&((rate * nch * 3) as u32).to_le_bytes());
    h.extend_from_slice(&((nch * 3) as u16).to_le_bytes());
    h.extend_from_slice(&24u16.to_le_bytes());
    h.extend_from_slice(&22u16.to_le_bytes());
    h.extend_from_slice(&24u16.to_le_bytes());
    h.extend_from_slice(&0u32.to_le_bytes());
    h.extend_from_slice(&[0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71]);
    h.extend_from_slice(b"data");
    h.extend_from_slice(&data_len.to_le_bytes());
    w.write_all(&h)?;
    let mut buf = Vec::with_capacity(4096 * nch * 3);
    for start in (0..n).step_by(4096) {
        buf.clear();
        for j in start..(start + 4096).min(n) {
            for c in chans {
                let x = ((c[j] as f64).clamp(-1.0, 1.0) * 8388607.0).round() as i32;
                buf.extend_from_slice(&[x as u8, (x >> 8) as u8, (x >> 16) as u8]);
            }
        }
        w.write_all(&buf)?;
    }
    Ok(())
}
