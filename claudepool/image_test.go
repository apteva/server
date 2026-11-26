package claudepool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apteva/server/claudepool/claudetest"
)

// solidPNG is a width×height PNG filled with one colour.
func solidPNG(t *testing.T, width, height int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pngImage is a small PNG image as a caller sends it; shade tells images
// apart.
func pngImage(t *testing.T, shade uint8) Image {
	t.Helper()
	return Image{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(solidPNG(t, 2, 2, color.RGBA{shade, 0, 0, 255}))}
}

// shown is how the fake CLI shows an image that reached it unchanged.
func shown(img Image) string { return claudetest.ImageMarker("image/png", img.Data) }

func TestUserImagesReachClaude(t *testing.T) {
	tp := newTestPool(t, Limits{})
	red, blue := pngImage(t, 1), pngImage(t, 2)
	req := Request{Owner: "a", Messages: []Message{{Role: "user", Text: "look", Images: []Image{red}}}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if want := "echo: look\n" + shown(red); r.text != want {
		t.Fatalf("reply = %q, want %q", r.text, want)
	}

	// An image-only message continues the same session.
	req.Messages = append(req.Messages, assistant(r.text), Message{Role: "user", Images: []Image{blue}})
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if want := "echo: " + shown(blue); r.text != want {
		t.Fatalf("reply = %q, want %q", r.text, want)
	}
	if n := len(tp.spawns(t)); n != 1 {
		t.Fatalf("spawned %d processes, want 1", n)
	}

	// The session keeps its history without the image data.
	tp.mu.Lock()
	for _, sess := range tp.sessions {
		for _, msg := range sess.history {
			for _, img := range msg.Images {
				if img.Data != "" || img.key == "" {
					t.Errorf("history image = %+v, want a key and no data", img)
				}
			}
		}
	}
	tp.mu.Unlock()

	// Another image in the same place is another conversation.
	req.Messages[2].Images = []Image{red}
	req.Messages = append(req.Messages, assistant("x"), user("next"))
	if _, err := tp.run(t, req); err != nil {
		t.Fatal(err)
	}
	if spawns := tp.spawns(t); len(spawns) != 2 || spawns[1].Mode != "new" {
		t.Fatalf("spawns = %+v, want a fresh session", spawns)
	}
}

func TestToolResultImagesReachClaude(t *testing.T) {
	tp := newTestPool(t, Limits{})
	screen, attached := pngImage(t, 3), pngImage(t, 4)
	req := Request{Owner: "a", Tools: []Tool{lookupTool}, Messages: []Message{user(`CALL lookup {"q":"a"}`)}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages,
		Message{Role: "assistant", ToolCalls: r.calls},
		Message{Role: "tool", ToolCallID: r.calls[0].ID, Text: "screenshot taken", Images: []Image{screen}},
		Message{Role: "user", Text: "also this", Images: []Image{attached}},
	)
	r, err = tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	// The held call's result carries both images: its own, then those of
	// the message that came with it.
	want := "got: screenshot taken\n" + shown(screen) + "\n\n\n<new_messages>\nalso this\n" + shown(attached) + "\n\n</new_messages>"
	if r.text != want {
		t.Fatalf("reply = %q\nwant %q", r.text, want)
	}
	if n := len(tp.spawns(t)); n != 1 {
		t.Fatalf("spawned %d processes, want the call held in one", n)
	}
}

func TestReseedSendsTheMostRecentImages(t *testing.T) {
	tp := newTestPool(t, Limits{})
	var images []Image
	for i := 0; i < maxReseedImages+3; i++ {
		images = append(images, pngImage(t, uint8(10+i)))
	}
	split := 10
	req := Request{Owner: "a", Messages: []Message{
		{Role: "user", Text: "first", Images: images[:split]},
		assistant("seen"),
		{Role: "user", Text: "second", Images: images[split : len(images)-1]},
		assistant("seen too"),
		{Role: "user", Text: "and now", Images: images[len(images)-1:]},
	}}
	// No session has this history, so a fresh one is seeded with it.
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.text, "echo: This conversation continues from an earlier session.") {
		t.Fatalf("reply = %q, want the transcript", r.text)
	}
	if n := strings.Count(r.text, "[image "); n != maxReseedImages {
		t.Fatalf("%d images sent, want %d:\n%s", n, maxReseedImages, r.text)
	}
	if n := strings.Count(r.text, "An earlier image was here"); n != 3 {
		t.Fatalf("%d earlier images named, want 3:\n%s", n, r.text)
	}
	for i, img := range images {
		if sent := strings.Contains(r.text, shown(img)); sent != (i >= 3) {
			t.Fatalf("image %d sent = %v", i, sent)
		}
	}
	if !strings.HasSuffix(r.text, "and now\n"+shown(images[len(images)-1])) {
		t.Fatalf("reply ends %q, want the new message and its image last", r.text[len(r.text)-80:])
	}
}

func TestWakeSendsImages(t *testing.T) {
	tp, parks := newSleepPool(t, Limits{IdleTimeout: 200 * time.Millisecond})
	req, r := sleepRequest(t, tp, "a")
	waitFor(t, "the sleeping process to close", func() bool { return tp.Status().Running == 0 })
	if parks() != 1 {
		t.Fatalf("parks = %d", parks())
	}
	screen, attached := pngImage(t, 5), pngImage(t, 6)
	req.Messages = append(req.Messages,
		Message{Role: "assistant", ToolCalls: r.calls},
		Message{Role: "tool", ToolCallID: r.calls[0].ID, Text: "set sleep=10m", Images: []Image{screen}},
		Message{Role: "user", Text: "event: screenshot", Images: []Image{attached}})
	woke, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	want := "echo: [tool result for call " + r.calls[0].ID + "]\nset sleep=10m\n" + shown(screen) +
		"\n\n\n<new_messages>\nevent: screenshot\n" + shown(attached) + "\n\n</new_messages>"
	if woke.text != want {
		t.Fatalf("wake reply = %q\nwant %q", woke.text, want)
	}
	if spawns := tp.spawns(t); len(spawns) != 2 || spawns[1].Mode != "resume" {
		t.Fatalf("spawns = %+v, want the session resumed", spawns)
	}
}

// An image that cannot be shown is named in a note; the turn goes on.
func TestImagesThatCannotBeShownBecomeNotes(t *testing.T) {
	linked := solidPNG(t, 3, 3, color.RGBA{0, 0, 255, 255})
	tp := newTestPoolWith(t, Limits{}, func(cfg *Config) {
		cfg.FetchImage = func(_ context.Context, link string) ([]byte, error) {
			if link == "https://images.example/ok.png" {
				return linked, nil
			}
			return nil, errors.New("HTTP 404")
		}
	})
	req := Request{Owner: "a", Messages: []Message{{Role: "user", Text: "four images", Images: []Image{
		{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("hello, not an image"))},
		{MediaType: "image/png", Data: "%%% not base64 %%%"},
		{URL: "https://images.example/ok.png"},
		{URL: "https://images.example/missing.png"},
	}}}}
	r, err := tp.run(t, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"cannot be shown to the model: it is not a PNG, JPEG, GIF or WebP image",
		"its data could not be read",
		claudetest.ImageMarker("image/png", base64.StdEncoding.EncodeToString(linked)),
		"linked from https://images.example/missing.png was attached here, but it could not be fetched (HTTP 404)",
	} {
		if !strings.Contains(r.text, want) {
			t.Errorf("reply lacks %q:\n%s", want, r.text)
		}
	}
}

