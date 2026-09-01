package receipt

// EXIF orientation handling: phone cameras store sensor-oriented pixels and
// record the display rotation in the JPEG's EXIF metadata. Decoders ignore
// that tag, so uploads would be stored (and scanned) sideways or upside
// down. The prepareImage pipeline in imaging.go bakes the transform into the
// pixels once, at upload, so everything downstream sees an upright image.

import (
	"bytes"
	"encoding/binary"
	"image"
)

// exifOrientationTag is the TIFF tag holding the display orientation.
const exifOrientationTag = 0x0112

// exifOrientation extracts the EXIF orientation (1–8) from JPEG bytes,
// returning 1 (the "normal" no-op) for anything non-JPEG, unlabelled, or
// malformed.
func exifOrientation(data []byte) int {
	// Scan the JPEG segment chain for the APP1 segment that carries the
	// Exif TIFF block; stop at the first scan segment (start of image data).
	pos := 2 // after the SOI marker
	for pos+4 <= len(data) {
		if data[pos] != 0xFF {
			return 1
		}
		marker := data[pos+1]
		if marker == 0xD9 || marker == 0xDA { // EOI or SOS: no EXIF ahead
			return 1
		}
		if marker == 0xFF || marker == 0x00 { // stuffing, not a segment
			pos += 2
			continue
		}
		if pos+4 > len(data) {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		if segLen < 2 || pos+2+segLen > len(data) {
			return 1
		}
		seg := data[pos+4 : pos+2+segLen]
		if marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return tiffOrientation(seg[6:])
		}
		pos += 2 + segLen
	}
	return 1
}

// tiffOrientation reads the orientation SHORT out of IFD0 of a TIFF block.
func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch {
	case bytes.HasPrefix(tiff, []byte("II")):
		order = binary.LittleEndian
	case bytes.HasPrefix(tiff, []byte("MM")):
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 1
	}
	ifd := int(order.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 1
	}
	entries := int(order.Uint16(tiff[ifd : ifd+2]))
	for i := 0; i < entries; i++ {
		off := ifd + 2 + i*12
		if off+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[off:off+2]) != exifOrientationTag {
			continue
		}
		// A SHORT count of 1 is stored inline in the entry's value field.
		if order.Uint16(tiff[off+2:off+4]) != 3 || order.Uint32(tiff[off+4:off+8]) != 1 {
			return 1
		}
		o := int(order.Uint16(tiff[off+8 : off+10]))
		if o >= 1 && o <= 8 {
			return o
		}
		return 1
	}
	return 1
}

// orientPixels returns the image with its EXIF orientation applied, i.e. the
// pixel arrangement a compliant viewer would display.
func orientPixels(src image.Image, o int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	// Each orientation maps a destination pixel to its source pixel; the
	// dimensions swap whenever the transform includes a quarter turn.
	var dstW, dstH int
	var locate func(x, y int) (int, int)
	switch o {
	case 2: // mirrored horizontally
		dstW, dstH = w, h
		locate = func(x, y int) (int, int) { return w - 1 - x, y }
	case 3: // rotated 180°
		dstW, dstH = w, h
		locate = func(x, y int) (int, int) { return w - 1 - x, h - 1 - y }
	case 4: // mirrored vertically
		dstW, dstH = w, h
		locate = func(x, y int) (int, int) { return x, h - 1 - y }
	case 5: // transposed
		dstW, dstH = h, w
		locate = func(x, y int) (int, int) { return y, x }
	case 6: // rotated 90° clockwise
		dstW, dstH = h, w
		locate = func(x, y int) (int, int) { return y, h - 1 - x }
	case 7: // transversed (anti-diagonal mirror)
		dstW, dstH = h, w
		locate = func(x, y int) (int, int) { return h - 1 - y, w - 1 - x }
	case 8: // rotated 90° counter-clockwise
		dstW, dstH = h, w
		locate = func(x, y int) (int, int) { return w - 1 - y, x }
	default:
		return src
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			sx, sy := locate(x, y)
			dst.Set(x, y, src.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}
