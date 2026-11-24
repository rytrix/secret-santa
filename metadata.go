package main

// Removing hidden metadata from gift pictures.
//
// Photos carry data the sender never sees: GPS coordinates, the phone's make
// and model, editing software, author names. Any of that could give away who
// a Secret Santa is (or where they live), so every picture is cleaned before
// it is stored.
//
// The cleaning is lossless: each format is a sequence of tagged blocks, and we
// copy the blocks that hold pixels and color information and drop the ones
// that hold metadata. Pixels are never decoded or re-encoded, so quality is
// unchanged. We keep an allowlist (not a blocklist) of blocks, so metadata in
// some block type we've never heard of is dropped too.

import (
	"bytes"
	"encoding/binary"
	"errors"
)

var errBadPicture = errors.New("malformed picture")

type picFormat struct {
	ext, contentType string
	strip            func([]byte) ([]byte, error)
}

// sniffPicture identifies the format from the file's first bytes rather than
// trusting the uploaded filename or content type.
func sniffPicture(b []byte) (picFormat, bool) {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return picFormat{".jpg", "image/jpeg", stripJPEG}, true
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return picFormat{".png", "image/png", stripPNG}, true
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return picFormat{".gif", "image/gif", stripGIF}, true
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return picFormat{".webp", "image/webp", stripWebP}, true
	}
	return picFormat{}, false
}

func contentTypeOf(ext string) string {
	switch ext {
	case ".jpg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return "application/octet-stream"
}

// ------------------------------------------------------------------- JPEG

// stripJPEG walks the JPEG's segments. Each one is 0xFF, a marker byte, then
// (for most markers) a 2-byte big-endian length that counts itself.
//
//	kept:    APP0 (JFIF), APP2 "ICC_PROFILE" (colors), APP14 "Adobe" (color
//	         transform), and every non-APP segment (tables, frame, scans)
//	dropped: APP1 (EXIF, XMP), other APP2 (e.g. MPF), APP3–APP13, APP15, COM
//	         (comments), and anything after the end-of-image marker (phones
//	         append extra images there, each with its own metadata)
//
// EXIF also holds the orientation flag that says "rotate this photo 90°".
// Phones store photos sideways and rely on that flag, so dropping it would
// show the picture rotated. We write back a minimal EXIF segment holding only
// the orientation.
func stripJPEG(b []byte) ([]byte, error) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, errBadPicture
	}
	out := make([]byte, 0, len(b))
	out = append(out, 0xFF, 0xD8) // SOI: start of image
	orientation := 0
	insertAt := len(out) // where the minimal EXIF goes: after SOI, or after a leading APP0
	first := true
	i := 2
	for {
		// A marker is 0xFF then a non-0xFF byte; extra 0xFF bytes are padding.
		if i >= len(b) || b[i] != 0xFF {
			return nil, errBadPicture
		}
		for i < len(b) && b[i] == 0xFF {
			i++
		}
		if i >= len(b) {
			return nil, errBadPicture
		}
		m := b[i]
		i++

		if m == 0xD9 { // EOI: end of image
			out = append(out, 0xFF, 0xD9)
			if orientation > 1 { // 1 means "upright", same as no flag
				out = insertBytes(out, insertAt, exifOrientationSegment(orientation))
			}
			return out, nil
		}
		if m == 0x01 || (m >= 0xD0 && m <= 0xD7) { // TEM, RST0–7: no length field
			out = append(out, 0xFF, m)
			continue
		}

		if i+2 > len(b) {
			return nil, errBadPicture
		}
		n := int(binary.BigEndian.Uint16(b[i:]))
		if n < 2 || i+n > len(b) {
			return nil, errBadPicture
		}
		seg := b[i : i+n] // length bytes + payload
		payload := seg[2:]
		i += n

		keep := true
		switch {
		case m == 0xE1: // APP1: EXIF or XMP
			keep = false
			if orientation == 0 {
				orientation = exifOrientationOf(payload)
			}
		case m == 0xE0: // APP0: JFIF header
		case m == 0xE2:
			keep = bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00"))
		case m == 0xEE:
			keep = bytes.HasPrefix(payload, []byte("Adobe"))
		case m >= 0xE3 && m <= 0xEF, m == 0xFE: // other APPn, COM
			keep = false
		}
		if keep {
			out = append(out, 0xFF, m)
			out = append(out, seg...)
			if m == 0xE0 && first {
				insertAt = len(out)
			}
		}
		first = false

		if m == 0xDA {
			// SOS: compressed pixel data follows, with no length. Inside it a
			// 0xFF byte is always followed by 0x00 (an escaped 0xFF) or a
			// restart marker, so the first 0xFF followed by anything else is
			// the next real segment.
			j := i
			for {
				if j+1 >= len(b) {
					return nil, errBadPicture
				}
				if b[j] == 0xFF {
					next := b[j+1]
					if next != 0x00 && next != 0xFF && !(next >= 0xD0 && next <= 0xD7) {
						break
					}
				}
				j++
			}
			out = append(out, b[i:j]...)
			i = j
		}
	}
}

