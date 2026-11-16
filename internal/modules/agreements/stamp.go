package agreements

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Generated stamps: a signer types the text of their company stamp and the
// server draws it, like a rubber stamp, as a transparent PNG. It is a
// picture of a stamp (what a stamp on paper is), not a certificate seal: no
// key or certificate is involved.
//
//   - ROUND and OVAL: a double ring border, the top text along the top arc
//     and the bottom text along the bottom arc (each letter turned to follow
//     the curve), a ring around the centre, and the centre lines (and the
//     date) across the middle.
//   - RECTANGLE: a double border with the lines stacked: the top text
//     larger, the centre lines, the date, the bottom text.
//
// The text is set in DejaVu Sans Condensed Bold (vendored in fonts/ with its
// licence); a text with a letter it does not have is set in Go Bold instead,
// like the signature fonts fall back (fontFor). The ink is slightly uneven,
// as stamped ink is, but never so much that a letter is lost. Rendering is
// deterministic: the same stamp and date give the same bytes.

//go:embed fonts/DejaVuSansCondensed-Bold.ttf
var stampTTF []byte

var stampPrimary, stampFallback *sfnt.Font

func init() {
	var err error
	if stampPrimary, err = sfnt.Parse(stampTTF); err != nil {
		panic(fmt.Sprintf("stamp font: %v", err))
	}
	if stampFallback, err = sfnt.Parse(gobold.TTF); err != nil {
		panic(fmt.Sprintf("stamp fallback font: %v", err))
	}
}

// Stamp shapes and ink colours.
const (
	StampRound     = "ROUND"
	StampOval      = "OVAL"
	StampRectangle = "RECTANGLE"

	InkBlue   = "BLUE"
	InkRed    = "RED"
	InkBlack  = "BLACK"
	InkGreen  = "GREEN"
	InkPurple = "PURPLE"
)

// Stamp limits and the STAMP field's sizes (PDF points).
const (
	MaxStampTextRunes = 60
	MaxStampLineRunes = 30
	MaxStampLines     = 3
	// StampFieldSize is the default STAMP field: 40 mm square.
	StampFieldSize = 113.0
	// MinStampFieldSize is the smallest side of a STAMP field.
	MinStampFieldSize = 57.0
	// StampPreviewLongSide is the long side of a stamp image, in pixels.
	StampPreviewLongSide = 600
)

// Stamp is the text of a generated stamp, as the signer typed it (upper-cased).
type Stamp struct {
	Shape       string   `json:"shape"`
	Colour      string   `json:"colour"`
	TopText     string   `json:"top_text"`
	CentreLines []string `json:"centre_lines"`
	BottomText  string   `json:"bottom_text"`
	IncludeDate bool     `json:"include_date"`
}

func stampShapes() []string { return []string{StampRound, StampOval, StampRectangle} }

func inkColours() []string { return []string{InkBlue, InkRed, InkBlack, InkGreen, InkPurple} }

var inks = map[string]color.NRGBA{
	InkBlue:   {R: 28, G: 58, B: 158, A: 255},
	InkRed:    {R: 186, G: 28, B: 38, A: 255},
	InkBlack:  {R: 28, G: 28, B: 30, A: 255},
	InkGreen:  {R: 22, G: 114, B: 58, A: 255},
	InkPurple: {R: 96, G: 38, B: 138, A: 255},
}

func copyStamp(s *Stamp) *Stamp {
	if s == nil {
		return nil
	}
	c := *s
	c.CentreLines = append([]string{}, s.CentreLines...)
	return &c
}

