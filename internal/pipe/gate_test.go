package pipe

import (
	"sync/atomic"
	"testing"
)

func TestGate(t *testing.T) {
	var open atomic.Bool
	open.Store(true)
	e := &Engine{gate: 1, Open: &open, ob: make([]float32, 256)}
	fill := func() {
		for i := range e.ob {
			e.ob[i] = 0.5
		}
	}
	fill()
	e.applyGate()
	if e.ob[255] != 0.5 {
		t.Fatal("открытый микрофон изменён")
	}
	open.Store(false)
	for k := 0; k < 3; k++ { // 3 блока = 16 мс, затухание 10 мс
		fill()
		e.applyGate()
	}
	if e.ob[255] != 0 {
		t.Fatalf("не закрылся: %v", e.ob[255])
	}
	open.Store(true)
	fill()
	e.applyGate()
	if e.ob[0] <= 0 || e.ob[0] > 0.01 || e.ob[255] < 0.2 {
		t.Fatalf("открытие не плавное: %v .. %v", e.ob[0], e.ob[255])
	}
}
