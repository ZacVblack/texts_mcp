package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// withOrientation puts an EXIF block saying orient right after a JPEG's start.
func withOrientation(jpg []byte, orient uint16) []byte {
	var tiff bytes.Buffer
	tiff.WriteString("MM")
	binary.Write(&tiff, binary.BigEndian, uint16(42))
	binary.Write(&tiff, binary.BigEndian, uint32(8))
	binary.Write(&tiff, binary.BigEndian, uint16(1)) // one entry
	binary.Write(&tiff, binary.BigEndian, uint16(0x0112))
	binary.Write(&tiff, binary.BigEndian, uint16(3)) // SHORT
	binary.Write(&tiff, binary.BigEndian, uint32(1))
	binary.Write(&tiff, binary.BigEndian, orient)
	binary.Write(&tiff, binary.BigEndian, uint16(0))
	binary.Write(&tiff, binary.BigEndian, uint32(0)) // no next IFD
	seg := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write(jpg[:2])
	out.Write([]byte{0xFF, 0xE1, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)})
	out.Write(seg)
	out.Write(jpg[2:])
	return out.Bytes()
}

// redCorner is a 40x20 picture, red in its top-left 10x10 corner, blue elsewhere.
func redCorner() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			c := color.RGBA{0, 0, 255, 255}
			if x < 10 && y < 10 {
				c = color.RGBA{255, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

func isRed(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return r > 0xB000 && g < 0x5000 && b < 0x5000
}

func TestJpegOrientationTurnsPhotoUpright(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, redCorner(), &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	if got := jpegOrientation(buf.Bytes()); got != 1 {
		t.Fatalf("plain JPEG orientation = %d", got)
	}
	src := withOrientation(buf.Bytes(), 6)
	if got := jpegOrientation(src); got != 6 {
		t.Fatalf("orientation = %d, want 6", got)
	}
	p, err := preparePhoto(src)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mime != "image/jpeg" || p.Width != 20 || p.Height != 40 {
		t.Fatalf("got %s %dx%d, want image/jpeg 20x40", p.Mime, p.Width, p.Height)
	}
	out, err := jpeg.Decode(bytes.NewReader(p.Data))
	if err != nil {
		t.Fatal(err)
	}
	// A quarter turn clockwise moves the top-left corner to the top-right.
	if !isRed(out.At(15, 4)) || isRed(out.At(4, 4)) {
		t.Fatal("photo was not turned a quarter clockwise")
	}
}

func TestOrientImageAllEight(t *testing.T) {
	src := scaleToFit(redCorner(), 100) // 40x20 RGBA
	// Where the red corner's centre (5,5) ends up for each orientation.
	want := map[int]image.Point{
		1: {5, 5}, 2: {34, 5}, 3: {34, 14}, 4: {5, 14},
		5: {5, 5}, 6: {14, 5}, 7: {14, 34}, 8: {5, 34},
	}
	for o, pt := range want {
		out := orientImage(src, o)
		if !isRed(out.At(pt.X, pt.Y)) {
			t.Errorf("orientation %d: red corner not at %v", o, pt)
		}
		b := out.Bounds()
		if o >= 5 && (b.Dx() != 20 || b.Dy() != 40) {
			t.Errorf("orientation %d: size %dx%d, want 20x40", o, b.Dx(), b.Dy())
		}
	}
}

func TestPreparePhotoKeepsSmallPhotos(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, redCorner()); err != nil {
		t.Fatal(err)
	}
	p, err := preparePhoto(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if p.Mime != "image/png" || !bytes.Equal(p.Data, buf.Bytes()) || p.Width != 40 || p.Height != 20 {
		t.Fatalf("small PNG was changed: %s %dx%d", p.Mime, p.Width, p.Height)
	}
}

func TestPreparePhotoShrinksBigPhotos(t *testing.T) {
	// Noise doesn't compress, so this is far over the size limit.
	img := image.NewRGBA(image.Rect(0, 0, 3000, 2200))
	rand.New(rand.NewSource(1)).Read(img.Pix)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= photoMaxBytes {
		t.Fatalf("test photo too small: %d bytes", buf.Len())
	}
	p, err := preparePhoto(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if p.Mime != "image/jpeg" || len(p.Data) > photoMaxBytes || max(p.Width, p.Height) > photoMaxSide {
		t.Fatalf("got %s %d bytes %dx%d", p.Mime, len(p.Data), p.Width, p.Height)
	}
	if r := float64(p.Width) / float64(p.Height); r < 1.35 || r > 1.38 {
		t.Fatalf("shape changed: %dx%d", p.Width, p.Height)
	}
}

func TestPreparePhotoRejectsUnknownFormats(t *testing.T) {
	if _, err := preparePhoto([]byte("....ftypheic not really a photo")); err == nil {
		t.Fatal("junk accepted")
	}
}

func TestPhotoJobsFor(t *testing.T) {
	m := &gmproto.Message{
		MessageID:      "m1",
		ConversationID: "781",
		Timestamp:      1_700_000_000_123_456,
		MessageInfo: []*gmproto.MessageInfo{
			{Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: "This is our schedule"}}},
			{Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{MimeType: "video/mp4", MediaID: "v", DecryptionKey: []byte{1}}}},
			{Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{MimeType: "image/jpeg", MediaID: "img", DecryptionKey: []byte{2}}}},
			{Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{MimeType: "image/png"}}}, // still downloading
		},
	}
	jobs := photoJobsFor(m)
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1: %+v", len(jobs), jobs)
	}
	j := jobs[0]
	if j.ID != "m1.1" || j.MessageID != "m1" || j.ConversationID != "781" || j.MediaID != "img" || j.TsMs != 1_700_000_000_123 {
		t.Fatalf("bad job %+v", j)
	}
}

