package claudepool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // decoders for image.Decode and image.DecodeConfig
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Image is an image in a message, as the caller sent it: base64 data, or a
// link to fetch. The pool checks it against what Claude accepts, and scales
// it down when it is too large, only when it sends it to Claude; until then
// it is kept as given.
type Image struct {
	// MediaType is the type the caller declared. The data decides: callers
	// sometimes label a JPEG as PNG.
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"` // base64
	URL       string `json:"url,omitempty"`  // http(s), fetched when sent

	// key identifies the image in a session's history, which keeps the key
	// and drops the data.
	key string
}

const (
	// maxImageEncoded is the largest base64 image sent to Claude. The Claude
	// API takes 10 MB per image and Bedrock 5 MB; the smaller is safe
	// wherever Claude Code sends its requests.
	maxImageEncoded = 5 << 20
	// maxImageEdge is the longest side sent to Claude. Current models see
	// at most 2576 px and scale larger images down themselves, but once a
	// request holds more than 20 images the API rejects any side over 3000
	// px, and a long conversation soon holds that many.
	maxImageEdge = 2576
	// manyImagesEdge is the longest side the API takes in a request with
	// more than 20 images.
	manyImagesEdge = 3000
	// maxImagePixels bounds the images decoded to scale them down.
	maxImagePixels = 40_000_000
	// maxImageFetch bounds an image fetched from a link.
	maxImageFetch = 20 << 20
	// maxReseedImages is how many of a conversation's images a fresh
	// session seeded from it gets, the most recent ones. It keeps the
	// request at or under the 20 images above which the API limits sizes.
	maxReseedImages = 20
)

// identity is what histories compare an image by.
func (img Image) identity() string {
	switch {
	case img.key != "":
		return img.key
	case img.URL != "":
		return "url:" + img.URL
	}
	sum := sha256.Sum256([]byte(img.Data))
	return hex.EncodeToString(sum[:])
}

func sameImages(a, b []Image) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].identity() != b[i].identity() {
			return false
		}
	}
	return true
}

// keyImages returns messages with each image's identity worked out once, so
// that matching a request against every session does not hash its images
// again. The caller's slices are not changed.
func keyImages(messages []Message) []Message {
	out := messages
	copied := false
	for i, msg := range messages {
		if len(msg.Images) == 0 {
			continue
		}
		if !copied {
			out = append([]Message(nil), messages...)
			copied = true
		}
		images := make([]Image, len(msg.Images))
		for j, img := range msg.Images {
			img.key = img.identity()
			images[j] = img
		}
		out[i].Images = images
	}
	return out
}

// historyOf is what a session keeps of a conversation: its images only as
// keys, since the history is used to match requests and nothing is sent
// from it.
func historyOf(messages []Message) []Message {
	out := append([]Message(nil), messages...)
	for i, msg := range out {
		if len(msg.Images) == 0 {
			continue
		}
		images := make([]Image, len(msg.Images))
		for j, img := range msg.Images {
			images[j] = Image{MediaType: img.MediaType, URL: img.URL, key: img.identity()}
		}
		out[i].Images = images
	}
	return out
}

// pieces is content on its way to Claude: text and images in order.
// Consecutive text is joined; an empty piece starts a new text block.
type pieces []piece

type piece struct {
	text  string
	image *Image
}

func (ps *pieces) addText(text string) {
	if n := len(*ps); n > 0 && (*ps)[n-1].image == nil {
		(*ps)[n-1].text += text
		return
	}
	if text != "" {
		*ps = append(*ps, piece{text: text})
	}
}

// newBlock makes the next text a block of its own.
func (ps *pieces) newBlock() {
	*ps = append(*ps, piece{})
}

func (ps *pieces) addImages(images []Image) {
	for i := range images {
		*ps = append(*ps, piece{image: &images[i]})
	}
}

// addNewMessages adds the messages that arrived while Claude waited on
// tools, after the results they came with.
func (ps *pieces) addNewMessages(messages []Message) {
	if len(messages) == 0 {
		return
	}
	ps.addText("\n\n<new_messages>\n")
	for i, msg := range messages {
		if i > 0 {
			ps.addText("\n\n")
		}
		ps.addText(msg.Text)
		ps.addImages(msg.Images)
	}
	ps.addText("\n</new_messages>")
}

// trimEnd drops the whitespace the content ends with.
func (ps pieces) trimEnd() pieces {
	for n := len(ps); n > 0 && ps[n-1].image == nil; n = len(ps) {
		ps[n-1].text = strings.TrimRightFunc(ps[n-1].text, isSpace)
		if ps[n-1].text != "" {
			break
		}
		ps = ps[:n-1]
	}
	return ps
}