// checkStamp validates a stamp; problems go to fields under key (for
// example "stamp.top_text"). The returned stamp is cleaned: trimmed,
// upper-cased, empty centre lines dropped, an empty shape ROUND and an empty
// colour BLUE.
func checkStamp(key string, in Stamp, fields map[string]string) Stamp {
	out := Stamp{Shape: strings.ToUpper(strings.TrimSpace(in.Shape)), Colour: strings.ToUpper(strings.TrimSpace(in.Colour)),
		IncludeDate: in.IncludeDate, CentreLines: []string{}}
	if out.Shape == "" {
		out.Shape = StampRound
	}
	if !slices.Contains(stampShapes(), out.Shape) {
		fields[key+".shape"] = "must be one of " + strings.Join(stampShapes(), ", ")
	}
	if out.Colour == "" {
		out.Colour = InkBlue
	}
	if !slices.Contains(inkColours(), out.Colour) {
		fields[key+".colour"] = "must be one of " + strings.Join(inkColours(), ", ")
	}
	top, problem := cleanText(in.TopText, MaxStampTextRunes, false)
	switch {
	case problem != "":
		fields[key+".top_text"] = problem
	case top == "":
		fields[key+".top_text"] = "is required: the name on the stamp"
	}
	out.TopText = strings.ToUpper(top)
	if len(in.CentreLines) > MaxStampLines {
		fields[key+".centre_lines"] = fmt.Sprintf("must have at most %d lines", MaxStampLines)
	}
	for i, line := range in.CentreLines {
		v, problem := cleanText(line, MaxStampLineRunes, false)
		if problem != "" {
			fields[key+".centre_lines["+strconv.Itoa(i)+"]"] = problem
			continue
		}
		if v != "" {
			out.CentreLines = append(out.CentreLines, strings.ToUpper(v))
		}
	}
	bottom, problem := cleanText(in.BottomText, MaxStampTextRunes, false)
	if problem != "" {
		fields[key+".bottom_text"] = problem
	}
	out.BottomText = strings.ToUpper(bottom)
	return out
}

// CheckStamp validates a stamp (fields under "stamp.") and returns it cleaned.
func CheckStamp(in Stamp) (Stamp, error) {
	fields := map[string]string{}
	out := checkStamp("stamp", in, fields)
	if len(fields) > 0 {
		return Stamp{}, &ValidationError{Fields: fields}
	}
	return out, nil
}

// StampPreview validates a stamp and renders it, dated date (YYYY-MM-DD;
// today when empty).
func (s *Service) StampPreview(in *Stamp, date string) ([]byte, error) {
	fields := map[string]string{}
	var clean Stamp
	if in == nil {
		fields["stamp"] = "is required"
	} else {
		clean = checkStamp("stamp", *in, fields)
	}
	day := s.clock()
	if strings.TrimSpace(date) != "" {
		d, err := time.Parse(time.DateOnly, strings.TrimSpace(date))
		if err != nil {
			fields["date"] = "must be a date (YYYY-MM-DD)"
		}
		day = d
	}
	if len(fields) > 0 {
		return nil, &ValidationError{Fields: fields}
	}
	return RenderStamp(clean, day)
}

// StampDate is how a stamp prints a date: "16 NOV 2026".
func StampDate(t time.Time) string { return strings.ToUpper(t.Format("2 Jan 2006")) }

func fontCovers(f *sfnt.Font, text string) bool {
	var buf sfnt.Buffer
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		if g, err := f.GlyphIndex(&buf, r); err != nil || g == 0 {
			return false
		}
	}
	return true
}

