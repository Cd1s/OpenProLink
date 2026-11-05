package main

import (
	"encoding/binary"
	"math"

	"github.com/Cd1s/OpenCDJ-3000/third_party/enginex/testfixture"
)

// drumTrack synthesizes 32 bars of 120 BPM drums (intro, groove, breakdown with
// a snare roll, drop with bass) and derives its ANLZ files from the same bands,
// so the waveform, beatgrid and audio a browser plays all agree.
func drumTrack() (wav, dat, ext, ex2 []byte) {
	const (
		rate  = 22050
		bpm   = 120
		bars  = 32
		beat  = rate * 60 / bpm
		total = bars * 4 * beat
		cols  = 75 // waveform columns per second
	)
	low, mid, high := make([]float64, total), make([]float64, total), make([]float64, total)
	seed := uint32(1)
	noise := func() float64 { seed = seed*1664525 + 1013904223; return float64(int32(seed)) / math.MaxInt32 }
	add := func(dst []float64, at, n int, f func(t float64) float64) {
		for i := 0; i < n && at+i < total; i++ {
			dst[at+i] += f(float64(i) / rate)
		}
	}
	for b := 0; b < bars; b++ {
		breakdown := b >= 12 && b < 16
		for q := 0; q < 4; q++ {
			at := (b*4 + q) * beat
			if !breakdown {
				phase := 0.0
				add(low, at, rate/4, func(t float64) float64 {
					phase += 2 * math.Pi * (50 + 70*math.Exp(-t*30)) / rate
					return math.Sin(phase) * math.Exp(-t*9)
				})
			}
			if b >= 8 && !breakdown && q%2 == 1 {
				add(mid, at, rate/5, func(t float64) float64 {
					return (0.55*noise() + 0.45*math.Sin(2*math.Pi*185*t)) * math.Exp(-t*18)
				})
			}
			if b >= 4 && !breakdown {
				prev := 0.0
				for h := 0; h < 2; h++ {
					gain := 0.25
					if h == 1 {
						gain = 0.45 // open hat on the offbeat
					}
					add(high, at+h*beat/2, rate/16, func(t float64) float64 {
						n := noise()
						v := (n - prev) * gain * math.Exp(-t*70)
						prev = n
						return v
					})
				}
			}
			if b >= 16 {
				add(low, at+beat/2, beat/2, func(t float64) float64 {
					return 0.6 * (math.Sin(2*math.Pi*55*t) + 0.3*math.Sin(2*math.Pi*110*t)) * math.Exp(-t*5)
				})
			}
			if breakdown {
				add(mid, at, beat, func(t float64) float64 {
					return 0.12 * (math.Sin(2*math.Pi*220*t) + math.Sin(2*math.Pi*277.2*t) + math.Sin(2*math.Pi*329.6*t))
				})
				if b == 15 {
					for s := 0; s < 4; s++ {
						add(mid, at+s*beat/4, beat/4, func(t float64) float64 {
							return 0.25 * float64(q*4+s+4) / 20 * noise() * math.Exp(-t*25)
						})
					}
				}
			}
		}
	}

	wav = make([]byte, 44+2*total)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], rate)
	binary.LittleEndian.PutUint32(wav[28:], rate*2)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(2*total))
	for i := range total {
		v := math.Max(-1, math.Min(1, 0.7*low[i]+0.5*mid[i]+0.6*high[i]))
		binary.LittleEndian.PutUint16(wav[44+2*i:], uint16(int16(v*30000)))
	}

	// Per-column band peaks, scaled so each band's loudest column is 240.
	n := total * cols / rate
	peaks := make([][3]float64, n)
	var top [3]float64
	for c := range n {
		for i := c * rate / cols; i < (c+1)*rate/cols; i++ {
			for k, band := range [][]float64{low, mid, high} {
				peaks[c][k] = math.Max(peaks[c][k], math.Abs(band[i]))
			}
		}
		for k := range top {
			top[k] = math.Max(top[k], peaks[c][k])
		}
	}
	pwv7 := make([]byte, 12+3*n) // entries are [mid, high, low]
	pwv5 := make([]byte, 12+2*n) // 3-bit RGB and 5-bit height per column
	binary.BigEndian.PutUint32(pwv7, 3)
	binary.BigEndian.PutUint32(pwv7[4:], uint32(n))
	binary.BigEndian.PutUint32(pwv5, 2)
	binary.BigEndian.PutUint32(pwv5[4:], uint32(n))
	for c, p := range peaks {
		var v [3]float64
		for k := range v {
			v[k] = p[k] / top[k]
		}
		pwv7[12+3*c], pwv7[13+3*c], pwv7[14+3*c] = byte(v[1]*240), byte(v[2]*240), byte(v[0]*240)
		height := math.Min(1, math.Max(v[0], math.Max(v[1], v[2])))
		rgb := uint16(v[0]*7)<<13 | uint16(v[1]*7)<<10 | uint16(v[2]*7)<<7 | uint16(height*31)<<2
		binary.BigEndian.PutUint16(pwv5[12+2*c:], rgb)
	}
	beats := make([]byte, 12+8*bars*4)
	binary.BigEndian.PutUint32(beats[4:], 0x80000)
	binary.BigEndian.PutUint32(beats[8:], bars*4)
	for i := range bars * 4 {
		p := beats[12+8*i:]
		binary.BigEndian.PutUint16(p, uint16(i%4+1))
		binary.BigEndian.PutUint16(p[2:], bpm*100)
		binary.BigEndian.PutUint32(p[4:], uint32(i*60000/bpm))
	}
	dat = testfixture.PMAI(testfixture.Section("PQTZ", 24, beats))
	ext = testfixture.PMAI(testfixture.Section("PWV5", 24, pwv5))
	ex2 = testfixture.PMAI(testfixture.Section("PWV7", 24, pwv7))
	return wav, dat, ext, ex2
}
