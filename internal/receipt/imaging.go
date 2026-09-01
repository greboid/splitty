package receipt

// Best-effort upload cleanup: bake in the camera's EXIF rotation, crop away
// the background around the receipt, and straighten small skew angles so
// text lines run horizontally. Every step is conservative — an upload that
// does not look like a receipt photo (or that trips any decode/encode
// hiccup) is stored exactly as it arrived, so cleanup can never make an
// upload worse, only skip it.

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
)

// maxProcessedPixels caps the decode size cleanup will touch; anything
// bigger passes through unprocessed rather than stalling the request.
const maxProcessedPixels = 25 << 20

// jpegQuality for cleaned-up re-encodes; visually lossless while still
// shrinking typical camera output.
const jpegQuality = 90

const (
	deskewMaxAngle   = 10.0  // degrees; beyond this a photo needs perspective correction, not a twist
	deskewMinAngle   = 0.4   // smaller corrections are invisible and not worth a re-encode
	deskewMinImprove = 1.25  // the best angle must beat straight-on by this margin
	deskewEdgeFrac   = 0.5   // angles this close to the search limit are peaks of noise, not text
	deskewPreview    = 400   // long edge of the downscaled copy used for angle search
	paperMinCoverage = 0.10  // receipt paper smaller than this fraction of the frame is suspicious
	paperMaxCoverage = 0.90  // above this there is no background to trim
	paperRowShare    = 0.50  // a row/column belongs to the receipt when this much of it is paper
	paperResumeShare = 0.08  // a row/col showing this much paper continues the sheet past a dark band
	paperMinContrast = 40.0  // required brightness gap between paper and background
	paperMaxSat      = 60    // paper is white/grey; saturated bright pixels are not paper
	paperMinInk      = 0.008 // the crop must contain some writing, not a blank sheet
	paperMaxInk      = 0.50  // and not be mostly background wedges
)

// prepareImage normalises an uploaded receipt image and returns the bytes to
// store. Non-JPEG/PNG data, undecodable data, and images where no step
// changed anything come back untouched, so well-behaved uploads pay no
// quality loss.
func prepareImage(data []byte) []byte {
	if len(data) < 8 {
		return data
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		return data
	}
	b := img.Bounds()
	if b.Dx() < 32 || b.Dy() < 32 || b.Dx() > maxProcessedPixels/b.Dy() {
		return data
	}

	changed := false
	if format == "jpeg" {
		if o := exifOrientation(data); o > 1 {
			img = orientPixels(img, o)
			changed = true
		}
	}
	if cropped, ok := trimReceipt(img); ok {
		img = cropped
		changed = true
	}
	if rotated := deskewImage(img); rotated != nil {
		img = rotated
		changed = true
	}
	if !changed {
		return data
	}

	var buf bytes.Buffer
	if format == "png" {
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality})
	}
	if err != nil {
		return data
	}
	return buf.Bytes()
}

