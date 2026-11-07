// Package h264 has the few H.264 Annex-B helpers the fake helper (cmd/fakecam)
// needs to replay a recorded stream: NAL scanning, keyframe/SPS detection and
// access-unit splitting. The server itself does not parse H.264: the helper
// marks keyframes and SPS in the video header.
package h264

// NAL unit types used here.
const (
	NALSlice = 1
	NALIDR   = 5
	NALSEI   = 6
	NALSPS   = 7
	NALPPS   = 8
	NALAUD   = 9
)

// NALUnits returns the NAL units of an Annex-B buffer, without start codes.
func NALUnits(b []byte) [][]byte {
	var out [][]byte
	start := -1
	i := 0
	for i+2 < len(b) {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				end := i
				if end > start && b[end-1] == 0 { // 4-byte start code
					end--
				}
				out = append(out, b[start:end])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// NALType returns the type of a NAL unit (without start code).
func NALType(nal []byte) int {
	if len(nal) == 0 {
		return -1
	}
	return int(nal[0] & 0x1f)
}

// Info reports whether an access unit contains an IDR slice and an SPS.
func Info(au []byte) (key, hasSPS bool) {
	for _, n := range NALUnits(au) {
		switch NALType(n) {
		case NALIDR:
			key = true
		case NALSPS:
			hasSPS = true
		}
	}
	return
}

// SplitAccessUnits splits an Annex-B stream into access units. A new AU
// starts at an AUD, or at the first non-VCL NAL (SPS/PPS/SEI) or VCL NAL that
// follows a VCL NAL. This is enough for single-slice streams such as the
// ones produced by the Raspberry Pi encoder or libx264 with default settings.
// Each returned AU is a fresh Annex-B buffer with 4-byte start codes.
func SplitAccessUnits(b []byte) [][]byte {
	var aus [][]byte
	var cur []byte
	sawVCL := false
	flush := func() {
		if len(cur) > 0 {
			aus = append(aus, cur)
		}
		cur = nil
		sawVCL = false
	}
	for _, n := range NALUnits(b) {
		t := NALType(n)
		vcl := t >= 1 && t <= 5
		if t == NALAUD || (sawVCL && (!vcl || firstMBZero(n))) {
			flush()
		}
		cur = append(cur, 0, 0, 0, 1)
		cur = append(cur, n...)
		if vcl {
			sawVCL = true
		}
	}
	flush()
	return aus
}

// firstMBZero reports whether a slice NAL starts a new picture
// (first_mb_in_slice == 0, i.e. the first bit of the slice header is 1 in
// ue(v) coding).
func firstMBZero(nal []byte) bool {
	t := NALType(nal)
	if t < 1 || t > 5 || len(nal) < 2 {
		return false
	}
	return nal[1]&0x80 != 0
}