func isSpace(r rune) bool { return r == ' ' || r == '\n' || r == '\t' || r == '\r' }

// blocks turns pieces into stream-json content blocks. Each image is
// checked and scaled down as needed; one that cannot be shown becomes a
// note saying so, and never fails the turn. Blank text is left out, so the
// result may be empty.
func (p *Pool) blocks(ctx context.Context, ps pieces) []map[string]any {
	out := make([]map[string]any, 0, len(ps))
	for _, piece := range ps {
		if piece.image == nil {
			if strings.TrimSpace(piece.text) != "" {
				out = append(out, map[string]any{"type": "text", "text": piece.text})
			}
			continue
		}
		mediaType, data, note := p.prepareImage(ctx, *piece.image)
		if note != "" {
			out = append(out, map[string]any{"type": "text", "text": note})
			continue
		}
		out = append(out, map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
		})
	}
	return out
}

// orContinue keeps a user message from being empty.
func orContinue(blocks []map[string]any) []map[string]any {
	if len(blocks) == 0 {
		return []map[string]any{{"type": "text", "text": "Continue."}}
	}
	return blocks
}

// prepareImage returns an image as Claude takes it, or the note to send
// instead.
func (p *Pool) prepareImage(ctx context.Context, img Image) (mediaType, data, note string) {
	var raw []byte
	if img.URL != "" {
		fetch := p.cfg.FetchImage
		if fetch == nil {
			fetch = fetchPublicImage
		}
		var err error
		raw, err = fetch(ctx, img.URL)
		if err != nil {
			p.cfg.Logf("[CLAUDE-POOL] could not fetch an image link: %v", err)
			return "", "", fmt.Sprintf("[An image linked from %s was attached here, but it could not be fetched (%v).]", img.URL, err)
		}
	} else {
		var err error
		raw, err = decodeBase64(img.Data)
		if err != nil || len(raw) == 0 {
			return "", "", "[An image was attached here, but its data could not be read.]"
		}
	}
	mediaType, fitted, err := fitImage(raw)
	if err != nil {
		p.cfg.Logf("[CLAUDE-POOL] an image is not passed on: %v", err)
		return "", "", "[An image was attached here, but it cannot be shown to the model: " + err.Error() + ".]"
	}
	return mediaType, base64.StdEncoding.EncodeToString(fitted), ""
}

func decodeBase64(data string) ([]byte, error) {
	data = strings.TrimSpace(data)
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		var out []byte
		if out, err = enc.DecodeString(data); err == nil {
			return out, nil
		}
	}
	return nil, err
}

// fitImage returns an image within what Claude accepts: the data as is when
// it fits, else scaled down. The error says why it cannot be shown.
func fitImage(data []byte) (string, []byte, error) {
	mediaType := sniffImage(data)
	if mediaType == "" {
		return "", nil, errors.New("it is not a PNG, JPEG, GIF or WebP image")
	}
	width, height, err := imageSize(mediaType, data)
	if err != nil {
		return "", nil, fmt.Errorf("the %s image could not be read", mediaType)
	}
	edge := max(width, height)
	if fits(len(data)) && edge <= maxImageEdge {
		return mediaType, data, nil
	}
	tooLarge := fmt.Errorf("the image (%dx%d px, %d KB) is larger than Claude accepts", width, height, len(data)>>10)
	if mediaType == "image/webp" {
		// There is no WebP decoder to scale it with; Claude scales it
		// itself up to the size many-image requests allow.
		if fits(len(data)) && edge <= manyImagesEdge {
			return mediaType, data, nil
		}
		return "", nil, tooLarge
	}
	if width*height > maxImagePixels {
		return "", nil, tooLarge
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", nil, fmt.Errorf("the %s image could not be read", mediaType)
	}
	out, outType, ok := shrinkImage(src, mediaType)
	if !ok {
		return "", nil, tooLarge
	}
	return outType, out, nil
}

// sniffImage returns the media type of the image formats Claude reads, or
// "" for anything else.
func sniffImage(data []byte) string {
	switch kind := http.DetectContentType(data); kind {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return kind
	}
	return ""
}