func TestFitImage(t *testing.T) {
	// Within the limits: unchanged.
	small := solidPNG(t, 10, 10, color.White)
	if kind, out, err := fitImage(small); err != nil || kind != "image/png" || !bytes.Equal(out, small) {
		t.Fatalf("small image: %s, %d bytes, %v", kind, len(out), err)
	}

	// A JPEG labelled as PNG is sent as what it is.
	var jpg bytes.Buffer
	_ = jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil)
	if kind, _, err := fitImage(jpg.Bytes()); err != nil || kind != "image/jpeg" {
		t.Fatalf("jpeg: %s, %v", kind, err)
	}

	// Too wide: scaled down to the longest side Claude takes.
	wide := solidPNG(t, 4000, 1000, color.RGBA{0, 128, 0, 255})
	kind, out, err := fitImage(wide)
	if err != nil || kind != "image/png" {
		t.Fatalf("wide image: %s, %v", kind, err)
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(out)); err != nil || cfg.Width != maxImageEdge || cfg.Height != 644 {
		t.Fatalf("wide image scaled to %+v, %v", cfg, err)
	}

	// Too many bytes for a PNG: noise does not compress, so it ends up a
	// smaller JPEG.
	noise := image.NewRGBA(image.Rect(0, 0, 2000, 2000))
	rand.New(rand.NewSource(1)).Read(noise.Pix)
	for i := 3; i < len(noise.Pix); i += 4 {
		noise.Pix[i] = 255
	}
	var big bytes.Buffer
	_ = png.Encode(&big, noise)
	if fits(big.Len()) {
		t.Fatalf("test image is only %d bytes", big.Len())
	}
	kind, out, err = fitImage(big.Bytes())
	if err != nil || kind != "image/jpeg" || !fits(len(out)) {
		t.Fatalf("noisy image: %s, %d bytes, %v", kind, len(out), err)
	}

	// Too large to decode, and formats Claude does not read.
	if _, _, err := fitImage(pngHeader(9000, 9000)); err == nil || !strings.Contains(err.Error(), "9000x9000 px") {
		t.Fatalf("huge image: %v", err)
	}
	if _, _, err := fitImage([]byte("BM\x00\x00 a bitmap")); err == nil {
		t.Fatal("a BMP was accepted")
	}
}

