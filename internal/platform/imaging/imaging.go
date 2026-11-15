// Package imaging holds the small image helpers shared by the uploads that
// are re-encoded before they are stored: profile logos and listing photos.
// Everything here is the standard library plus golang.org/x/image.
package imaging

import (
	"bytes"
	"encoding/binary"
	"image"
)

// JPEGOrientation returns the EXIF orientation of a JPEG (1 to 8), or 1 when
// it has none or it cannot be read.
func JPEGOrientation(content []byte) int {
	const none = 1
	for at := 2; at+4 <= len(content) && content[at] == 0xff; {
		marker, length := content[at+1], int(binary.BigEndian.Uint16(content[at+2:at+4]))
		if marker == 0xda || length < 2 || at+2+length > len(content) { // start of scan: no metadata after it
			return none
		}
		segment := content[at+4 : at+2+length]
		at += 2 + length
		if marker != 0xe1 || !bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			continue
		}
		tiff := segment[6:]
		if len(tiff) < 8 {
			return none
		}
		var order binary.ByteOrder = binary.BigEndian
		if string(tiff[:2]) == "II" {
			order = binary.LittleEndian
		}
		ifd := int(order.Uint32(tiff[4:8]))
		if ifd < 8 || ifd+2 > len(tiff) {
			return none
		}
		entries := int(order.Uint16(tiff[ifd : ifd+2]))
		for i := 0; i < entries; i++ {
			entry := ifd + 2 + 12*i
			if entry+12 > len(tiff) {
				return none
			}
			if order.Uint16(tiff[entry:entry+2]) == 0x0112 { // the Orientation tag
				if o := int(order.Uint16(tiff[entry+8 : entry+10])); o >= 1 && o <= 8 {
					return o
				}
				return none
			}
		}
		return none
	}
	return none
}

// Upright returns img turned the way its EXIF orientation says it is to be
// shown (orientation 1, or anything unknown, is img itself).
func Upright(img *image.RGBA, orientation int) *image.RGBA {
	if orientation < 2 || orientation > 8 {
		return img
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	if orientation >= 5 {
		out = image.NewRGBA(image.Rect(0, 0, h, w))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			tx, ty := x, y
			switch orientation {
			case 2:
				tx = w - 1 - x
			case 3:
				tx, ty = w-1-x, h-1-y
			case 4:
				ty = h - 1 - y
			case 5:
				tx, ty = y, x
			case 6:
				tx, ty = h-1-y, x
			case 7:
				tx, ty = h-1-y, w-1-x
			case 8:
				tx, ty = y, w-1-x
			}
			out.SetRGBA(tx, ty, img.RGBAAt(x+img.Bounds().Min.X, y+img.Bounds().Min.Y))
		}
	}
	return out
}

// Rotated reports whether an orientation swaps width and height.
func Rotated(orientation int) bool { return orientation >= 5 && orientation <= 8 }

// AnimatedPNG reports whether a PNG has an animation control chunk (acTL)
// before its image data, which is what makes it an animated PNG.
func AnimatedPNG(content []byte) bool {
	const signature = 8
	for at := signature; at+8 <= len(content); {
		length := int(binary.BigEndian.Uint32(content[at : at+4]))
		switch string(content[at+4 : at+8]) {
		case "acTL":
			return true
		case "IDAT", "IEND":
			return false
		}
		if length < 0 || at+12+length > len(content) {
			return false
		}
		at += 12 + length // length, type, data, CRC
	}
	return false
}
