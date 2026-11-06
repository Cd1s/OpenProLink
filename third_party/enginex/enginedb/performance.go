// Package enginedb writes the measured Engine OS 5.0.4 Database2 layout.
package enginedb

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unicode/utf8"

	"github.com/Cd1s/OpenProLink/third_party/enginex/rekordbox"
)

var big = binary.BigEndian
var little = binary.LittleEndian

func QCompress(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	_ = binary.Write(&out, big, uint32(len(raw)))
	w := zlib.NewWriter(&out)
	if _, e := w.Write(raw); e != nil {
		return nil, e
	}
	if e := w.Close(); e != nil {
		return nil, e
	}
	return out.Bytes(), nil
}

// QUncompress is bounded and rejects both trailing compressed data and incorrect lengths.
func QUncompress(blob []byte, limit int) ([]byte, error) {
	if limit < 0 {
		return nil, fmt.Errorf("qCompress: negative size limit")
	}
	if len(blob) < 4 {
		return nil, fmt.Errorf("qCompress: missing length")
	}
	n := uint64(big.Uint32(blob))
	if n > uint64(limit) {
		return nil, fmt.Errorf("qCompress: size limit")
	}
	r := bytes.NewReader(blob[4:])
	z, e := zlib.NewReader(r)
	if e != nil {
		return nil, e
	}
	raw, e := io.ReadAll(io.LimitReader(z, int64(n)+1))
	ce := z.Close()
	if e != nil {
		return nil, e
	}
	if ce != nil {
		return nil, ce
	}
	if uint64(len(raw)) != n || r.Len() != 0 {
		return nil, fmt.Errorf("qCompress: length/trailing data mismatch")
	}
	return raw, nil
}

type Performance struct {
	TrackData, Overview, Beats, QuickCues, Loops []byte
	ActiveLoops                                  uint32
	Complete                                     bool
}

