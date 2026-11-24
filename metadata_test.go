package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

const secret = "SECRET-GPS-51.5N"

func testImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 16, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 16), uint8(y * 32), 128, 255})
		}
	}
	return img
}

func jpegSeg(marker byte, payload []byte) []byte {
	s := []byte{0xFF, marker}
	s = binary.BigEndian.AppendUint16(s, uint16(len(payload)+2))
	return append(s, payload...)
}

func TestStripJPEG(t *testing.T) {
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	clean := enc.Bytes()

	// EXIF with orientation 6 (rotate 90°) plus "GPS" data, an XMP packet, a
	// comment, an MPF segment, and a second image appended after EOI.
	exif := append(exifOrientationSegment(6)[4:], secret...)
	var dirty []byte
	dirty = append(dirty, clean[:2]...)
	dirty = append(dirty, jpegSeg(0xE1, exif)...)
	dirty = append(dirty, jpegSeg(0xE1, []byte("http://ns.adobe.com/xap/1.0/\x00"+secret))...)
	dirty = append(dirty, jpegSeg(0xFE, []byte(secret))...)
	dirty = append(dirty, jpegSeg(0xE2, []byte("MPF\x00"+secret))...)
	dirty = append(dirty, jpegSeg(0xE2, []byte("ICC_PROFILE\x00keep"))...)
	dirty = append(dirty, clean[2:]...)
	dirty = append(dirty, secret...)

	out, err := stripJPEG(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(secret)) {
		t.Fatal("metadata survived")
	}
	if !bytes.Contains(out, []byte("ICC_PROFILE\x00keep")) {
		t.Fatal("color profile was dropped")
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("cleaned JPEG doesn't decode: %v", err)
	}
	// Orientation is preserved as the first segment.
	if out[2] != 0xFF || out[3] != 0xE1 {
		t.Fatalf("expected EXIF orientation segment first, got % x", out[2:4])
	}
	n := int(binary.BigEndian.Uint16(out[4:]))
	if o := exifOrientationOf(out[6 : 4+n]); o != 6 {
		t.Fatalf("orientation = %d, want 6", o)
	}
}

func TestStripJPEGNoOrientation(t *testing.T) {
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	out, err := stripJPEG(enc.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, enc.Bytes()) {
		t.Fatal("a JPEG with no metadata should come out unchanged")
	}
}

func pngChunk(typ string, data []byte) []byte {
	c := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	c = append(c, typ...)
	c = append(c, data...)
	return binary.BigEndian.AppendUint32(c, crc32.ChecksumIEEE(c[4:]))
}

func TestStripPNG(t *testing.T) {
	var enc bytes.Buffer
	if err := png.Encode(&enc, testImage()); err != nil {
		t.Fatal(err)
	}
	clean := enc.Bytes()
	ihdrEnd := 8 + 12 + 13 // signature + IHDR chunk
	var dirty []byte
	dirty = append(dirty, clean[:ihdrEnd]...)
	dirty = append(dirty, pngChunk("tEXt", []byte("Author\x00"+secret))...)
	dirty = append(dirty, pngChunk("eXIf", []byte(secret))...)
	dirty = append(dirty, pngChunk("caBX", []byte(secret))...) // unknown private chunk
	dirty = append(dirty, clean[ihdrEnd:]...)
	dirty = append(dirty, secret...)

	out, err := stripPNG(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(secret)) {
		t.Fatal("metadata survived")
	}
	if !bytes.Equal(out, clean) {
		t.Fatal("expected exactly the original PNG back")
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("cleaned PNG doesn't decode: %v", err)
	}
}

func TestStripGIF(t *testing.T) {
	frame := func() *image.Paletted {
		p := image.NewPaletted(image.Rect(0, 0, 8, 8), palette.Plan9)
		p.SetColorIndex(1, 1, 5)
		return p
	}
	var enc bytes.Buffer
	anim := &gif.GIF{Image: []*image.Paletted{frame(), frame()}, Delay: []int{10, 10}, LoopCount: 0}
	if err := gif.EncodeAll(&enc, anim); err != nil {
		t.Fatal(err)
	}
	clean := enc.Bytes()
	headerEnd := 13
	if clean[10]&0x80 != 0 {
		headerEnd += 3 << ((clean[10] & 7) + 1)
	}
	comment := append([]byte{0x21, 0xFE, byte(len(secret))}, secret...)
	comment = append(comment, 0)
	xmp := append([]byte{0x21, 0xFF, 11}, "XMP DataXMP"...)
	xmp = append(xmp, byte(len(secret)))
	xmp = append(xmp, secret...)
	xmp = append(xmp, 0)

	var dirty []byte
	dirty = append(dirty, clean[:headerEnd]...)
	dirty = append(dirty, comment...)
	dirty = append(dirty, xmp...)
	dirty = append(dirty, clean[headerEnd:]...)

	out, err := stripGIF(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(secret)) {
		t.Fatal("metadata survived")
	}
	if !bytes.Equal(out, clean) {
		t.Fatal("expected exactly the original GIF back")
	}
	g, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("cleaned GIF doesn't decode: %v", err)
	}
	if len(g.Image) != 2 || g.LoopCount != 0 {
		t.Fatalf("animation lost: %d frames, loop count %d", len(g.Image), g.LoopCount)
	}
}

func riffChunk(fourcc string, data []byte) []byte {
	c := append([]byte(fourcc), binary.LittleEndian.AppendUint32(nil, uint32(len(data)))...)
	c = append(c, data...)
	if len(data)%2 == 1 {
		c = append(c, 0)
	}
	return c
}

func TestStripWebP(t *testing.T) {
	vp8x := make([]byte, 10)
	vp8x[0] = 0x10 | 0x08 | 0x04 // alpha, EXIF, XMP
	pix := []byte("pretend-VP8L-data")
	var body []byte
	body = append(body, "WEBP"...)
	body = append(body, riffChunk("VP8X", vp8x)...)
	body = append(body, riffChunk("VP8L", pix)...)
	body = append(body, riffChunk("EXIF", []byte(secret))...)
	body = append(body, riffChunk("XMP ", []byte(secret+"x"))...) // odd length → padded
	dirty := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	dirty = append(dirty, body...)

	out, err := stripWebP(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(secret)) {
		t.Fatal("metadata survived")
	}
	if got := int(binary.LittleEndian.Uint32(out[4:8])); got != len(out)-8 {
		t.Fatalf("RIFF size %d, want %d", got, len(out)-8)
	}
	if flags := out[12+8]; flags != 0x10 {
		t.Fatalf("VP8X flags = %#x, want only alpha (0x10)", flags)
	}
	if !bytes.Contains(out, pix) {
		t.Fatal("image data was dropped")
	}
}

// Truncated or corrupt files must give an error, never a panic.
func TestStripTruncated(t *testing.T) {
	var j, p, g bytes.Buffer
	jpeg.Encode(&j, testImage(), nil)
	png.Encode(&p, testImage())
	gif.Encode(&g, testImage(), nil)
	for _, file := range [][]byte{j.Bytes(), p.Bytes(), g.Bytes()} {
		f, ok := sniffPicture(file)
		if !ok {
			t.Fatal("format not recognized")
		}
		for n := 0; n < len(file); n++ {
			f.strip(file[:n]) // must not panic
		}
	}
}

func TestSniffRejectsOtherFiles(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("hello"), []byte("%PDF-1.7"), []byte("\x00\x00\x00\x18ftypheic")} {
		if _, ok := sniffPicture(b); ok {
			t.Fatalf("%q accepted as a picture", b)
		}
	}
}