// exifOrientationOf reads the Orientation tag (0x0112) from an APP1 EXIF
// payload: "Exif\0\0", then a TIFF header ("II" little-endian or "MM"
// big-endian, 42, offset of the first directory), then that directory: a
// 2-byte entry count and 12-byte entries (tag, type, count, value).
// Returns 0 if absent or unreadable.
func exifOrientationOf(p []byte) int {
	if !bytes.HasPrefix(p, []byte("Exif\x00\x00")) {
		return 0
	}
	t := p[6:]
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 0
	}
	count := int(bo.Uint16(t[off:]))
	for k := 0; k < count; k++ {
		e := off + 2 + 12*k
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:]) == 0x0112 && bo.Uint16(t[e+2:]) == 3 { // Orientation, type SHORT
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 0
		}
	}
	return 0
}

// exifOrientationSegment builds a 36-byte APP1 segment whose only content is
// the orientation: marker (2) + length (2) + "Exif\0\0" (6) + TIFF header (8)
// + entry count (2) + one entry (12) + next-directory offset (4).
func exifOrientationSegment(o int) []byte {
	p := []byte("Exif\x00\x00MM\x00\x2a")
	p = binary.BigEndian.AppendUint32(p, 8)      // first directory right after the header
	p = binary.BigEndian.AppendUint16(p, 1)      // one entry
	p = binary.BigEndian.AppendUint16(p, 0x0112) // tag: Orientation
	p = binary.BigEndian.AppendUint16(p, 3)      // type: SHORT
	p = binary.BigEndian.AppendUint32(p, 1)      // one value
	p = binary.BigEndian.AppendUint16(p, uint16(o))
	p = append(p, 0, 0)                     // pad the 4-byte value field
	p = binary.BigEndian.AppendUint32(p, 0) // no next directory
	seg := []byte{0xFF, 0xE1}
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(p)+2))
	return append(seg, p...)
}

func insertBytes(b []byte, at int, ins []byte) []byte {
	out := make([]byte, 0, len(b)+len(ins))
	out = append(out, b[:at]...)
	out = append(out, ins...)
	return append(out, b[at:]...)
}

// -------------------------------------------------------------------- PNG

// PNG is an 8-byte signature then chunks: 4-byte length, 4-byte type, data,
// 4-byte CRC. The CRC covers only that chunk, so dropping chunks needs no
// recalculation. Metadata lives in tEXt/zTXt/iTXt (text, often XMP), eXIf,
// tIME, and private chunks; only image and color chunks are kept.
var pngKeep = map[string]bool{
	"IHDR": true, "PLTE": true, "IDAT": true, "IEND": true, // the image itself
	"tRNS": true, "bKGD": true, "sBIT": true, "hIST": true, "pHYs": true,
	"cHRM": true, "gAMA": true, "iCCP": true, "sRGB": true, // color
	"cICP": true, "mDCV": true, "cLLI": true, // HDR color
	"acTL": true, "fcTL": true, "fdAT": true, // animation (APNG)
}

func stripPNG(b []byte) ([]byte, error) {
	if len(b) < 8 {
		return nil, errBadPicture
	}
	out := append([]byte(nil), b[:8]...)
	i := 8
	for {
		if i+12 > len(b) {
			return nil, errBadPicture
		}
		n := int(binary.BigEndian.Uint32(b[i:]))
		if n > len(b)-i-12 {
			return nil, errBadPicture
		}
		typ := string(b[i+4 : i+8])
		end := i + 12 + n
		if pngKeep[typ] {
			out = append(out, b[i:end]...)
		}
		i = end
		if typ == "IEND" {
			return out, nil // anything after IEND is dropped
		}
	}
}

// -------------------------------------------------------------------- GIF