func TestPhotoQueueKeepsRecentPhotosAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "photos-pending.json")
	q := newPhotoQueue(path, zerolog.Nop())
	now := time.Now()
	old := photoJob{ID: "old.0", TsMs: now.Add(-(photoMaxAgeDays + 1) * 24 * time.Hour).UnixMilli(), MediaID: "a", Key: []byte{1}}
	newer := photoJob{ID: "new.0", TsMs: now.Add(-time.Hour).UnixMilli(), MediaID: "b", Key: []byte{2}}
	newest := photoJob{ID: "newest.0", TsMs: now.UnixMilli(), MediaID: "c", Key: []byte{3}}
	q.Add(old, newer, newest)
	if q.Pending() != 2 {
		t.Fatalf("pending = %d, want 2 (old photo skipped)", q.Pending())
	}
	if got := q.next(10); len(got) != 2 || got[0].ID != "newest.0" {
		t.Fatalf("next = %+v, want newest first", got)
	}

	q2 := newPhotoQueue(path, zerolog.Nop())
	if q2.Pending() != 2 {
		t.Fatalf("after restart pending = %d, want 2", q2.Pending())
	}
	q2.finish("newest.0")
	q2.Add(newest) // already done: not queued again
	if q2.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", q2.Pending())
	}
	for i := 0; i < photoMaxTries-1; i++ {
		if q2.retry("new.0") {
			t.Fatalf("dropped after %d tries", i+1)
		}
	}
	if !q2.retry("new.0") || q2.Pending() != 0 {
		t.Fatal("photo not dropped after the last try")
	}
}

func TestNilPhotoQueueIsSafe(t *testing.T) {
	var q *photoQueue
	q.Add(photoJob{ID: "x.0", TsMs: time.Now().UnixMilli()})
	if q.Pending() != 0 {
		t.Fatal("nil queue has pending photos")
	}
}

func TestPreparePhotoTurnsHEICIntoJPEG(t *testing.T) {
	src, err := os.ReadFile("testdata/test.heic")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	p, err := preparePhoto(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("HEIC %dx%d prepared in %v", p.Width, p.Height, time.Since(start))
	if p.Mime != "image/jpeg" || p.Width <= 0 || p.Height <= 0 {
		t.Fatalf("got %s %dx%d", p.Mime, p.Width, p.Height)
	}
	if _, err := jpeg.Decode(bytes.NewReader(p.Data)); err != nil {
		t.Fatal(err)
	}

	// The same photo with a generic brand first, as some phones write it.
	generic := append([]byte(nil), src...)
	copy(generic[8:12], "mif1")
	if _, err := preparePhoto(generic); err != nil {
		t.Fatalf("mif1 brand: %v", err)
	}
}
