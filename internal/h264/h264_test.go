package h264

import (
	"bytes"
	"os"
	"testing"
)

// A real SPS from a 1440x1080 libx264 High profile stream (truncated is fine
// for the codec-string bytes).
var sps = []byte{0x67, 0x64, 0x00, 0x28, 0xac, 0xd9}

func TestNALUnits(t *testing.T) {
	b := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 0, 1, 0x65, 4, 5}
	nals := NALUnits(b)
	if len(nals) != 3 {
		t.Fatalf("got %d NALs", len(nals))
	}
	want := [][]byte{{0x67, 1, 2}, {0x68, 3}, {0x65, 4, 5}}
	for i := range want {
		if !bytes.Equal(nals[i], want[i]) {
			t.Errorf("nal %d = %x want %x", i, nals[i], want[i])
		}
	}
	key, hasSPS := Info(b)
	if !key || !hasSPS {
		t.Errorf("Info = %v %v", key, hasSPS)
	}
}

func TestSplitAccessUnitsSynthetic(t *testing.T) {
	sc := []byte{0, 0, 0, 1}
	var b []byte
	add := func(n ...byte) { b = append(b, sc...); b = append(b, n...) }
	add(0x67, 1)       // SPS
	add(0x68, 2)       // PPS
	add(0x65, 0x88, 3) // IDR, first_mb=0
	add(0x41, 0x9a, 4) // P, first_mb=0
	add(0x41, 0x9a, 5) // P
	aus := SplitAccessUnits(b)
	if len(aus) != 3 {
		t.Fatalf("got %d AUs", len(aus))
	}
	if k, s := Info(aus[0]); !k || !s {
		t.Errorf("AU0 key=%v sps=%v", k, s)
	}
	if k, _ := Info(aus[1]); k {
		t.Errorf("AU1 should not be key")
	}
}

func TestSplitAccessUnitsTestdata(t *testing.T) {
	b, err := os.ReadFile("../../cmd/fakecam/testdata/test.h264")
	if err != nil {
		t.Skip("testdata missing:", err)
	}
	aus := SplitAccessUnits(b)
	if len(aus) < 30 {
		t.Fatalf("only %d AUs", len(aus))
	}
	if k, s := Info(aus[0]); !k || !s {
		t.Fatalf("first AU key=%v sps=%v", k, s)
	}
	keys := 0
	for _, au := range aus {
		if k, _ := Info(au); k {
			keys++
		}
	}
	if keys < 2 {
		t.Fatalf("keys = %d", keys)
	}
}
