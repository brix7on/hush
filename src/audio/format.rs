use std::fmt;

#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Format {
    pub rate: usize,
    pub channels: usize,
    pub bits: usize,
    pub float: bool,
    pub block_align: usize,
}

impl fmt::Display for Format {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        let k = if self.float { "float" } else { "int" };
        write!(f, "{} Hz, {} ch, {} bit {}", self.rate, self.channels, self.bits, k)
    }
}

fn u16le(b: &[u8], o: usize) -> u16 {
    u16::from_le_bytes([b[o], b[o + 1]])
}
fn u32le(b: &[u8], o: usize) -> u32 {
    u32::from_le_bytes([b[o], b[o + 1], b[o + 2], b[o + 3]])
}

impl Format {
    // разбор WAVEFORMATEX / WAVEFORMATEXTENSIBLE
    pub fn parse(b: &[u8]) -> Result<Format, String> {
        if b.len() < 16 {
            return Err("short WAVEFORMATEX".into());
        }
        let tag = u16le(b, 0);
        let mut f = Format {
            channels: u16le(b, 2) as usize,
            rate: u32le(b, 4) as usize,
            block_align: u16le(b, 12) as usize,
            bits: u16le(b, 14) as usize,
            float: false,
        };
        match tag {
            1 => {}
            3 => f.float = true,
            0xFFFE => {
                if b.len() < 40 {
                    return Err("truncated WAVEFORMATEXTENSIBLE".into());
                }
                match u32le(b, 24) {
                    1 => {}
                    3 => f.float = true,
                    _ => return Err("unknown SubFormat".into()),
                }
            }
            _ => return Err(format!("unsupported format (tag=0x{tag:X})")),
        }
        if f.channels < 1 || f.rate < 8000 || f.block_align < 1 {
            return Err(format!("odd format: {f}"));
        }
        if f.float && f.bits != 32 {
            return Err("only float32 is supported".into());
        }
        if !f.float && f.bits != 16 && f.bits != 24 && f.bits != 32 {
            return Err(format!("unsupported bit depth {}", f.bits));
        }
        Ok(f)
    }

    #[inline]
    fn sample(&self, b: &[u8]) -> f32 {
        if self.float {
            f32::from_bits(u32le(b, 0))
        } else if self.bits == 16 {
            u16le(b, 0) as i16 as f32 / 32768.0
        } else if self.bits == 24 {
            let v = ((b[0] as u32) << 8 | (b[1] as u32) << 16 | (b[2] as u32) << 24) as i32 >> 8;
            v as f32 / 8388608.0
        } else {
            u32le(b, 0) as i32 as f32 / 2147483648.0
        }
    }

    pub fn decode_mono(&self, data: &[u8], frames: usize, dst: &mut Vec<f32>) {
        let bps = self.bits / 8;
        for i in 0..frames {
            let base = i * self.block_align;
            let mut s = 0f32;
            for c in 0..self.channels {
                s += self.sample(&data[base + c * bps..]);
            }
            dst.push(s / self.channels as f32);
        }
    }

    pub fn decode_channels(&self, data: &[u8], frames: usize, dst: &mut [Vec<f32>]) {
        let bps = self.bits / 8;
        for i in 0..frames {
            let base = i * self.block_align;
            for (c, d) in dst.iter_mut().enumerate().take(self.channels) {
                d.push(self.sample(&data[base + c * bps..]));
            }
        }
    }

    #[inline]
    fn put(&self, b: &mut [u8], v: f32) {
        let v = v.clamp(-1.0, 1.0);
        if self.float {
            b[..4].copy_from_slice(&v.to_bits().to_le_bytes());
        } else if self.bits == 16 {
            b[..2].copy_from_slice(&((v as f64 * 32767.0).round() as i16).to_le_bytes());
        } else if self.bits == 24 {
            let x = (v as f64 * 8388607.0).round() as i32;
            b[0] = x as u8;
            b[1] = (x >> 8) as u8;
            b[2] = (x >> 16) as u8;
        } else {
            b[..4].copy_from_slice(&((v as f64 * 2147483647.0).round() as i32).to_le_bytes());
        }
    }

    // моно во все каналы; dst вмещает len(mono)*block_align байт
    pub fn encode_mono(&self, mono: &[f32], dst: &mut [u8]) {
        let bps = self.bits / 8;
        for (i, &v) in mono.iter().enumerate() {
            let base = i * self.block_align;
            for c in 0..self.channels {
                self.put(&mut dst[base + c * bps..], v);
            }
        }
    }
}

pub fn mono_of(chs: &[Vec<f32>], dst: &mut Vec<f32>) {
    if chs.is_empty() {
        return;
    }
    let inv = 1.0 / chs.len() as f32;
    for i in 0..chs[0].len() {
        let s: f32 = chs.iter().map(|c| c[i]).sum();
        dst.push(s * inv);
    }
}

// многоканальный сигнал в левую и правую стороны
pub fn mix_lr(chs: &[Vec<f32>], l: &mut Vec<f32>, r: &mut Vec<f32>) {
    if chs.is_empty() {
        return;
    }
    for i in 0..chs[0].len() {
        let (mut a, mut b);
        match chs.len() {
            1 => {
                a = chs[0][i];
                b = chs[0][i];
            }
            2 => {
                a = chs[0][i];
                b = chs[1][i];
            }
            4 => {
                a = chs[0][i] + chs[2][i];
                b = chs[1][i] + chs[3][i];
            }
            6 | 8 => {
                let c = 0.707 * chs[2][i];
                a = chs[0][i] + c + chs[4][i];
                b = chs[1][i] + c + chs[5][i];
                if chs.len() == 8 {
                    a += chs[6][i];
                    b += chs[7][i];
                }
            }
            _ => {
                a = 0.0;
                b = 0.0;
                for (c, ch) in chs.iter().enumerate() {
                    if c % 2 == 0 {
                        a += ch[i];
                    } else {
                        b += ch[i];
                    }
                }
            }
        }
        l.push(a);
        r.push(b);
    }
}
