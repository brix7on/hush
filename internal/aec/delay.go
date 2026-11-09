package aec

import (
	"math"
	"math/cmplx"
)

// EstimateDelay оценивает, насколько mic «опаздывает» относительно ref (GCC-PHAT): mic[n] ≈ ref[n-lag].

func EstimateDelay(ref, mic []float32, maxLag int) (lag int, conf float64) {
	n := len(ref)
	if len(mic) < n {
		n = len(mic)
	}
	N := 1
	for N < 2*n {
		N <<= 1
	}
	f := newFFT(N)
	X := make([]complex128, N)
	Y := make([]complex128, N)
	for i := 0; i < n; i++ {
		X[i] = complex(float64(ref[i]), 0)
		Y[i] = complex(float64(mic[i]), 0)
	}
	f.forward(X)
	f.forward(Y)
	for k := 0; k < N; k++ {
		c := Y[k] * cmplx.Conj(X[k])
		if m := cmplx.Abs(c); m > 1e-20 {
			c /= complex(m, 0)
		} else {
			c = 0
		}
		X[k] = c
	}
	f.inverse(X)
	var peak, sum float64
	cnt := 0
	for k := -maxLag; k <= maxLag; k++ {
		v := math.Abs(real(X[(k+N)%N]))
		sum += v
		cnt++
		if v > peak {
			peak, lag = v, k
		}
	}
	return lag, peak / (sum/float64(cnt) + 1e-18)
}
