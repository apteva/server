package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The fixture audio is synthesized: harmonic "syllables" shaped by two
// formants, with pitch glides, pauses and one noise burst, so it looks like
// speech to level meters and voice-activity detectors without being anyone's
// voice. Generation is deterministic up to float rounding across CPUs,
// which the comparison below tolerates.

type mockSyllable struct {
	ms           int
	f0a, f0b     float64 // pitch glide, Hz
	f1, f2       float64 // formants, Hz
	amp          float64
	pause, noise bool
}

var mockCallerPhrase = []mockSyllable{
	{ms: 60, pause: true},
	{ms: 220, f0a: 140, f0b: 152, f1: 700, f2: 1200, amp: 0.9},
	{ms: 260, f0a: 150, f0b: 124, f1: 500, f2: 1700, amp: 1.0},
	{ms: 120, pause: true},
	{ms: 180, f0a: 134, f0b: 146, f1: 400, f2: 2100, amp: 0.8},
	{ms: 200, f0a: 146, f0b: 138, f1: 620, f2: 1100, amp: 0.85},
	{ms: 240, f0a: 138, f0b: 126, f1: 750, f2: 1250, amp: 0.9},
	{ms: 90, pause: true},
	{ms: 110, noise: true, amp: 0.35},
	{ms: 220, f0a: 132, f0b: 120, f1: 350, f2: 2300, amp: 0.8},
	{ms: 300, f0a: 126, f0b: 104, f1: 560, f2: 900, amp: 0.75},
	{ms: 200, pause: true},
}

var mockAgentPhrase = []mockSyllable{
	{ms: 80, pause: true},
	{ms: 200, f0a: 215, f0b: 232, f1: 820, f2: 1400, amp: 0.85},
	{ms: 240, f0a: 230, f0b: 208, f1: 450, f2: 2200, amp: 0.95},
	{ms: 100, pause: true},
	{ms: 90, noise: true, amp: 0.3},
	{ms: 220, f0a: 212, f0b: 224, f1: 650, f2: 1050, amp: 0.85},
	{ms: 280, f0a: 222, f0b: 190, f1: 380, f2: 1900, amp: 0.8},
	{ms: 180, pause: true},
}

func mockSynthesize(phrase []mockSyllable, rate int, seed uint32) []float64 {
	var out []float64
	noise := seed
	for _, syl := range phrase {
		n := syl.ms * rate / 1000
		attack, release := 30*rate/1000, 50*rate/1000
		env := func(i int) float64 {
			switch {
			case i < attack:
				return 0.5 - 0.5*math.Cos(math.Pi*float64(i)/float64(attack))
			case i > n-release:
				return 0.5 - 0.5*math.Cos(math.Pi*float64(n-i)/float64(release))
			}
			return 1
		}
		switch {
		case syl.pause:
			out = append(out, make([]float64, n)...)
		case syl.noise:
			prev := 0.0
			for i := 0; i < n; i++ {
				noise = noise*1103515245 + 12345
				x := float64(int32(noise>>16&0x7fff)-16384) / 16384
				out = append(out, syl.amp*env(i)*(x-prev)/2)
				prev = x
			}
		default:
			phases := make([]float64, 40)
			for i := 0; i < n; i++ {
				t := float64(i) / float64(n)
				f0 := syl.f0a + (syl.f0b-syl.f0a)*t
				sum := 0.0
				for k := 1; k < len(phases) && float64(k)*f0 < 3600; k++ {
					f := float64(k) * f0
					gain := 1/(1+math.Pow((f-syl.f1)/120, 2)) + 0.6/(1+math.Pow((f-syl.f2)/180, 2)) + 0.04
					phases[k] += 2 * math.Pi * f / float64(rate)
					sum += gain * math.Sin(phases[k])
				}
				out = append(out, syl.amp*env(i)*sum)
			}
		}
	}
	return out
}

func mockNormalize(signal []float64, peak float64) []int16 {
	max := 0.0
	for _, v := range signal {
		max = math.Max(max, math.Abs(v))
	}
	out := make([]int16, len(signal))
	for i, v := range signal {
		out[i] = int16(math.Round(v / max * peak))
	}
	return out
}

func mockGenerateFixtures() map[string]mockWAV {
	const rate = 8000
	caller := mockNormalize(mockSynthesize(mockCallerPhrase, rate, 7), 0.6*32767)
	agent := mockNormalize(mockSynthesize(mockAgentPhrase, rate, 11), 0.5*32767)
	gap := make([]int16, 300*rate/1000)
	timeline := len(caller) + len(gap) + len(agent)
	mono := make([]int16, 0, timeline)
	mono = append(append(append(mono, caller...), gap...), agent...)
	stereo := make([]int16, 2*timeline)
	for i := 0; i < timeline; i++ {
		if i < len(caller) {
			stereo[2*i] = caller[i]
		}
		if j := i - len(caller) - len(gap); j >= 0 {
			stereo[2*i+1] = agent[j]
		}
	}
	return map[string]mockWAV{
		"caller-speech-8k.wav":      {rate: rate, channels: 1, samples: caller},
		"call-recording-mono.wav":   {rate: rate, channels: 1, samples: mono},
		"call-recording-stereo.wav": {rate: rate, channels: 2, samples: stereo},
	}
}