// trimReceipt crops the image to the bright, low-saturation sheet it finds
// (the receipt paper) when one stands out against a contrasting background.
func trimReceipt(img image.Image) (image.Image, bool) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	grey, sat := grayscalePlane(img)

	cut, lowMean, highMean := otsuThreshold(grey)
	if highMean-lowMean < paperMinContrast {
		// No clear paper-versus-background split: scans on white paper,
		// dark-mode screenshots, and photos of flat documents all land here.
		return img, false
	}

	rows := make([]int, h)
	cols := make([]int, w)
	paper, ink := 0, 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if int(grey[i]) >= cut && sat[i] <= paperMaxSat {
				rows[y]++
				cols[x]++
				paper++
			}
			if int(grey[i]) < cut {
				ink++
			}
		}
	}
	if cover := float64(paper) / float64(w*h); cover < paperMinCoverage || cover > paperMaxCoverage {
		return img, false
	}

	y0, y1, ok := longestRun(rows, w)
	if !ok {
		return img, false
	}
	x0, x1, ok := longestRun(cols, h)
	if !ok {
		return img, false
	}
	// Dark interior bands (a big logo, a photo block) can dip below the run
	// threshold; walk the run back out through rows and columns that still
	// show paper, so over-inked content is never cropped away. True
	// background carries too little paper to resume.
	for y0 > 0 && float64(rows[y0-1]) >= paperResumeShare*float64(w) {
		y0--
	}
	for y1 < h-1 && float64(rows[y1+1]) >= paperResumeShare*float64(w) {
		y1++
	}
	for x0 > 0 && float64(cols[x0-1]) >= paperResumeShare*float64(h) {
		x0--
	}
	for x1 < w-1 && float64(cols[x1+1]) >= paperResumeShare*float64(h) {
		x1++
	}
	if x1-x0 < w/4 || y1-y0 < h/4 {
		return img, false
	}
	if x1-x0 >= w*97/100 && y1-y0 >= h*97/100 {
		return img, false // the frame is already the receipt
	}
	// The candidate crop must hold the writing, not an empty sheet; and not
	// be dominated by background wedges around a skewed receipt.
	inW, inH := x1-x0+1, y1-y0+1
	// Text is judged over the crop's interior: JPEG bleed keeps the columns
	// hugging the edges permanently inky, which is not writing.
	ix0, ix1 := x0+2, x1-2
	innerW := ix1 - ix0 + 1
	cropInk := 0
	textRows := 0
	for y := y0; y <= y1; y++ {
		rowInk, innerInk := 0, 0
		for x := x0; x <= x1; x++ {
			if int(grey[y*w+x]) < cut {
				rowInk++
				if x >= ix0 && x <= ix1 {
					innerInk++
				}
			}
		}
		cropInk += rowInk
		// Text rows have some ink but stay mostly paper, and never hug the
		// crop edge the way background bleed does. A sheet with no such rows
		// is not a receipt.
		if y >= y0+2 && y <= y1-2 {
			if f := float64(innerInk) / float64(innerW); f >= 0.02 && f <= 0.85 {
				textRows++
			}
		}
	}
	if f := float64(cropInk) / float64(inW*inH); f < paperMinInk || f > paperMaxInk {
		return img, false
	}
	if textRows < 3 || textRows < inH/12 {
		return img, false
	}

	// Keep a small margin around the paper so no content hugs the edge.
	padX, padY := 4+w/64, 4+h/64
	return cropImage(img, image.Rect(
		b.Min.X+max(x0-padX, 0), b.Min.Y+max(y0-padY, 0),
		b.Min.X+min(x1+padX, w-1)+1, b.Min.Y+min(y1+padY, h-1)+1)), true
}

// cropImage extracts a rectangle, using the native SubImage of the standard
// image types and a pixel copy as a fallback for anything else.
func cropImage(img image.Image, r image.Rectangle) image.Image {
	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	if si, ok := img.(subImager); ok {
		return si.SubImage(r)
	}
	dst := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			dst.Set(x, y, img.At(r.Min.X+x, r.Min.Y+y))
		}
	}
	return dst
}

// deskewImage returns the image rotated so text lines are horizontal, or nil
// when no confident skew is found. The angle is searched on a downscaled
// greyscale copy; only the final correction touches full resolution.
func deskewImage(img image.Image) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	grey, _ := grayscalePlane(img)

	small, sw, sh := downscaleGray(grey, w, h, deskewPreview)
	angle, bestScore, straightScore := bestSkewAngle(small, sw, sh)
	if math.Abs(angle) < deskewMinAngle || bestScore < deskewMinImprove*straightScore {
		return nil
	}
	// A peak pressed against the search limit is brightness structure (a
	// two-tone background, a colour band), not aligned text; refuse to act.
	if math.Abs(angle) > deskewMaxAngle-deskewEdgeFrac {
		return nil
	}
	return rotateImage(img, angle)
}

