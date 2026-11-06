package h264

import (
	"os"
	"testing"
)

func TestTestdataFrameCount(t *testing.T) {
	b, err := os.ReadFile("../../cmd/fakecam/testdata/test.h264")
	if err != nil {
		t.Skip()
	}
	if n := len(SplitAccessUnits(b)); n != 120 {
		t.Fatalf("got %d AUs, want 120 (4 s at 30 fps)", n)
	}
}
