package devseed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"

	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Sample logos. Every sample business gets a small logo drawn here, at seed
// time, with the standard library only: a coloured square with the business's
// initials in a tiny bitmap font written for this file. No third-party font
// or artwork is involved.
//
// Two things are deliberately left without one, so that clients exercise
// their fallbacks:
//
//   - NoLogoProfile, a sample agency with listings, has NO logo: a client
//     shows its initials;
//   - no sample AGENT has a photo: a client falls back to the agency's logo.

// NoLogoProfile is the one sample business ("kind/slug") without a logo.
const NoLogoProfile = "agencies/sample-realty"

// SampleLogoSize is the width and height of a sample logo, in pixels.
const SampleLogoSize = 256

// glyphs is a 5x7 dot-matrix alphabet: seven rows of five cells per letter.
var glyphs = map[rune][7]string{
	'A': {".###.", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'B': {"####.", "#...#", "#...#", "####.", "#...#", "#...#", "####."},
	'C': {".###.", "#...#", "#....", "#....", "#....", "#...#", ".###."},
	'D': {"####.", "#...#", "#...#", "#...#", "#...#", "#...#", "####."},
	'E': {"#####", "#....", "#....", "####.", "#....", "#....", "#####"},
	'F': {"#####", "#....", "#....", "####.", "#....", "#....", "#...."},
	'G': {".###.", "#...#", "#....", "#.###", "#...#", "#...#", ".###."},
	'H': {"#...#", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'I': {".###.", "..#..", "..#..", "..#..", "..#..", "..#..", ".###."},
	'J': {"..###", "...#.", "...#.", "...#.", "...#.", "#..#.", ".##.."},
	'K': {"#...#", "#..#.", "#.#..", "##...", "#.#..", "#..#.", "#...#"},
	'L': {"#....", "#....", "#....", "#....", "#....", "#....", "#####"},
	'M': {"#...#", "##.##", "#.#.#", "#.#.#", "#...#", "#...#", "#...#"},
	'N': {"#...#", "##..#", "#.#.#", "#..##", "#...#", "#...#", "#...#"},
	'O': {".###.", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'P': {"####.", "#...#", "#...#", "####.", "#....", "#....", "#...."},
	'Q': {".###.", "#...#", "#...#", "#...#", "#.#.#", "#..#.", ".##.#"},
	'R': {"####.", "#...#", "#...#", "####.", "#.#..", "#..#.", "#...#"},
	'S': {".####", "#....", "#....", ".###.", "....#", "....#", "####."},
	'T': {"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "..#.."},
	'U': {"#...#", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'V': {"#...#", "#...#", "#...#", "#...#", "#...#", ".#.#.", "..#.."},
	'W': {"#...#", "#...#", "#...#", "#.#.#", "#.#.#", "##.##", "#...#"},
	'X': {"#...#", "#...#", ".#.#.", "..#..", ".#.#.", "#...#", "#...#"},
	'Y': {"#...#", "#...#", ".#.#.", "..#..", "..#..", "..#..", "..#.."},
	'Z': {"#####", "....#", "...#.", "..#..", ".#...", "#....", "#####"},
}

// logoColours are the background colours of the sample logos, in turn.
var logoColours = []color.RGBA{
	{0x1f, 0x4f, 0xd8, 0xff}, {0x0f, 0x76, 0x6e, 0xff}, {0xb4, 0x53, 0x09, 0xff}, {0x7c, 0x3a, 0xed, 0xff},
	{0xbe, 0x12, 0x3c, 0xff}, {0x15, 0x80, 0x3d, 0xff}, {0x33, 0x41, 0x55, 0xff}, {0xa1, 0x62, 0x07, 0xff},
}

// Initials returns up to two letters for a name: the first letters of its
// first two words that start with a letter ("Sample Estates" -> "SE").
func Initials(name string) string {
	var out []rune
	for _, word := range strings.Fields(strings.ToUpper(name)) {
		if r := []rune(word)[0]; r >= 'A' && r <= 'Z' && len(out) < 2 {
			out = append(out, r)
		}
	}
	return string(out)
}

// DrawLogo draws a SampleLogoSize square PNG: a coloured square with a
// lighter frame and the initials in white. variant moves the frame inwards,
// so two businesses with the same initials and colour still differ. It is
// deterministic: the same arguments always give the same bytes (and the same
// checksum).
func DrawLogo(initials string, background color.RGBA, variant int) []byte {
	const (
		background0 = iota
		frame
		ink
	)
	lighter := color.RGBA{R: background.R + (0xff-background.R)/3, G: background.G + (0xff-background.G)/3, B: background.B + (0xff-background.B)/3, A: 0xff}
	img := image.NewPaletted(image.Rect(0, 0, SampleLogoSize, SampleLogoSize), color.Palette{background, lighter, color.RGBA{0xff, 0xff, 0xff, 0xff}})
	fill := func(x0, y0, x1, y1 int, index uint8) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.SetColorIndex(x, y, index)
			}
		}
	}
	fill(0, 0, SampleLogoSize, SampleLogoSize, background0)
	const line = 4
	margin := 12 + 6*(variant%4)
	fill(margin, margin, SampleLogoSize-margin, margin+line, frame)
	fill(margin, SampleLogoSize-margin-line, SampleLogoSize-margin, SampleLogoSize-margin, frame)
	fill(margin, margin, margin+line, SampleLogoSize-margin, frame)
	fill(SampleLogoSize-margin-line, margin, SampleLogoSize-margin, SampleLogoSize-margin, frame)

	letters := []rune(initials)
	const cell, gap = 16, 16 // a glyph is 5x7 cells
	width := len(letters)*5*cell + max(len(letters)-1, 0)*gap
	x0, y0 := (SampleLogoSize-width)/2, (SampleLogoSize-7*cell)/2
	for i, letter := range letters {
		for row, cells := range glyphs[letter] {
			for col, c := range cells {
				if c == '#' {
					x, y := x0+i*(5*cell+gap)+col*cell, y0+row*cell
					fill(x, y, x+cell, y+cell, ink)
				}
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// SampleLogo is the logo of one sample profile.
type SampleLogo struct {
	// Profile is the profile's "kind/slug".
	Profile string
	Logo    directory.Logo
	Content []byte
}

// sampleLogos draws every sample logo once.
var sampleLogos = sync.OnceValue(func() map[string]SampleLogo {
	out := map[string]SampleLogo{}
	n := 0
	for _, p := range Profiles() {
		if p.AgentUserID != nil || p.Key() == NoLogoProfile {
			continue
		}
		n++
		content := DrawLogo(Initials(p.Name), logoColours[n%len(logoColours)], n/len(logoColours))
		sum := sha256.Sum256(content)
		out[p.Key()] = SampleLogo{Profile: p.Key(), Content: content, Logo: directory.Logo{
			// A fixed key, so seeding again overwrites the same object.
			ObjectKey: fmt.Sprintf("%s/sample%02d", directory.LogoKeyPrefix, n), ContentType: "image/png",
			Width: SampleLogoSize, Height: SampleLogoSize, Checksum: hex.EncodeToString(sum[:]), Size: int64(len(content)), UpdatedAt: sampleEpoch,
		}}
	}
	return out
})

// SampleLogos returns the logo of every sample business that has one, by
// "kind/slug". Agents and NoLogoProfile have none.
func SampleLogos() map[string]SampleLogo { return sampleLogos() }

// SeedLogoFiles writes the sample logo images to the file storage. The
// profiles that point to them come from ProfileRecords.
func SeedLogoFiles(ctx context.Context, files storage.Storage) error {
	for _, l := range SampleLogos() {
		if _, err := directory.CheckLogo(l.Content); err != nil {
			return fmt.Errorf("sample logo of %s: %w", l.Profile, err)
		}
		if err := files.Put(ctx, l.Logo.ObjectKey, bytes.NewReader(l.Content), l.Logo.Size, l.Logo.ContentType); err != nil {
			return fmt.Errorf("seed logo of %s: %w", l.Profile, err)
		}
	}
	return nil
}
