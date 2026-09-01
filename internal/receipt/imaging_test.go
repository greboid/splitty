package receipt

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// syntheticReceipt JPEG-encodes a receipt-like test image: pale paper with
// dashed dark text bars inset on a dark background by margin pixels. The
// bars slant by skewDeg, mimicking a skewed photo; with margin 0 the paper
// fills the whole frame like a full-bleed scan.
func syntheticReceipt(t *testing.T, w, h, margin int, skewDeg float64) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	bg := color.RGBA{R: 60, G: 60, B: 60, A: 255}
	paper := color.RGBA{R: 228, G: 228, B: 224, A: 255}
	ink := color.RGBA{R: 30, G: 30, B: 30, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, bg)
		}
	}
	px0, py0, px1, py1 := margin, margin, w-margin, h-margin
	for y := py0; y < py1; y++ {
		for x := px0; x < px1; x++ {
			img.Set(x, y, paper)
		}
	}
	tan := math.Tan(skewDeg * math.Pi / 180)
	for baseY := py0 + 8; baseY+2 < py1-4; baseY += 8 {
		for x := px0 + 4; x < px1-4; x++ {
			if (x/3)%2 != 0 {
				continue // dashes, like glyphs separated by paper
			}
			y := baseY + int(float64(x-px0-4)*tan)
			for dy := 0; dy < 2; dy++ {
				if y+dy >= py0 && y+dy < py1 {
					img.Set(x, y+dy, ink)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeImage(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return img
}

// inkRowScore scores how sharply the text lines concentrate into horizontal
// rows when viewed at deg: squared per-row ink totals over a central window
// shared by both angles. A deskewed receipt scores highest at 0.
func inkRowScore(t *testing.T, img image.Image, deg float64) float64 {
	t.Helper()
	grey, _ := grayscalePlane(img)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	winW, winH := inscribedSize(w, h, deg)
	rot, rw, rh := rotateGrayWindow(grey, w, h, winW, winH, deg)
	var s float64
	for y := 0; y < rh; y++ {
		var row float64
		for x := 0; x < rw; x++ {
			row += float64(255 - rot[y*rw+x])
		}
		s += row * row
	}
	return s
}

func TestPrepareTrimsBackground(t *testing.T) {
	const w, h, margin = 160, 240, 24
	out := decodeImage(t, prepareImage(syntheticReceipt(t, w, h, margin, 0)))
	b := out.Bounds()
	gotW, gotH := b.Dx(), b.Dy()
	paperW, paperH := w-2*margin, h-2*margin
	// The crop keeps the paper (plus its padding and a few pixels of
	// JPEG bleed) and eats most of the margin.
	if gotW < paperW-4 || gotW > w-2*(margin-16) || gotH < paperH-4 || gotH > h-2*(margin-16) {
		t.Fatalf("trimmed to %dx%d, want between the %dx%d paper and %dx%d", gotW, gotH, paperW, paperH, w-32, h-32)
	}
	if gotW*gotH > w*h*3/4 {
		t.Errorf("crop %dx%d barely smaller than the %dx%d frame", gotW, gotH, w, h)
	}
}

func TestPrepareDeskewsSkewedReceipt(t *testing.T) {
	const skew = 3.0
	// Full-bleed paper so the crop step stays out and this isolates deskew.
	out := decodeImage(t, prepareImage(syntheticReceipt(t, 200, 300, 0, skew)))
	if b := out.Bounds(); b.Dx() < 100 || b.Dy() < 150 {
		t.Fatalf("deskewed size %v looks over-cropped", b)
	}
	// On the straightened output the lines must align at 0° and clearly not
	// at the original skew.
	straight := inkRowScore(t, out, 0)
	skewed := inkRowScore(t, out, skew)
	if straight < 1.3*skewed {
		t.Errorf("output not straightened: score(0°)=%v vs score(%v°)=%v", straight, skew, skewed)
	}
	// And the estimator should find no residual skew.
	grey, _ := grayscalePlane(out)
	angle, best, zero := bestSkewAngle(grey, out.Bounds().Dx(), out.Bounds().Dy())
	if math.Abs(angle) > 0.5 {
		t.Errorf("residual skew %v°", angle)
	}
	if best > deskewMinImprove*zero {
		t.Errorf("estimator still prefers %v° over straight", angle)
	}
}

func TestPrepareLeavesImagesAlone(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"tiny PNG", testPNG(t)},
		// A JPEG that carries EXIF but will not decode must pass through
		// untouched rather than failing the upload.
		{"undecodable JPEG with EXIF", append([]byte{0xFF, 0xD8}, exifAPP1(6, true)...)},
		// Full-bleed straight scan: nothing to trim, nothing to rotate, so
		// no re-encode and zero quality loss.
		{"full-bleed straight receipt", syntheticReceipt(t, 160, 240, 0, 0)},
		// Colourful photo-like content without a paper sheet.
		{"colourful blocks", func() []byte {
			var buf bytes.Buffer
			jpeg.Encode(&buf, cornerImage(t), nil)
			return buf.Bytes()
		}()},
	}
	for _, tc := range cases {
		if got := prepareImage(tc.data); !bytes.Equal(got, tc.data) {
			t.Errorf("%s: was modified", tc.name)
		}
	}
}

// TestScanUprightsRotatedJPEG uploads an orientation-6 JPEG through the
// upload endpoint and checks the stored file comes out upright.
func TestScanUprightsRotatedJPEG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": `{"merchant":"M","items":[]}`},
			}},
		})
	}))
	defer srv.Close()

	store := mustStore(t)
	h := &Handlers{Store: store, Client: NewClient(srv.URL+"/v1", "k", "m", "")}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("image", "receipt.jpg")
	fw.Write(exifJPEG(t, decodeImage(t, syntheticReceipt(t, 64, 32, 0, 0)), 6, false))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/receipts", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.Upload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var stored scanResponse
	if err := json.NewDecoder(rec.Body).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	f, _, err := store.Open(stored.File)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	img := decodeImage(t, data)
	if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 64 {
		t.Errorf("stored bounds = %v, want upright 32×64", b)
	}
}