func imageSize(mediaType string, data []byte) (int, int, error) {
	if mediaType == "image/webp" {
		return webpSize(data)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// webpSize reads the canvas size from a WebP header (the lossy VP8, the
// lossless VP8L or the extended VP8X layout).
func webpSize(data []byte) (int, int, error) {
	if len(data) < 30 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, errors.New("not a WebP image")
	}
	chunk := data[20:]
	switch string(data[12:16]) {
	case "VP8X":
		w := int(chunk[4]) | int(chunk[5])<<8 | int(chunk[6])<<16
		h := int(chunk[7]) | int(chunk[8])<<8 | int(chunk[9])<<16
		return w + 1, h + 1, nil
	case "VP8 ":
		if chunk[3] != 0x9d || chunk[4] != 0x01 || chunk[5] != 0x2a {
			return 0, 0, errors.New("bad VP8 frame")
		}
		w := int(binary.LittleEndian.Uint16(chunk[6:8]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(chunk[8:10]) & 0x3fff)
		return w, h, nil
	case "VP8L":
		if chunk[0] != 0x2f {
			return 0, 0, errors.New("bad VP8L header")
		}
		bits := binary.LittleEndian.Uint32(chunk[1:5])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, nil
	}
	return 0, 0, errors.New("unknown WebP layout")
}

// shrinkImage scales an image to fit maxImageEdge and encodes it within
// maxImageEncoded: as PNG when it came as PNG or GIF and that fits, which
// keeps text in screenshots sharp, else as JPEG, at smaller sizes until it
// fits.
func shrinkImage(src image.Image, mediaType string) ([]byte, string, bool) {
	b := src.Bounds()
	scale := min(1, float64(maxImageEdge)/float64(max(b.Dx(), b.Dy())))
	rgba := toRGBA(src)
	for attempt := 0; attempt < 6; attempt++ {
		width := max(1, int(float64(b.Dx())*scale+0.5))
		height := max(1, int(float64(b.Dy())*scale+0.5))
		scaled := rgba
		if width != b.Dx() || height != b.Dy() {
			scaled = scaleDown(rgba, width, height)
		}
		var buf bytes.Buffer
		if mediaType != "image/jpeg" && png.Encode(&buf, scaled) == nil && fits(buf.Len()) {
			return buf.Bytes(), "image/png", true
		}
		buf.Reset()
		if jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: 85}) == nil && fits(buf.Len()) {
			return buf.Bytes(), "image/jpeg", true
		}
		scale *= 0.7
	}
	return nil, "", false
}

func fits(size int) bool { return base64.StdEncoding.EncodedLen(size) <= maxImageEncoded }

func toRGBA(src image.Image) *image.RGBA {
	if rgba, ok := src.(*image.RGBA); ok && rgba.Rect.Min == (image.Point{}) {
		return rgba
	}
	b := src.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Rect, src, b.Min, draw.Src)
	return rgba
}

// scaleDown shrinks an image by averaging the source pixels behind each
// target pixel.
func scaleDown(src *image.RGBA, width, height int) *image.RGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		y0, y1 := y*sh/height, (y+1)*sh/height
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < width; x++ {
			x0, x1 := x*sw/width, (x+1)*sw/width
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint32
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					px := row[sx*4 : sx*4+4]
					r += uint32(px[0])
					g += uint32(px[1])
					bl += uint32(px[2])
					a += uint32(px[3])
					n++
				}
			}
			d := dst.Pix[y*dst.Stride+x*4:]
			d[0], d[1], d[2], d[3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}

// fetchPublicImage fetches an image link for the model. As the providers
// that fetch links themselves do, it reaches public addresses only, checked
// on every connection (redirects included): the link comes from the
// conversation, and must not be a way into the server's own network.
func fetchPublicImage(ctx context.Context, link string) ([]byte, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("not an http(s) link")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "image/*")
	resp, err := publicImageClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageFetch+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageFetch {
		return nil, fmt.Errorf("larger than %d MB", maxImageFetch>>20)
	}
	return data, nil
}

var publicImageClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:                 nil, // a proxy would make the address check moot
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, Control: publicAddressOnly}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConnsPerHost:   2,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("redirected to a non-http link")
		}
		return nil
	},
}

var errPrivateAddress = errors.New("the link points to a private address")

// sharedAddressSpace is 100.64.0.0/10, used inside carrier and overlay
// networks.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// publicAddressOnly refuses connections to loopback, private, link-local and
// other non-public addresses.
func publicAddressOnly(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return errPrivateAddress
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() ||
		sharedAddressSpace.Contains(addr) || (addr.Is4() && addr.As4()[0] == 0) {
		return errPrivateAddress
	}
	return nil
}