// pngHeader is the start of a PNG of the given size: enough to read its
// size, not to decode it.
func pngHeader(width, height int) []byte {
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(width))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(height))
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	var buf bytes.Buffer
	buf.WriteString("\x89PNG\r\n\x1a\n")
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr[:]...)
	buf.Write(chunk)
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return buf.Bytes()
}

func TestWebPSize(t *testing.T) {
	webp := func(layout string, chunk []byte) []byte {
		data := append([]byte("RIFF\x00\x00\x00\x00WEBP"+layout+"\x00\x00\x00\x00"), chunk...)
		return append(data, make([]byte, 16)...)
	}
	lossless := make([]byte, 5)
	lossless[0] = 0x2f
	binary.LittleEndian.PutUint32(lossless[1:], uint32(640-1)|uint32(480-1)<<14)
	lossy := []byte{0, 0, 0, 0x9d, 0x01, 0x2a, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(lossy[6:], 800)
	binary.LittleEndian.PutUint16(lossy[8:], 600)
	extended := []byte{0, 0, 0, 0, 0xff, 0x0f, 0, 0x37, 0x04, 0} // 4096x1080
	for _, tc := range []struct {
		data  []byte
		w, h  int
		label string
	}{
		{webp("VP8L", lossless), 640, 480, "lossless"},
		{webp("VP8 ", lossy), 800, 600, "lossy"},
		{webp("VP8X", extended), 4096, 1080, "extended"},
	} {
		if kind := sniffImage(tc.data); kind != "image/webp" {
			t.Fatalf("%s sniffed as %q", tc.label, kind)
		}
		if w, h, err := webpSize(tc.data); err != nil || w != tc.w || h != tc.h {
			t.Fatalf("%s: %dx%d, %v", tc.label, w, h, err)
		}
	}
	// A WebP image cannot be scaled here: one past what Claude takes is
	// named instead.
	if _, _, err := fitImage(webp("VP8X", extended)); err == nil {
		t.Fatal("an oversized WebP image was accepted")
	}
	if kind, _, err := fitImage(webp("VP8L", lossless)); err != nil || kind != "image/webp" {
		t.Fatalf("small WebP: %s, %v", kind, err)
	}
}

func TestFetchPublicImageStaysOffPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(solidPNG(t, 1, 1, color.White))
	}))
	defer srv.Close()
	if _, err := fetchPublicImage(context.Background(), srv.URL+"/a.png"); err == nil || !strings.Contains(err.Error(), "private address") {
		t.Fatalf("fetch from a loopback server: %v", err)
	}
	if _, err := fetchPublicImage(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("a file link was fetched")
	}
	for address, public := range map[string]bool{
		"93.184.215.14:443":          true,
		"[2606:4700::6810:84e5]:443": true,
		"127.0.0.1:80":               false,
		"10.1.2.3:80":                false,
		"192.168.1.10:80":            false,
		"169.254.169.254:80":         false,
		"100.100.1.1:80":             false,
		"0.1.2.3:80":                 false,
		"[::1]:80":                   false,
		"[fd00::1]:80":               false,
		"[::ffff:127.0.0.1]:80":      false,
	} {
		if err := publicAddressOnly("tcp", address, nil); (err == nil) != public {
			t.Errorf("%s: %v, want public=%v", address, err, public)
		}
	}
}

func TestSameMessageComparesImages(t *testing.T) {
	a, b := pngImage(t, 1), pngImage(t, 2)
	withA := Message{Role: "user", Text: "look", Images: []Image{a}}
	withB := Message{Role: "user", Text: "look", Images: []Image{b}}
	if sameMessage(withA, withB) {
		t.Fatal("messages with other images matched")
	}
	if sameMessage(withA, user("look")) {
		t.Fatal("a message matched itself without its image")
	}
	if !sameMessage(historyOf([]Message{withA})[0], keyImages([]Message{withA})[0]) {
		t.Fatal("a kept history did not match the request it came from")
	}
	link := Message{Role: "user", Images: []Image{{URL: "https://x.example/a.png"}}}
	if !sameMessage(link, link) || sameMessage(link, Message{Role: "user", Images: []Image{{URL: "https://x.example/b.png"}}}) {
		t.Fatal("linked images compare by link")
	}
	// Tool results still compare by call id only.
	if !sameMessage(Message{Role: "tool", ToolCallID: "1", Images: []Image{a}}, Message{Role: "tool", ToolCallID: "1"}) {
		t.Fatal("tool results with the same id did not match")
	}
}

func TestRenderTranscriptKeepsImagesInPlace(t *testing.T) {
	img := pngImage(t, 7)
	ps := renderTranscript([]Message{
		user("hi"),
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "shot", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Text: "here", Images: []Image{img}},
	})
	if len(ps) != 3 || ps[1].image == nil || ps[1].image.Data != img.Data {
		t.Fatalf("pieces = %+v", ps)
	}
	if !strings.HasSuffix(ps[0].text, "[tool result for call c1]\nhere") || !strings.HasSuffix(ps[2].text, "Continue from the last tool results above.") {
		t.Fatalf("text around the image = %q / %q", ps[0].text, ps[2].text)
	}
}