func put(b *bytes.Buffer, order binary.ByteOrder, v any) { _ = binary.Write(b, order, v) }
func label(b *bytes.Buffer, s string) { // Engine uses an 8-bit UTF-8 byte length
	if len(s) > 255 {
		s = s[:255]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	b.WriteByte(byte(len(s)))
	b.WriteString(s)
}

// EncodePerformance never synthesizes cues. Missing hot slots have position -1;
// memory cues stay in BridgeAnalysis, with the first used as main cue. Engine's
// native quickCues has exactly eight slots; appending memory cues would produce
// an unsupported blob. BridgeAnalysis also retains labels beyond 255 UTF-8 bytes.
// beatPositions returns each beat's sample offset. PQTZ times are whole
// milliseconds, so using them directly gives a 130 BPM track spacings of 461 and
// 462 ms, and Engine's per-beat BPM then jumps between 130.15 and 129.87. Beats in
// a constant-BPM run are instead placed at one exact step from the run's first
// beat. The step is 60/BPM unless the 0.01-precision BPM would leave the last beat
// over 1 ms from its exported time; a long run then uses the step between its end
// times, because at 130 BPM that rounding alone can reach 18 ms over 1000 beats.
func beatPositions(beats []rekordbox.Beat, sampleRate float64) []float64 {
	positions := make([]float64, len(beats))
	for start := 0; start < len(beats); {
		end := start + 1
		for end < len(beats) && beats[start].BPM > 0 && math.Abs(beats[end].BPM-beats[start].BPM) < 0.005 {
			end++
		}
		first := float64(beats[start].TimeMS)
		stepMS := 0.0
		if beats[start].BPM > 0 && end-start > 1 {
			stepMS = 60000 / beats[start].BPM
			span := float64(end - start - 1)
			last := float64(beats[end-1].TimeMS)
			if math.Abs(first+span*stepMS-last) > 1 {
				stepMS = (last - first) / span
			}
		}
		for i := start; i < end; i++ {
			positions[i] = (first + float64(i-start)*stepMS) * sampleRate / 1000
		}
		start = end
	}
	return positions
}

func EncodePerformance(t rekordbox.Track, a *rekordbox.Analysis) (Performance, error) {
	if a == nil {
		a = &rekordbox.Analysis{}
	}
	sr := t.SampleRate
	if sr == 0 {
		sr = 44100
	}
	if sr < 1 || t.Duration < 0 || math.IsNaN(t.Duration) || math.IsInf(t.Duration, 0) {
		return Performance{}, fmt.Errorf("invalid track duration/sample rate")
	}
	sampleCount := t.SampleCount
	if sampleCount == 0 {
		sampleCount = uint64(math.Round(t.Duration * float64(sr)))
	}
	if sampleCount == 0 {
		// Short exports may omit duration/sample count but still contain analysis.
		sampleCount = 1024
		if len(a.Beats) > 0 {
			sampleCount = max(sampleCount, uint64(a.Beats[len(a.Beats)-1].TimeMS)*uint64(sr)/1000+1)
		}
		for _, w := range a.Waveforms {
			if w.EntryBytes > 0 {
				sampleCount = max(sampleCount, uint64(len(w.Data))/uint64(w.EntryBytes))
			}
		}
	}
	samples := float64(sampleCount)
	p := Performance{}
	var track bytes.Buffer
	put(&track, big, float64(sr))
	put(&track, big, sampleCount)
	key := uint32(0)
	if k, ok := EngineKey(t.Key).(int); ok {
		key = uint32(k)
	}
	put(&track, big, key)
	for i := 0; i < 3; i++ {
		put(&track, big, float64(0))
	}
	var e error
	p.TrackData, e = QCompress(track.Bytes())
	if e != nil {
		return p, e
	}
	if len(a.Beats) > 0 {
		positions := beatPositions(a.Beats, float64(sr))
		var b bytes.Buffer
		put(&b, big, float64(sr))
		put(&b, big, samples)
		b.WriteByte(1)
		for grid := 0; grid < 2; grid++ {
			put(&b, big, uint64(len(a.Beats)))
			var previous uint32
			for i, beat := range a.Beats {
				if i > 0 && beat.TimeMS <= previous {
					return p, fmt.Errorf("non-increasing beatgrid")
				}
				previous = beat.TimeMS
				put(&b, little, positions[i])
				put(&b, little, int64(i)+int64(a.Beats[0].Number)-1)
				next := uint32(1)
				if i == len(a.Beats)-1 {
					next = 0
				}
				put(&b, little, next)
				put(&b, little, uint32(0))
			}
		}
		b.WriteByte(0)
		p.Beats, e = QCompress(b.Bytes())
		if e != nil {
			return p, e
		}
	}
	var wave *rekordbox.Waveform
	priority := map[string]int{"PWAV": 1, "PWV2": 1, "PWV3": 2, "PWV4": 3, "PWV5": 4, "PWV7": 5}
	for i := range a.Waveforms {
		w := &a.Waveforms[i]
		if len(w.Data) > 0 && (wave == nil || priority[w.Tag] > priority[wave.Tag]) {
			wave = w
		}
	}
	if wave != nil {
		if wave.EntryBytes == 0 || uint64(wave.EntryBytes) > uint64(len(wave.Data)) || uint64(len(wave.Data))%uint64(wave.EntryBytes) != 0 {
			return p, fmt.Errorf("invalid waveform entries")
		}
		var b bytes.Buffer
		const bins = 1024
		put(&b, big, uint64(bins))
		put(&b, big, uint64(bins))
		put(&b, big, samples/bins)
		n := uint64(len(wave.Data)) / uint64(wave.EntryBytes)
		// Preserve peaks when downsampling. PWV5 uses 5-bit height and 3-bit RGB.
		for i := 0; i < bins; i++ {
			lo, hi := uint64(i)*n/bins, uint64(i+1)*n/bins
			if hi <= lo {
				hi = lo + 1
			}
			var peak [3]byte
			for j := lo; j < hi && j < n; j++ {
				v := waveRGB(*wave, int(j))
				for c := 0; c < 3; c++ {
					if v[c] > peak[c] {
						peak[c] = v[c]
					}
				}
			}
			b.Write(peak[:])
		}
		b.Write([]byte{0, 0, 0})
		p.Overview, e = QCompress(b.Bytes())
		if e != nil {
			return p, e
		}
	}
	var hot [8]*rekordbox.Cue
	var memory []rekordbox.Cue
	var memoryLoops []rekordbox.Cue
	for i := range a.Cues {
		c := &a.Cues[i]
		if c.HotCue > 8 {
			return p, fmt.Errorf("unsupported hot cue slot %d", c.HotCue)
		}
		if c.HotCue > 0 {
			hot[c.HotCue-1] = c
		} else {
			memory = append(memory, *c)
			if c.Loop {
				memoryLoops = append(memoryLoops, *c)
			}
		}
	}
	var q bytes.Buffer
	put(&q, big, uint64(8))
	writeCue := func(c *rekordbox.Cue) {
		if c == nil {
			label(&q, "")
			put(&q, big, float64(-1))
			q.Write([]byte{0, 0, 0, 0})
			return
		}
		label(&q, c.Name)
		put(&q, big, float64(c.TimeMS)*float64(sr)/1000)
		color := cueColor(*c)
		q.Write(color[:])
	}
	for _, c := range hot {
		writeCue(c)
	}
	main := float64(-1)
	if len(memory) > 0 {
		main = float64(memory[0].TimeMS) * float64(sr) / 1000
	}
	put(&q, big, main)
	q.WriteByte(0)
	put(&q, big, main)
	p.QuickCues, e = QCompress(q.Bytes())
	if e != nil {
		return p, e
	}
	var loops bytes.Buffer
	loops.WriteByte(8)
	loops.Write(make([]byte, 7))
	writeLoop := func(c *rekordbox.Cue) {
		if c == nil || !c.Loop {
			label(&loops, "")
			put(&loops, little, float64(-1))
			put(&loops, little, float64(-1))
			loops.Write(make([]byte, 6))
			return
		}
		label(&loops, c.Name)
		put(&loops, little, float64(c.TimeMS)*float64(sr)/1000)
		put(&loops, little, float64(c.EndMS)*float64(sr)/1000)
		loops.Write([]byte{1, 1})
		color := cueColor(*c)
		loops.Write(color[:])
	}
	// Memory loops fill unused loop slots, never the independent Hot Cue slots.
	loopSlots := hot
	for _, c := range memoryLoops {
		for i, existing := range loopSlots {
			if existing == nil || !existing.Loop {
				copy := c
				loopSlots[i] = &copy
				break
			}
		}
	}
	for i, c := range loopSlots {
		writeLoop(c)
		if c != nil && c.Loop && c.Active {
			p.ActiveLoops |= 1 << uint(i)
		}
	}
	p.Loops = loops.Bytes()
	p.Complete = len(a.Beats) > 0 && wave != nil
	return p, nil
}

// Older PCOB entries have implicit Pioneer pad colors, not transparent pads.
// Explicit PCP2 RGB (including opaque black) takes precedence over the palette.
func cueColor(c rekordbox.Cue) [4]byte {
	if c.Color != [4]byte{} {
		return [4]byte{255, c.Color[1], c.Color[2], c.Color[3]}
	}
	palette := [9][4]byte{{}, {255, 255, 0, 127}, {255, 255, 0, 0}, {255, 255, 127, 0}, {255, 255, 255, 0}, {255, 0, 255, 0}, {255, 0, 255, 255}, {255, 0, 0, 255}, {255, 127, 0, 255}}
	if c.ColorID > 0 && int(c.ColorID) < len(palette) {
		return palette[c.ColorID]
	}
	if c.Loop {
		return [4]byte{255, 255, 127, 0}
	}
	return [4]byte{255, 0, 255, 0}
}

func waveRGB(w rekordbox.Waveform, i int) [3]byte {
	offset := uint64(i) * uint64(w.EntryBytes)
	if i < 0 || w.EntryBytes == 0 || offset >= uint64(len(w.Data)) || uint64(w.EntryBytes) > uint64(len(w.Data))-offset {
		return [3]byte{}
	}
	p := w.Data[int(offset):]
	switch w.Tag {
	case "PWV7":
		if w.EntryBytes >= 3 {
			return [3]byte{p[0], p[1], p[2]}
		}
	case "PWV5":
		if w.EntryBytes >= 2 {
			v := big.Uint16(p)
			height := float64((v>>2)&31) / 31
			return [3]byte{byte(float64((v>>13)&7) * 255 / 7 * height), byte(float64((v>>10)&7) * 255 / 7 * height), byte(float64((v>>7)&7) * 255 / 7 * height)}
		}
	case "PWV4":
		if w.EntryBytes >= 6 {
			return [3]byte{p[3], p[4], p[5]}
		}
	}
	v := byte(int(p[0]&31) * 255 / 31)
	return [3]byte{v, v, v}
}