// stampFontFor returns the font a stamp text is set in: the condensed bold,
// or Go Bold when the condensed bold lacks one of its letters.
func stampFontFor(text string) *sfnt.Font {
	if fontCovers(stampPrimary, text) {
		return stampPrimary
	}
	return stampFallback
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

type pt struct{ x, y float64 }

type pathOp struct {
	kind byte // 'M', 'L', 'Q', 'C'
	p    [3]pt
}

type canvas struct {
	w, h int
	mask *image.Alpha
	buf  sfnt.Buffer
}

func newCanvas(w, h int) *canvas {
	return &canvas{w: w, h: h, mask: image.NewAlpha(image.Rect(0, 0, w, h))}
}

// fill rasterises one shape (its contours together, so that opposite
// windings make holes) onto the mask.
func (c *canvas) fill(ops []pathOp) {
	if len(ops) == 0 {
		return
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, op := range ops {
		n := map[byte]int{'M': 1, 'L': 1, 'Q': 2, 'C': 3}[op.kind]
		for _, p := range op.p[:n] {
			minX, minY, maxX, maxY = math.Min(minX, p.x), math.Min(minY, p.y), math.Max(maxX, p.x), math.Max(maxY, p.y)
		}
	}
	r := image.Rect(int(math.Floor(minX))-1, int(math.Floor(minY))-1, int(math.Ceil(maxX))+1, int(math.Ceil(maxY))+1).Intersect(c.mask.Bounds())
	if r.Empty() {
		return
	}
	z := vector.NewRasterizer(r.Dx(), r.Dy())
	ox, oy := float64(r.Min.X), float64(r.Min.Y)
	f := func(p pt) (float32, float32) { return float32(p.x - ox), float32(p.y - oy) }
	open := false
	for _, op := range ops {
		switch op.kind {
		case 'M':
			if open {
				z.ClosePath()
			}
			z.MoveTo(f(op.p[0]))
			open = true
		case 'L':
			z.LineTo(f(op.p[0]))
		case 'Q':
			bx, by := f(op.p[0])
			cx, cy := f(op.p[1])
			z.QuadTo(bx, by, cx, cy)
		case 'C':
			bx, by := f(op.p[0])
			cx, cy := f(op.p[1])
			dx, dy := f(op.p[2])
			z.CubeTo(bx, by, cx, cy, dx, dy)
		}
	}
	if open {
		z.ClosePath()
	}
	z.Draw(c.mask, r, image.Opaque, image.Point{})
}

// ellipsePath is an ellipse as a polygon; reverse winds it the other way
// (a hole when filled with an outer ellipse).
func ellipsePath(cx, cy, a, b float64, reverse bool) []pathOp {
	const n = 360
	ops := make([]pathOp, 0, n+1)
	for i := 0; i <= n; i++ {
		t := 2 * math.Pi * float64(i) / n
		if reverse {
			t = -t
		}
		kind := byte('L')
		if i == 0 {
			kind = 'M'
		}
		ops = append(ops, pathOp{kind: kind, p: [3]pt{{cx + a*math.Cos(t), cy + b*math.Sin(t)}}})
	}
	return ops
}

// ring fills the band between the ellipse (a, b) and the one width inside it.
func (c *canvas) ring(cx, cy, a, b, width float64) {
	c.fill(append(ellipsePath(cx, cy, a, b, false), ellipsePath(cx, cy, a-width, b-width, true)...))
}

func rectPath(x0, y0, x1, y1 float64, reverse bool) []pathOp {
	pts := []pt{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}}
	if reverse {
		pts = []pt{{x0, y0}, {x0, y1}, {x1, y1}, {x1, y0}}
	}
	ops := []pathOp{{kind: 'M', p: [3]pt{pts[0]}}}
	for _, p := range pts[1:] {
		ops = append(ops, pathOp{kind: 'L', p: [3]pt{p}})
	}
	return ops
}

// frame fills a rectangular border of that width inside (x0, y0)-(x1, y1).
func (c *canvas) frame(x0, y0, x1, y1, width float64) {
	c.fill(append(rectPath(x0, y0, x1, y1, false), rectPath(x0+width, y0+width, x1-width, y1-width, true)...))
}

// star fills a five-pointed star of outer radius r.
func (c *canvas) star(cx, cy, r float64) {
	var ops []pathOp
	for i := 0; i < 10; i++ {
		rr := r
		if i%2 == 1 {
			rr = r * 0.42
		}
		t := -math.Pi/2 + float64(i)*math.Pi/5
		kind := byte('L')
		if i == 0 {
			kind = 'M'
		}
		ops = append(ops, pathOp{kind: kind, p: [3]pt{{cx + rr*math.Cos(t), cy + rr*math.Sin(t)}}})
	}
	c.fill(ops)
}

// glyph is one letter of a text at a size: its outline (origin at the left
// end of the baseline, Y down) and advance.
type glyph struct {
	ops []pathOp
	adv float64
}

func (c *canvas) glyphs(f *sfnt.Font, text string, size float64) []glyph {
	ppem := fixed.Int26_6(math.Round(size * 64))
	var out []glyph
	for _, r := range text {
		gi, err := f.GlyphIndex(&c.buf, r)
		if err != nil {
			gi = 0
		}
		adv, err := f.GlyphAdvance(&c.buf, gi, ppem, font.HintingNone)
		if err != nil {
			continue
		}
		g := glyph{adv: float64(adv) / 64}
		segs, err := f.LoadGlyph(&c.buf, gi, ppem, nil)
		if err == nil {
			for _, s := range segs {
				op := pathOp{}
				switch s.Op {
				case sfnt.SegmentOpMoveTo:
					op.kind = 'M'
				case sfnt.SegmentOpLineTo:
					op.kind = 'L'
				case sfnt.SegmentOpQuadTo:
					op.kind = 'Q'
				case sfnt.SegmentOpCubeTo:
					op.kind = 'C'
				}
				for i := range 3 {
					op.p[i] = pt{float64(s.Args[i].X) / 64, float64(s.Args[i].Y) / 64}
				}
				g.ops = append(g.ops, op)
			}
		}
		out = append(out, g)
	}
	return out
}

// capHeight is the height of the capitals of f at size.
func (c *canvas) capHeight(f *sfnt.Font, size float64) float64 {
	m, err := f.Metrics(&c.buf, fixed.Int26_6(math.Round(size*64)), font.HintingNone)
	if err != nil || m.CapHeight <= 0 {
		return size * 0.72
	}
	return float64(m.CapHeight) / 64
}

// sizeForCap returns the font size whose capitals are cap high.
func (c *canvas) sizeForCap(f *sfnt.Font, cap float64) float64 {
	return cap * 100 / c.capHeight(f, 100)
}

// width is the advance of the glyphs with tracking between them.
func width(gs []glyph, tracking float64) float64 {
	w := 0.0
	for i, g := range gs {
		w += g.adv
		if i > 0 {
			w += tracking
		}
	}
	return w
}

// place draws a glyph rotated by angle (radians, clockwise on screen) with
// the middle of its baseline at base.
func (c *canvas) place(g glyph, base pt, angle float64) {
	sin, cos := math.Sincos(angle)
	ops := make([]pathOp, len(g.ops))
	for i, op := range g.ops {
		ops[i].kind = op.kind
		for j, p := range op.p {
			x, y := p.x-g.adv/2, p.y
			ops[i].p[j] = pt{base.x + x*cos - y*sin, base.y + x*sin + y*cos}
		}
	}
	c.fill(ops)
}

// line draws a text centred on (cx, baseline), shrunk to fit maxW; cap is
// the wanted capital height.
func (c *canvas) line(text string, cx, baseline, cap, maxW, trackingEm float64) {
	f := stampFontFor(text)
	size := c.sizeForCap(f, cap)
	gs := c.glyphs(f, text, size)
	if w := width(gs, size*trackingEm); w > maxW && w > 0 {
		size *= maxW / w
		gs = c.glyphs(f, text, size)
	}
	tracking := size * trackingEm
	x := cx - width(gs, tracking)/2
	for _, g := range gs {
		c.place(g, pt{x + g.adv/2, baseline}, 0)
		x += g.adv + tracking
	}
}

// arc measures an ellipse (semi-axes a, b) by arc length from its top
// (bottom=false) or bottom: t is the angle parameter from that point,
// positive towards the right.
type arc struct {
	cx, cy, a, b float64
	ts, ss       []float64 // t from -pi to pi and the arc length from t=0
}

func newArc(cx, cy, a, b float64) *arc {
	const n = 4000
	e := &arc{cx: cx, cy: cy, a: a, b: b}
	speed := func(t float64) float64 { s, c := math.Sincos(t); return math.Hypot(a*c, b*s) }
	e.ts, e.ss = make([]float64, 2*n+1), make([]float64, 2*n+1)
	step := math.Pi / n
	for i := 0; i <= n; i++ {
		t := float64(i) * step
		e.ts[n+i], e.ts[n-i] = t, -t
		if i > 0 {
			ds := (speed(t-step) + speed(t)) / 2 * step
			e.ss[n+i] = e.ss[n+i-1] + ds
			e.ss[n-i] = -e.ss[n+i]
		}
	}
	return e
}

// length is the arc length from -t to t.
func (e *arc) length(t float64) float64 {
	return 2 * e.at(t)
}

func (e *arc) at(t float64) float64 {
	i, _ := slices.BinarySearch(e.ts, t)
	return e.ss[min(i, len(e.ss)-1)]
}

// param returns the t at signed arc length s from t=0.
func (e *arc) param(s float64) float64 {
	i, _ := slices.BinarySearch(e.ss, s)
	return e.ts[min(i, len(e.ts)-1)]
}

// point is the point at t and the angle of the reading direction there.
func (e *arc) point(t float64, bottom bool) (pt, float64) {
	s, c := math.Sincos(t)
	if bottom {
		return pt{e.cx + e.a*s, e.cy + e.b*c}, math.Atan2(-e.b*s, e.a*c)
	}
	return pt{e.cx + e.a*s, e.cy - e.b*c}, math.Atan2(e.b*s, e.a*c)
}

// curved draws a text along the top (or bottom) of a band between two
// ellipses, its capitals cap high centred on the middle ellipse (semi-axes
// a, b), within an angle of maxT either side. It returns the t at which the
// text ends (half its extent).
func (c *canvas) curved(text string, cx, cy, a, b, cap, maxT float64, bottom bool) float64 {
	if text == "" {
		return 0
	}
	f := stampFontFor(text)
	size := c.sizeForCap(f, cap)
	const trackingEm = 0.1
	mid := newArc(cx, cy, a, b)
	gs := c.glyphs(f, text, size)
	if w, room := width(gs, size*trackingEm), mid.length(maxT); w > room && w > 0 {
		size *= room / w
		cap = c.capHeight(f, size)
		gs = c.glyphs(f, text, size)
	}
	// The baseline is inside the capitals' middle for the top text (letters
	// stand outwards) and outside it for the bottom text (letters stand
	// inwards): either way the letters' feet face the same ring.
	d := -cap / 2
	if bottom {
		d = cap / 2
	}
	base := newArc(cx, cy, a+d, b+d)
	tracking := size * trackingEm
	s := -width(gs, tracking) / 2
	for _, g := range gs {
		t := mid.param(s + g.adv/2)
		p, angle := base.point(t, bottom)
		c.place(g, p, angle)
		s += g.adv + tracking
	}
	return mid.param(width(gs, tracking) / 2)
}

// centre draws the centre lines and the date stacked in a box of height
// maxH around cy, each line at most chord(y) wide.
func (c *canvas) centre(lines []string, date string, cx, cy, maxH float64, chord func(y float64) float64) {
	type row struct {
		text string
		cap  float64
		date bool
	}
	var rows []row
	for _, l := range lines {
		rows = append(rows, row{text: l, cap: 40})
	}
	if date != "" {
		rows = append(rows, row{text: date, cap: 32, date: true})
	}
	if len(rows) == 0 {
		return
	}
	const gap = 0.55
	total := func() float64 {
		h := 0.0
		for i, r := range rows {
			h += r.cap
			if i > 0 {
				h += gap * max(r.cap, rows[i-1].cap)
			}
		}
		return h
	}
	if h := total(); h > maxH {
		for i := range rows {
			rows[i].cap *= maxH / h
		}
	}
	y := cy - total()/2
	for i, r := range rows {
		if i > 0 {
			y += gap * max(r.cap, rows[i-1].cap)
		}
		top, bottom := y, y+r.cap
		w := math.Min(chord(top), chord(bottom)) * 0.9
		if r.date {
			// A dater: the date between two rules.
			rule := math.Max(2, r.cap*0.08)
			rw := w * 0.92
			c.fill(rectPath(cx-rw/2, top-r.cap*0.32-rule, cx+rw/2, top-r.cap*0.32, false))
			c.fill(rectPath(cx-rw/2, bottom+r.cap*0.32, cx+rw/2, bottom+r.cap*0.32+rule, false))
			w *= 0.85
		}
		c.line(r.text, cx, bottom, r.cap, w, 0.06)
		y = bottom
	}
}

func (c *canvas) round(s Stamp, date string) {
	cx, cy := float64(c.w)/2, float64(c.h)/2
	a, b := cx-8, cy-8
	band := 82.0
	if s.Shape == StampOval {
		band = 70
	}
	// The double ring, then the ring around the centre.
	c.ring(cx, cy, a, b, 10)
	a2, b2 := a-15, b-15
	c.ring(cx, cy, a2, b2, 3.5)
	a3, b3 := a2-3.5-band, b2-3.5-band
	c.ring(cx, cy, a3, b3, 3.5)
	ma, mb := a3+band/2, b3+band/2
	cap := band * 0.52
	topMax, bottomMax := 150*math.Pi/180, 0.0
	if s.BottomText != "" {
		topMax, bottomMax = 100*math.Pi/180, 62*math.Pi/180
	}
	if s.Shape == StampOval {
		topMax = math.Min(topMax, 115*math.Pi/180)
	}
	topEnd := c.curved(s.TopText, cx, cy, ma, mb, cap, topMax, false)
	if s.BottomText != "" {
		bottomEnd := c.curved(s.BottomText, cx, cy, ma, mb, cap, bottomMax, true)
		// A star either side, halfway between the two texts.
		mid := newArc(cx, cy, ma, mb)
		t := (topEnd + (math.Pi - bottomEnd)) / 2
		for _, side := range []float64{1, -1} {
			p, _ := mid.point(side*t, false)
			c.star(p.x, p.y, cap*0.42)
		}
	}
	ia, ib := a3-3.5, b3-3.5
	c.centre(s.CentreLines, date, cx, cy, 2*ib*0.78, func(y float64) float64 {
		dy := (y - cy) / ib
		if math.Abs(dy) >= 1 {
			return 0
		}
		return 2 * ia * math.Sqrt(1-dy*dy)
	})
}

type rectRow struct {
	text string
	cap  float64
	date bool
}

// rectRows are the stacked lines of a rectangle stamp.
func rectRows(s Stamp, date string) []rectRow {
	rows := []rectRow{{text: s.TopText, cap: 46}}
	for _, l := range s.CentreLines {
		rows = append(rows, rectRow{text: l, cap: 34})
	}
	if date != "" {
		rows = append(rows, rectRow{text: date, cap: 30, date: true})
	}
	if s.BottomText != "" {
		rows = append(rows, rectRow{text: s.BottomText, cap: 30})
	}
	return rows
}

// rectGap is the space above row i (more around the date and its rules).
func rectGap(rows []rectRow, i int) float64 {
	g := 0.6 * max(rows[i].cap, rows[i-1].cap)
	if rows[i].date || rows[i-1].date {
		g += 12
	}
	return g
}

// rectContentHeight is the height of a rectangle stamp's stacked lines.
func rectContentHeight(s Stamp, date string) float64 {
	rows := rectRows(s, date)
	h := 0.0
	for i, r := range rows {
		h += r.cap
		if i > 0 {
			h += rectGap(rows, i)
		}
	}
	return h
}

func (c *canvas) rectangle(s Stamp, date string) {
	w, h := float64(c.w), float64(c.h)
	c.frame(4, 4, w-4, h-4, 10)
	c.frame(20, 20, w-20, h-20, 3.5)
	rows := rectRows(s, date)
	y := (h - rectContentHeight(s, date)) / 2
	maxW := w - 2*44
	for i, r := range rows {
		if i > 0 {
			y += rectGap(rows, i)
		}
		if r.date {
			rule := 2.5
			c.fill(rectPath(w/2-maxW*0.3, y-10-rule, w/2+maxW*0.3, y-10, false))
			c.fill(rectPath(w/2-maxW*0.3, y+r.cap+10, w/2+maxW*0.3, y+r.cap+10+rule, false))
		}
		c.line(r.text, w/2, y+r.cap, r.cap, maxW, 0.05)
		y += r.cap
	}
}

// ink turns the coverage mask into the stamp's colour, a little uneven.
func ink(mask *image.Alpha, colour color.NRGBA) *image.NRGBA {
	b := mask.Bounds()
	out := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			a := mask.AlphaAt(x, y).A
			if a == 0 {
				continue
			}
			// Two scales of smooth noise: the ink is 80 to 100 percent dense.
			n := 0.6*valueNoise(float64(x)/7, float64(y)/7, 1) + 0.4*valueNoise(float64(x)/29, float64(y)/29, 2)
			density := 0.8 + 0.2*n
			// Rare small voids, as where the rubber did not touch.
			if valueNoise(float64(x)/3, float64(y)/3, 3) > 0.93 {
				density *= 0.72
			}
			c := colour
			c.A = uint8(math.Round(float64(a) * density))
			out.SetNRGBA(x, y, c)
		}
	}
	return out
}