// bestSkewAngle finds the small rotation under which the rows of dark pixels
// (text lines) line up most sharply. Sharp alignment maximises the variance
// of the per-row ink totals, so every candidate angle is scored by the sum
// of squared row sums over a fixed central window.
func bestSkewAngle(grey []uint8, w, h int) (bestDeg float64, bestScore, straightScore float64) {
	winW, winH := inscribedSize(w, h, deskewMaxAngle)
	score := func(deg float64) float64 {
		rot, rw, rh := rotateGrayWindow(grey, w, h, winW, winH, deg)
		var s float64
		for y := 0; y < rh; y++ {
			var rowInk float64
			for x := 0; x < rw; x++ {
				rowInk += float64(255 - rot[y*rw+x])
			}
			s += rowInk * rowInk
		}
		return s
	}

	bestDeg, bestScore, straightScore = 0, score(0), score(0)
	for d := -deskewMaxAngle; d <= deskewMaxAngle; d++ {
		if s := score(d); s > bestScore {
			bestDeg, bestScore = d, s
		}
	}
	coarse := bestDeg
	for t := -9; t <= 9; t++ {
		d := coarse + float64(t)/10
		if d < -deskewMaxAngle || d > deskewMaxAngle {
			continue
		}
		if s := score(d); s > bestScore {
			bestDeg, bestScore = d, s
		}
	}
	return bestDeg, bestScore, straightScore
}

// grayscalePlane converts to 8-bit luma plus per-pixel saturation
// (max channel minus min).
func grayscalePlane(img image.Image) (grey, sat []uint8) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	grey = make([]uint8, w*h)
	sat = make([]uint8, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			r8, g8, b8 := uint8(r>>8), uint8(g>>8), uint8(bl>>8)
			grey[y*w+x] = uint8((299*int(r8) + 587*int(g8) + 114*int(b8)) / 1000)
			sat[y*w+x] = max(r8, g8, b8) - min(r8, g8, b8)
		}
	}
	return grey, sat
}

// otsuThreshold picks the grey level separating the image's two dominant
// brightness modes and returns a decision threshold midway between the
// classes. The raw maximiser of the inter-class variance lands wherever the
// histogram gap begins, so the midpoint is what separates the modes.
func otsuThreshold(grey []uint8) (int, float64, float64) {
	var hist [256]int
	for _, v := range grey {
		hist[v]++
	}
	total := float64(len(grey))
	var sumAll float64
	for v, n := range hist {
		sumAll += float64(v) * float64(n)
	}
	var wLow, sumLow float64
	var lowMean, highMean float64
	best := -1.0
	for t := 0; t < 256; t++ {
		wLow += float64(hist[t])
		if wLow == 0 {
			continue
		}
		wHigh := total - wLow
		if wHigh == 0 {
			break
		}
		sumLow += float64(t) * float64(hist[t])
		lowMean = sumLow / wLow
		highMean = (sumAll - sumLow) / wHigh
		d := lowMean - highMean
		if v := wLow * wHigh * d * d; v > best {
			best = v
		}
	}
	return int((lowMean + highMean) / 2), lowMean, highMean
}

// longestRun finds the longest stretch of rows (or columns) whose paper
// fraction stays above paperRowShare, tolerating text lines and noise via a
// moving average. per is the pixel count that corresponds to full coverage
// (the image width when scanning rows, its height for columns). Runs shorter
// than a quarter of the image are rejected.
func longestRun(counts []int, per int) (lo, hi int, ok bool) {
	n := len(counts)
	pref := make([]int, n+1)
	for i, c := range counts {
		pref[i+1] = pref[i] + c
	}
	win := n/50 + 1
	inRun := make([]bool, n)
	for i := range inRun {
		a, b := max(i-win, 0), min(i+win+1, n)
		inRun[i] = float64(pref[b]-pref[a])/float64((b-a)*per) >= paperRowShare
	}
	best, cur, start, bestStart := 0, 0, 0, 0
	for i, v := range inRun {
		if v {
			if cur == 0 {
				start = i
			}
			cur++
			if cur > best {
				best, bestStart = cur, start
			}
		} else {
			cur = 0
		}
	}
	if best >= n/4 {
		return bestStart, bestStart + best - 1, true
	}
	return 0, 0, false
}

// downscaleGray box-averages a greyscale plane until its long edge is at most
// maxEdge.
func downscaleGray(grey []uint8, w, h, maxEdge int) ([]uint8, int, int) {
	k := (max(w, h) + maxEdge - 1) / maxEdge
	if k <= 1 {
		return grey, w, h
	}
	dw, dh := w/k, h/k
	out := make([]uint8, dw*dh)
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sum int
			for dy := 0; dy < k; dy++ {
				for dx := 0; dx < k; dx++ {
					sum += int(grey[(y*k+dy)*w+x*k+dx])
				}
			}
			out[y*dw+x] = uint8(sum / (k * k))
		}
	}
	return out, dw, dh
}

