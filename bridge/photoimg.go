package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Photos are shrunk on the phone so each one fits in a single Worker row and
// stays sharp enough to read a schedule or a whiteboard.
const (
	photoMaxSide  = 2048
	photoMaxBytes = 1_000_000
	// A photo bigger than this is not downloaded at all.
	photoMaxSourceBytes = 40 << 20
	// Refuse absurd pixel counts before decoding (a 12 MP phone photo is fine).
	photoMaxPixels = 80_000_000
	photoMinSide   = 480
)

var errPhotoFormat = errors.New("this photo format can't be shown")

// preparedPhoto is a photo ready to upload.
type preparedPhoto struct {
	Data          []byte
	Mime          string
	Width, Height int
}

var passThroughMimes = map[string]string{
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"gif":  "image/gif",
	"webp": "image/webp",
}

// preparePhoto returns the photo as it is when it is small enough and needs no
// turning, and otherwise a turned, shrunk JPEG of at most photoMaxBytes.
func preparePhoto(src []byte) (*preparedPhoto, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", errPhotoFormat, http.DetectContentType(src))
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > photoMaxPixels {
		return nil, fmt.Errorf("%w (%dx%d pixels)", errPhotoFormat, cfg.Width, cfg.Height)
	}
	orient := 1
	if format == "jpeg" {
		orient = jpegOrientation(src)
	}
	if mime, ok := passThroughMimes[format]; ok && orient == 1 && len(src) <= photoMaxBytes &&
		max(cfg.Width, cfg.Height) <= photoMaxSide {
		return &preparedPhoto{Data: src, Mime: mime, Width: cfg.Width, Height: cfg.Height}, nil
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errPhotoFormat, err)
	}
	side := photoMaxSide
	for {
		small := orientImage(scaleToFit(img, side), orient)
		for _, q := range []int{85, 75, 65} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: q}); err != nil {
				return nil, err
			}
			if buf.Len() <= photoMaxBytes {
				b := small.Bounds()
				return &preparedPhoto{Data: buf.Bytes(), Mime: "image/jpeg", Width: b.Dx(), Height: b.Dy()}, nil
			}
		}
		if side <= photoMinSide {
			return nil, fmt.Errorf("%w (still too big at %d pixels)", errPhotoFormat, side)
		}
		side = max(side*3/4, photoMinSide)
	}
}

// scaleToFit draws img onto a white RGBA canvas whose longest side is at most
// side. White, so see-through PNGs and GIFs don't turn black as a JPEG.
func scaleToFit(img image.Image, side int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if long := max(w, h); long > side {
		w = max(1, w*side/long)
		h = max(1, h*side/long)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	if w == b.Dx() && h == b.Dy() {
		draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Over)
	} else {
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	}
	return dst
}

// orientImage turns or flips a picture the way its EXIF orientation says, so
// it is upright once the EXIF data is gone.
func orientImage(src *image.RGBA, orient int) *image.RGBA {
	if orient < 2 || orient > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if orient >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		for dx := 0; dx < dw; dx++ {
			var sx, sy int
			switch orient {
			case 2: // mirrored
				sx, sy = w-1-dx, dy
			case 3: // upside down
				sx, sy = w-1-dx, h-1-dy
			case 4: // mirrored upside down
				sx, sy = dx, h-1-dy
			case 5: // mirrored, turned
				sx, sy = dy, dx
			case 6: // needs a quarter turn clockwise
				sx, sy = dy, h-1-dx
			case 7: // mirrored, turned the other way
				sx, sy = w-1-dy, h-1-dx
			case 8: // needs a quarter turn counterclockwise
				sx, sy = w-1-dy, dx
			}
			si := src.PixOffset(sx, sy)
			di := dst.PixOffset(dx, dy)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

// jpegOrientation reads the EXIF orientation (1 to 8) from a JPEG, or 1.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		switch {
		case marker == 0xFF:
			i++
			continue
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			i += 2
			continue
		case marker == 0xDA || marker == 0xD9:
			// Image data starts: no EXIF before it.
			return 1
		}
		segLen := int(b[i+2])<<8 | int(b[i+3])
		if segLen < 2 || i+2+segLen > len(b) {
			return 1
		}
		seg := b[i+4 : i+2+segLen]
		if marker == 0xE1 && len(seg) >= 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + segLen
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + 12*k
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}
