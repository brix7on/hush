use std::ops::{Add, AddAssign, Mul, MulAssign, Sub};

#[derive(Clone, Copy, Default, Debug, PartialEq)]
pub struct C64 {
    pub re: f64,
    pub im: f64,
}

impl C64 {
    pub const ZERO: C64 = C64 { re: 0.0, im: 0.0 };
    #[inline]
    pub fn new(re: f64, im: f64) -> Self {
        C64 { re, im }
    }
    #[inline]
    pub fn conj(self) -> Self {
        C64 { re: self.re, im: -self.im }
    }
    #[inline]
    pub fn norm2(self) -> f64 {
        self.re * self.re + self.im * self.im
    }
    #[inline]
    pub fn scale(self, s: f64) -> Self {
        C64 { re: self.re * s, im: self.im * s }
    }
}

impl Add for C64 {
    type Output = C64;
    #[inline]
    fn add(self, o: C64) -> C64 {
        C64 { re: self.re + o.re, im: self.im + o.im }
    }
}
impl Sub for C64 {
    type Output = C64;
    #[inline]
    fn sub(self, o: C64) -> C64 {
        C64 { re: self.re - o.re, im: self.im - o.im }
    }
}
impl Mul for C64 {
    type Output = C64;
    #[inline]
    fn mul(self, o: C64) -> C64 {
        C64 { re: self.re * o.re - self.im * o.im, im: self.re * o.im + self.im * o.re }
    }
}
impl AddAssign for C64 {
    #[inline]
    fn add_assign(&mut self, o: C64) {
        self.re += o.re;
        self.im += o.im;
    }
}
impl MulAssign<f64> for C64 {
    #[inline]
    fn mul_assign(&mut self, s: f64) {
        self.re *= s;
        self.im *= s;
    }
}

// radix-2 БПФ, размер — степень двойки
pub struct Fft {
    n: usize,
    rev: Vec<usize>,
    tw: Vec<C64>,
}

impl Fft {
    pub fn new(n: usize) -> Self {
        assert!(n.is_power_of_two());
        let lg = n.trailing_zeros();
        let rev = (0..n)
            .map(|i| if lg == 0 { 0 } else { ((i as u32).reverse_bits() >> (32 - lg)) as usize })
            .collect();
        let tw = (0..n / 2)
            .map(|i| {
                let a = -2.0 * std::f64::consts::PI * i as f64 / n as f64;
                C64::new(a.cos(), a.sin())
            })
            .collect();
        Fft { n, rev, tw }
    }

    pub fn len(&self) -> usize {
        self.n
    }

    fn run(&self, a: &mut [C64], inv: bool) {
        let n = self.n;
        for i in 0..n {
            let j = self.rev[i];
            if j > i {
                a.swap(i, j);
            }
        }
        let mut size = 2;
        while size <= n {
            let half = size >> 1;
            let step = n / size;
            let mut start = 0;
            while start < n {
                for k in 0..half {
                    let mut w = self.tw[k * step];
                    if inv {
                        w = w.conj();
                    }
                    let u = a[start + k];
                    let v = a[start + k + half] * w;
                    a[start + k] = u + v;
                    a[start + k + half] = u - v;
                }
                start += size;
            }
            size <<= 1;
        }
    }

    pub fn forward(&self, a: &mut [C64]) {
        self.run(a, false);
    }

    pub fn inverse(&self, a: &mut [C64]) {
        self.run(a, true);
        let s = 1.0 / self.n as f64;
        for v in a.iter_mut() {
            *v *= s;
        }
    }
}
