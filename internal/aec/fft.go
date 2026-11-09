package aec

import (
	"math"
	"math/bits"
	"math/cmplx"
)

// fft — простой итеративный комплексный БПФ (radix-2), размер — степень двойки.
type fft struct {
	n   int
	rev []int
	tw  []complex128
}

func newFFT(n int) *fft {
	f := &fft{n: n, rev: make([]int, n), tw: make([]complex128, n/2)}
	lg := bits.TrailingZeros(uint(n))
	for i := 0; i < n; i++ {
		f.rev[i] = int(bits.Reverse32(uint32(i)) >> (32 - uint(lg)))
	}
	for i := range f.tw {
		a := -2 * math.Pi * float64(i) / float64(n)
		f.tw[i] = complex(math.Cos(a), math.Sin(a))
	}
	return f
}

func (f *fft) run(a []complex128, inv bool) {
	n := f.n
	for i := 0; i < n; i++ {
		if j := f.rev[i]; j > i {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		step := n / size
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				w := f.tw[k*step]
				if inv {
					w = cmplx.Conj(w)
				}
				u := a[start+k]
				v := a[start+k+half] * w
				a[start+k] = u + v
				a[start+k+half] = u - v
			}
		}
	}
}

func (f *fft) forward(a []complex128) { f.run(a, false) }

func (f *fft) inverse(a []complex128) {
	f.run(a, true)
	s := complex(1/float64(f.n), 0)
	for i := range a {
		a[i] *= s
	}
}
