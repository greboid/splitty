package receipt

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// exifJPEG builds a JPEG carrying an EXIF orientation tag. The tag sits in
// an APP1 segment spliced in after the SOI marker, mimicking a camera file.
func exifJPEG(t *testing.T, img image.Image, orientation int, bigEndian bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	encoded := buf.Bytes()
	out := make([]byte, 0, len(encoded)+len(exifAPP1(orientation, bigEndian)))
	out = append(out, encoded[:2]...) // SOI
	out = append(out, exifAPP1(orientation, bigEndian)...)
	return append(out, encoded[2:]...)
}

// exifAPP1 builds an APP1 segment holding an IFD0 with just the orientation
// tag, in either byte order.
func exifAPP1(orientation int, bigEndian bool) []byte {
	var order binary.ByteOrder = binary.LittleEndian
	bo := []byte("II")
	if bigEndian {
		order = binary.BigEndian
		bo = []byte("MM")
	}

	tiff := make([]byte, 8+2+12+4)
	copy(tiff, bo)
	order.PutUint16(tiff[2:4], 42)
	order.PutUint32(tiff[4:8], 8) // IFD0 follows the header
	order.PutUint16(tiff[8:10], 1)
	order.PutUint16(tiff[10:12], exifOrientationTag)
	order.PutUint16(tiff[12:14], 3) // SHORT
	order.PutUint32(tiff[14:18], 1) // count
	order.PutUint16(tiff[18:20], uint16(orientation))

	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := make([]byte, 4+len(seg))
	app1[0], app1[1] = 0xFF, 0xE1
	binary.BigEndian.PutUint16(app1[2:4], uint16(len(seg)+2))
	copy(app1[4:], seg)
	return app1
}

// cornerImage is 64×32 with four solid corner blocks — red, lime top row;
// blue, white bottom row — big enough to survive JPEG chroma subsampling.
func cornerImage(t *testing.T) *image.RGBA {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	fill := func(x0, y0, x1, y1 int, c color.RGBA) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.Set(x, y, c)
			}
		}
	}
	fill(0, 0, 32, 16, color.RGBA{R: 255, A: 255})
	fill(32, 0, 64, 16, color.RGBA{G: 255, A: 255})
	fill(0, 16, 32, 32, color.RGBA{B: 255, A: 255})
	fill(32, 16, 64, 32, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	return img
}

// closeColor compares channels in 8-bit units; JPEG round trips wiggle
// pixel values by a few steps, especially near block edges.
func closeColor(a, b color.Color) bool {
	const tol = 24 * 0x101 // 24 steps on the 16-bit scale RGBA() returns
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	for _, pair := range [][2]uint32{{ar, br}, {ag, bg}, {ab, bb}} {
		d := int(pair[0]) - int(pair[1])
		if d < -tol || d > tol {
			return false
		}
	}
	return true
}

func TestExifOrientationParsing(t *testing.T) {
	img := cornerImage(t)
	for _, bigEndian := range []bool{false, true} {
		order := "little"
		if bigEndian {
			order = "big"
		}
		for o := 1; o <= 8; o++ {
			if got := exifOrientation(exifJPEG(t, img, o, bigEndian)); got != o {
				t.Errorf("%s endian: orientation = %d, want %d", order, got, o)
			}
		}
		if got := exifOrientation(exifJPEG(t, img, 9, bigEndian)); got != 1 {
			t.Errorf("%s endian: out-of-range orientation = %d, want 1", order, got)
		}
		if got := exifOrientation(exifJPEG(t, img, 0, bigEndian)); got != 1 {
			t.Errorf("%s endian: zero orientation = %d, want 1", order, got)
		}
	}

	if got := exifOrientation(testPNG(t)); got != 1 {
		t.Errorf("PNG orientation = %d, want 1", got)
	}
	plain := new(bytes.Buffer)
	jpeg.Encode(plain, cornerImage(t), nil)
	if got := exifOrientation(plain.Bytes()); got != 1 {
		t.Errorf("EXIF-less JPEG orientation = %d, want 1", got)
	}
	// An APP1 segment whose TIFF block is cut short must not panic or read
	// out of bounds.
	truncated := append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x0C}, []byte("Exif\x00\x00MM\x00\x2A")...)
	if got := exifOrientation(truncated); got != 1 {
		t.Errorf("truncated EXIF orientation = %d, want 1", got)
	}
}

func TestOrientPixelsRotates(t *testing.T) {
	// Orientation 6 means "rotate 90° clockwise for display": the 64×32
	// corner image becomes 32×64 and the bottom-left block moves to the
	// top-left.
	src := cornerImage(t)
	got := orientPixels(src, 6)
	if b := got.Bounds(); b.Dx() != 32 || b.Dy() != 64 {
		t.Fatalf("bounds = %v, want upright 32×64", b)
	}
	checks := []struct {
		x, y         int
		wantX, wantY int
		label        string
	}{
		{8, 8, 8, 24, "blue bottom-left to top-left"},
		{24, 8, 8, 8, "red top-left to top-right"},
		{8, 56, 56, 24, "white bottom-right to bottom-left"},
		{24, 56, 56, 8, "lime top-right to bottom-right"},
	}
	for _, c := range checks {
		if !closeColor(got.At(c.x, c.y), src.At(c.wantX, c.wantY)) {
			t.Errorf("orientation 6: %s: got %v at (%d,%d), want %v", c.label, got.At(c.x, c.y), c.x, c.y, src.At(c.wantX, c.wantY))
		}
	}
}