func hash2(x, y, seed int) float64 {
	h := uint32(x)*374761393 + uint32(y)*668265263 + uint32(seed)*2246822519
	h = (h ^ (h >> 13)) * 1274126177
	h ^= h >> 16
	return float64(h&0xffffff) / float64(0xffffff)
}

// valueNoise is smooth noise from 0 to 1.
func valueNoise(x, y float64, seed int) float64 {
	x0, y0 := math.Floor(x), math.Floor(y)
	fx, fy := x-x0, y-y0
	fx, fy = fx*fx*(3-2*fx), fy*fy*(3-2*fy)
	ix, iy := int(x0), int(y0)
	top := hash2(ix, iy, seed)*(1-fx) + hash2(ix+1, iy, seed)*fx
	bottom := hash2(ix, iy+1, seed)*(1-fx) + hash2(ix+1, iy+1, seed)*fx
	return top*(1-fy) + bottom*fy
}

// stampSize is the image size of a stamp in pixels.
func stampSize(s Stamp, date string) (int, int) {
	switch s.Shape {
	case StampOval:
		return StampPreviewLongSide, StampPreviewLongSide * 2 / 3
	case StampRectangle:
		h := int(math.Ceil(rectContentHeight(s, date))) + 2*48
		return StampPreviewLongSide, max(200, min(h, StampPreviewLongSide))
	}
	return StampPreviewLongSide, StampPreviewLongSide
}

// RenderStamp draws a stamp as a transparent PNG, about 600 pixels on its
// long side, dated date when the stamp includes the date. The stamp should
// have passed CheckStamp; the text is upper-cased again here.
func RenderStamp(s Stamp, date time.Time) ([]byte, error) {
	s.TopText, s.BottomText = strings.ToUpper(s.TopText), strings.ToUpper(s.BottomText)
	lines := make([]string, 0, len(s.CentreLines))
	for _, l := range s.CentreLines {
		lines = append(lines, strings.ToUpper(l))
	}
	s.CentreLines = lines
	day := ""
	if s.IncludeDate {
		day = StampDate(date)
	}
	colour, ok := inks[s.Colour]
	if !ok {
		colour = inks[InkBlue]
	}
	w, h := stampSize(s, day)
	c := newCanvas(w, h)
	if s.Shape == StampRectangle {
		c.rectangle(s, day)
	} else {
		c.round(s, day)
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, ink(c.mask, colour)); err != nil {
		return nil, fmt.Errorf("render stamp: %w", err)
	}
	return buf.Bytes(), nil
}