func TestIntegrationMockFixturesAreCurrent(t *testing.T) {
	generated := mockGenerateFixtures()
	if os.Getenv("UPDATE_MOCK_FIXTURES") == "1" {
		for name, wav := range generated {
			if err := os.WriteFile(filepath.Join(integrationMockFixtureDir, name), mockWriteWAV(wav), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Skip("fixtures rewritten; rebuild to embed them")
	}
	entries, err := integrationMockFixtureFS.ReadDir(integrationMockFixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(generated) {
		t.Fatalf("embedded %d fixtures, generator makes %d", len(entries), len(generated))
	}
	for name, want := range generated {
		fixture, ok := mockFixtureByName(name)
		if !ok {
			t.Fatalf("fixture %s is not embedded", name)
		}
		got, err := mockReadWAV(fixture.data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.rate != want.rate || got.channels != want.channels || len(got.samples) != len(want.samples) {
			t.Fatalf("%s: %d Hz x%d, %d samples; generator %d Hz x%d, %d samples (UPDATE_MOCK_FIXTURES=1 rewrites)", name, got.rate, got.channels, len(got.samples), want.rate, want.channels, len(want.samples))
		}
		for i := range want.samples {
			if d := int(got.samples[i]) - int(want.samples[i]); d < -2 || d > 2 {
				t.Fatalf("%s: sample %d is %d, generator %d (UPDATE_MOCK_FIXTURES=1 rewrites)", name, i, got.samples[i], want.samples[i])
			}
		}
	}
}

func TestIntegrationMockCallerAudioLooksLikeSpeech(t *testing.T) {
	samples, err := mockCallerSamples("caller-speech-8k.wav", 8000)
	if err != nil {
		t.Fatal(err)
	}
	if ms := len(samples) * 1000 / 8000; ms < 2000 || ms > 3000 {
		t.Fatalf("caller audio lasts %d ms", ms)
	}
	loud, quiet := 0, 0
	for start := 0; start+160 <= len(samples); start += 160 {
		sum := 0.0
		for _, s := range samples[start : start+160] {
			sum += float64(s) * float64(s)
		}
		if math.Sqrt(sum/160) >= mockMediaSilenceRMS {
			loud++
		} else {
			quiet++
		}
	}
	if loud < 70 || quiet < 15 {
		t.Fatalf("caller audio has %d loud and %d quiet 20 ms frames; want speech with pauses", loud, quiet)
	}
	wide, err := mockCallerSamples("caller-speech-8k.wav", 16000)
	if err != nil {
		t.Fatal(err)
	}
	if len(wide) != 2*len(samples) {
		t.Fatalf("16 kHz caller audio has %d samples, want %d", len(wide), 2*len(samples))
	}
	if mockAudioFixtureKnown("call-recording-mono.wav") || !mockAudioFixtureKnown("silence") || mockAudioFixtureKnown("../main.go") {
		t.Fatal("caller audio fixture set is wrong")
	}
}

func TestIntegrationMockCodecsRoundTrip(t *testing.T) {
	for _, codec := range []string{"PCMU", "PCMA", "L16"} {
		for _, v := range []int16{0, 1, -1, 100, -100, 1000, -1000, 12000, -12000, 32767, -32768} {
			encoded := mockEncodeAudio([]int16{v}, codec)
			decoded, err := mockDecodeAudio(encoded, codec)
			if err != nil {
				t.Fatalf("%s %d: %v", codec, v, err)
			}
			tolerance := 0.0
			if codec != "L16" {
				// G.711 keeps about 4 significant bits of the mantissa.
				tolerance = math.Max(16, math.Abs(float64(v))/16)
			}
			if math.Abs(float64(decoded[0])-float64(v)) > tolerance {
				t.Fatalf("%s %d decodes to %d", codec, v, decoded[0])
			}
		}
	}
	// Known G.711 code points.
	if mockLinearToULaw(0) != 0xFF || mockULawToLinear(0xFF) != 0 || mockULawToLinear(0x00) != -32124 {
		t.Fatal("mu-law reference points are wrong")
	}
	if mockLinearToALaw(0) != 0xD5 || mockALawToLinear(0xD5) != 8 || mockALawToLinear(0x2A) != -32256 {
		t.Fatal("A-law reference points are wrong")
	}
	if _, err := mockDecodeAudio([]byte{1, 2, 3}, "L16"); err == nil {
		t.Fatal("odd L16 payload decoded")
	}
}

func TestIntegrationSimulationFixtureRoute(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleIntegrationSimulationFixture(rec, httptest.NewRequest(http.MethodGet, "/integration-simulation/fixtures/call-recording-stereo.wav", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("fixture GET = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	wav, err := mockReadWAV(rec.Body.Bytes())
	if err != nil || wav.channels != 2 {
		t.Fatalf("served stereo recording: %+v %v", wav.channels, err)
	}
	for _, path := range []string{"/integration-simulation/fixtures/nope.wav", "/integration-simulation/fixtures/..%2fmain.go", "/integration-simulation/fixtures/"} {
		rec := httptest.NewRecorder()
		s.handleIntegrationSimulationFixture(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	s.handleIntegrationSimulationFixture(rec, httptest.NewRequest(http.MethodPost, "/integration-simulation/fixtures/caller-speech-8k.wav", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/integration-simulation/fixtures/caller-speech-8k.wav", nil)
	req.Header.Set("Range", "bytes=0-43")
	s.handleIntegrationSimulationFixture(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != 44 {
		t.Fatalf("range GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
}