// GIF is a 6-byte header, a 7-byte screen descriptor, an optional color
// table, then blocks until the 0x3B trailer. Extension blocks (0x21) and
// image data are stored as "sub-blocks": a size byte then that many bytes,
// ending at a size of 0. Kept: images, graphic control (frame timing and
// transparency), plain text, and the NETSCAPE/ANIMEXTS loop-count extension.
// Dropped: comments and every other application extension (XMP lives there).
func stripGIF(b []byte) ([]byte, error) {
	if len(b) < 13 {
		return nil, errBadPicture
	}
	i := 13
	if b[10]&0x80 != 0 { // global color table: 3 bytes × 2^(n+1) colors
		i += 3 << ((b[10] & 7) + 1)
	}
	if i > len(b) {
		return nil, errBadPicture
	}
	out := append([]byte(nil), b[:i]...)

	// subBlocks returns the index just past a run of sub-blocks starting at j.
	subBlocks := func(j int) (int, error) {
		for {
			if j >= len(b) {
				return 0, errBadPicture
			}
			n := int(b[j])
			j += 1 + n
			if n == 0 {
				return j, nil
			}
		}
	}

	for {
		if i >= len(b) {
			return nil, errBadPicture
		}
		switch b[i] {
		case 0x3B: // trailer; anything after it is dropped
			return append(out, 0x3B), nil

		case 0x21: // extension: 0x21, label, sub-blocks
			if i+2 > len(b) {
				return nil, errBadPicture
			}
			label := b[i+1]
			end, err := subBlocks(i + 2)
			if err != nil {
				return nil, err
			}
			keep := label == 0xF9 || label == 0x01 // graphic control, plain text
			if label == 0xFF {                     // application: first sub-block is an 11-byte ID
				app := b[i+2 : end]
				keep = bytes.HasPrefix(app, []byte("\x0bNETSCAPE2.0")) || bytes.HasPrefix(app, []byte("\x0bANIMEXTS1.0"))
			}
			if keep {
				out = append(out, b[i:end]...)
			}
			i = end

		case 0x2C: // image: 10-byte descriptor, optional local color table, LZW size, sub-blocks
			j := i + 10
			if j > len(b) {
				return nil, errBadPicture
			}
			if b[i+9]&0x80 != 0 {
				j += 3 << ((b[i+9] & 7) + 1)
			}
			end, err := subBlocks(j + 1) // +1 skips the LZW minimum code size byte
			if err != nil {
				return nil, err
			}
			out = append(out, b[i:end]...)
			i = end

		default:
			return nil, errBadPicture
		}
	}
}

// ------------------------------------------------------------------- WebP

// WebP is a RIFF file: "RIFF", 4-byte little-endian size, "WEBP", then chunks:
// 4-byte type, 4-byte little-endian size, data, padded to an even length.
// EXIF and XMP are their own chunks, and the VP8X header chunk has flag bits
// announcing them, which are cleared when the chunks are dropped.
var webpKeep = map[string]bool{
	"VP8 ": true, "VP8L": true, "VP8X": true, "ALPH": true, // image, header, alpha
	"ANIM": true, "ANMF": true, // animation
	"ICCP": true, // color profile
}

func stripWebP(b []byte) ([]byte, error) {
	if len(b) < 12 {
		return nil, errBadPicture
	}
	size := int(binary.LittleEndian.Uint32(b[4:8]))
	if size < 4 || size > len(b)-8 {
		return nil, errBadPicture
	}
	b = b[:8+size] // anything after the RIFF is dropped

	body := []byte("WEBP")
	i := 12
	for i < len(b) {
		if i+8 > len(b) {
			return nil, errBadPicture
		}
		n := int(binary.LittleEndian.Uint32(b[i+4:]))
		if n > len(b)-i-8 {
			return nil, errBadPicture
		}
		padded := n + n&1
		end := i + 8 + padded
		if end > len(b) { // some encoders leave off the final pad byte
			end = len(b)
		}
		fourcc := string(b[i : i+4])
		if webpKeep[fourcc] {
			chunk := append([]byte(nil), b[i:end]...)
			for len(chunk) < 8+padded {
				chunk = append(chunk, 0)
			}
			if fourcc == "VP8X" && n >= 1 {
				chunk[8] &^= 0x08 | 0x04 // clear the EXIF and XMP flags
			}
			body = append(body, chunk...)
		}
		i = end
	}
	out := []byte("RIFF")
	out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
	return append(out, body...), nil
}