// inscribedSize gives the largest axis-aligned rectangle that stays inside a
// w×h image after a rotation of deg: the rectangle that, rotated back,
// touches all four sides.
func inscribedSize(w, h int, deg float64) (int, int) {
	t := deg * math.Pi / 180
	c, s := math.Cos(t), math.Sin(t)
	denom := c*c - s*s
	wo := int((float64(w)*c - float64(h)*s) / denom)
	ho := int((float64(h)*c - float64(w)*s) / denom)
	return max(wo, 8), max(ho, 8)
}

// rotateGrayWindow rotates grey by deg around the image centre, writing only
// the wo×ho inscribed window so out-of-bounds fill never influences scores.
func rotateGrayWindow(grey []uint8, w, h, wo, ho int, deg float64) ([]uint8, int, int) {
	t := deg * math.Pi / 180
	c, s := math.Cos(t), math.Sin(t)
	cx, cy := float64(w)/2, float64(h)/2
	out := make([]uint8, wo*ho)
	for y := 0; y < ho; y++ {
		dy := float64(y) - float64(ho)/2 + 0.5
		for x := 0; x < wo; x++ {
			dx := float64(x) - float64(wo)/2 + 0.5
			out[y*wo+x] = sampleGray(grey, w, h, cx+dx*c-dy*s, cy+dx*s+dy*c)
		}
	}
	return out, wo, ho
}

// rotateImage rotates img by deg around its centre and crops to the inscribed
// rectangle, so the corners cut off by the twist never show as fill.
func rotateImage(img image.Image, deg float64) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	wo, ho := inscribedSize(w, h, deg)
	dst := image.NewNRGBA(image.Rect(0, 0, wo, ho))
	t := deg * math.Pi / 180
	c, s := math.Cos(t), math.Sin(t)
	cx, cy := float64(w)/2, float64(h)/2
	for y := 0; y < ho; y++ {
		dy := float64(y) - float64(ho)/2 + 0.5
		for x := 0; x < wo; x++ {
			dx := float64(x) - float64(wo)/2 + 0.5
			dst.Set(x, y, sampleColor(img, b, cx+dx*c-dy*s, cy+dx*s+dy*c))
		}
	}
	return dst
}

func sampleGray(grey []uint8, w, h int, x, y float64) uint8 {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	if x0 < 0 || y0 < 0 || x0 >= w-1 || y0 >= h-1 {
		return 255 // outside counts as paper, i.e. no ink
	}
	fx, fy := x-float64(x0), y-float64(y0)
	i := y0*w + x0
	top := float64(grey[i])*(1-fx) + float64(grey[i+1])*fx
	bot := float64(grey[i+w])*(1-fx) + float64(grey[i+w+1])*fx
	return uint8(top*(1-fy) + bot*fy + 0.5)
}

func sampleColor(img image.Image, b image.Rectangle, x, y float64) color.RGBA {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	if x0 < 0 || y0 < 0 || x0 >= b.Dx()-1 || y0 >= b.Dy()-1 {
		return color.RGBA{R: 255, G: 255, B: 255, A: 255}
	}
	fx, fy := x-float64(x0), y-float64(y0)
	at := func(px, py int) [4]float64 {
		r, g, bl, a := img.At(b.Min.X+px, b.Min.Y+py).RGBA()
		return [4]float64{float64(r), float64(g), float64(bl), float64(a)}
	}
	tl, tr := at(x0, y0), at(x0+1, y0)
	bl, br := at(x0, y0+1), at(x0+1, y0+1)
	var out [4]uint8
	for i := 0; i < 4; i++ {
		top := tl[i]*(1-fx) + tr[i]*fx
		bot := bl[i]*(1-fx) + br[i]*fx
		v := (top*(1-fy) + bot*fy) / 257 // 16-bit back to 8-bit
		out[i] = uint8(v + 0.5)
	}
	return color.RGBA{R: out[0], G: out[1], B: out[2], A: out[3]}
}
