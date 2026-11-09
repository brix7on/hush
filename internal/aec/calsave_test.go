package aec

import (
	"encoding/json"
	"testing"
)

// Подстроенная модель, сохранённая и загруженная заново, помнит исходную калибровку
// и не может «уплыть» дальше пределов от неё (раньше пределы считались от сохранённого).
func TestCalibSaveKeepsOrigin(t *testing.T) {
	s := NewSuppressor(48000)
	w := make([]float64, s.nb)
	for i := range w {
		w[i] = 1
	}
	s.SetCalib(Calib{Rate: 48000, D: 10, W: w, Found: true})
	for round := 0; round < 5; round++ {
		c := s.Calib()
		for i := range c.W {
			c.W[i] *= 0.5 // как будто подстройка сильно занизила
		}
		b, _ := json.Marshal(c)
		var c2 Calib
		json.Unmarshal(b, &c2)
		s = NewSuppressor(48000)
		s.SetCalib(c2)
	}
	if s.w0[3] != 1 {
		t.Fatalf("исходная калибровка потерялась: w0=%v", s.w0[3])
	}
}
