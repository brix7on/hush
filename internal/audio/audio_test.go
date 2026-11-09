package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestResampleSine(t *testing.T) {
	for _, c := range [][2]int{{44100, 48000}, {96000, 48000}, {48000, 44100}, {48000, 48000}, {16000, 48000}} {
		in, out := c[0], c[1]
		r := NewResampler(in, out, true)
		f := 1000.0
		var res []float32
		chunk := make([]float32, in/100)
		for s := 0; s < 3; s++ { // 3 секунды кусками по 10 мс
			for k := 0; k < 100; k++ {
				for i := range chunk {
					n := float64(s*in + k*len(chunk) + i)
					chunk[i] = float32(0.5 * math.Sin(2*math.Pi*f*n/float64(in)))
				}
				res = r.Process(chunk, res)
			}
		}
		// ищем лучшую фазовую задержку (в выходных отсчётах) и считаем ошибку
		best := math.Inf(1)
		for d := -60.0; d <= 60.0; d += 0.05 {
			var e, p float64
			for i := out; i < len(res)-out; i++ {
				ideal := 0.5 * math.Sin(2*math.Pi*f*(float64(i)-d)/float64(out))
				e += (float64(res[i]) - ideal) * (float64(res[i]) - ideal)
				p += ideal * ideal
			}
			if v := e / p; v < best {
				best = v
			}
		}
		snr := -10 * math.Log10(best)
		t.Logf("%d→%d: SNR %.1f дБ, выход %d отсчётов", in, out, snr, len(res))
		if snr < 50 {
			t.Errorf("%d→%d: слабое качество %.1f дБ", in, out, snr)
		}
		want := 3 * out
		if len(res) < want-200 || len(res) > want+10 {
			t.Errorf("%d→%d: ожидалось ~%d отсчётов, получено %d", in, out, want, len(res))
		}
	}
}

func TestFormats(t *testing.T) {
	// WAVEFORMATEXTENSIBLE float32, 2 канала, 48 кГц
	b := make([]byte, 40)
	binary.LittleEndian.PutUint16(b[0:], 0xFFFE)
	binary.LittleEndian.PutUint16(b[2:], 2)
	binary.LittleEndian.PutUint32(b[4:], 48000)
	binary.LittleEndian.PutUint16(b[12:], 8)
	binary.LittleEndian.PutUint16(b[14:], 32)
	binary.LittleEndian.PutUint16(b[16:], 22)
	binary.LittleEndian.PutUint32(b[24:], 3)
	f, err := ParseWaveFormat(b)
	if err != nil || !f.Float || f.Channels != 2 || f.Rate != 48000 {
		t.Fatalf("разбор: %v %v", f, err)
	}
	for _, ff := range []Format{
		{Rate: 48000, Channels: 2, Bits: 16, BlockAlign: 4},
		{Rate: 48000, Channels: 2, Bits: 24, BlockAlign: 6},
		{Rate: 48000, Channels: 1, Bits: 32, BlockAlign: 4},
		{Rate: 48000, Channels: 2, Bits: 32, Float: true, BlockAlign: 8},
	} {
		mono := []float32{0, 0.5, -0.5, 0.999, -1, 0.1234}
		buf := make([]byte, len(mono)*ff.BlockAlign)
		ff.EncodeMono(mono, buf)
		got := ff.DecodeMono(buf, len(mono), nil)
		for i := range mono {
			if math.Abs(float64(got[i]-mono[i])) > 1e-3 {
				t.Errorf("%v: [%d] %v != %v", ff, i, got[i], mono[i])
			}
		}
	}
}
