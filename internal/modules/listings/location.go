package listings

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Listing coordinates and map search (API 0.29.0, BR-002; FSD: listing
// records store "geocoordinates").
//
// PostGIS is NOT used: the project's Postgres is the official postgres:16
// image, without the postgis extension. Coordinates are two numeric(9,6)
// columns, a bounding box is a range on each (btree index), and
// Elasticsearch uses its own geo_point. No distance search is offered.

// Location precisions.
const (
	// PrecisionExact: the public coordinates are the stored ones.
	PrecisionExact = "EXACT"
	// PrecisionApproximate: the public coordinates are rounded to
	// ApproximateDecimals (about 1 km); the exact ones stay with the lister.
	PrecisionApproximate = "APPROXIMATE"
)

// MaxCoordinateDecimals is how many decimals a stored coordinate keeps
// (numeric(9,6): about 10 cm).
const MaxCoordinateDecimals = 6

// ApproximateDecimals is how many decimals the public coordinates of an
// APPROXIMATE listing keep (0.01 degree: about 1.1 km of latitude).
const ApproximateDecimals = 2

// Field messages of the coordinates.
const (
	MsgLatitudeRange    = "must be a number from -90 to 90"
	MsgLongitudeRange   = "must be a number from -180 to 180"
	MsgCoordinateNumber = "must be a number or null"
	MsgCoordinateDigits = "must have at most 6 decimal places"
	MsgCoordinatePair   = "is required with the other coordinate: send latitude and longitude together, or null for both"
	MsgNullIsland       = "0, 0 is not a property's location: pick the property's location"
	MsgPrecision        = "must be EXACT or APPROXIMATE"
)

// Location is the public position of a listing on a map. For an APPROXIMATE
// listing the coordinates are already rounded: the exact ones are never in
// a public response.
type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	// Precision is EXACT or APPROXIMATE.
	Precision string `json:"precision"`
}

// NormalPrecision is the stored form of a precision: "" (never set) is EXACT.
func NormalPrecision(p string) string {
	if p == "" {
		return PrecisionExact
	}
	return p
}

// micro is a coordinate in millionths of a degree (the stored unit).
func micro(v float64) int64 { return int64(math.Round(v * 1e6)) }

// RoundCoordinate rounds a coordinate to a number of decimals (0 to 6),
// half away from zero, on its decimal value (as Postgres round(numeric, n)
// does), so 1.005 is 1.01 whatever its binary form.
func RoundCoordinate(v float64, decimals int) float64 {
	if decimals >= MaxCoordinateDecimals {
		return float64(micro(v)) / 1e6
	}
	unit := int64(math.Pow10(MaxCoordinateDecimals - decimals))
	m := micro(v)
	sign := int64(1)
	if m < 0 {
		sign, m = -1, -m
	}
	q := (m + unit/2) / unit
	return float64(sign*q) / math.Pow10(decimals)
}

// publicLocation is the location the public sees, nil without coordinates.
func publicLocation(lat, lon *float64, precision string) *Location {
	if lat == nil || lon == nil {
		return nil
	}
	p := NormalPrecision(precision)
	l := &Location{Latitude: RoundCoordinate(*lat, MaxCoordinateDecimals), Longitude: RoundCoordinate(*lon, MaxCoordinateDecimals), Precision: p}
	if p == PrecisionApproximate {
		l.Latitude, l.Longitude = RoundCoordinate(*lat, ApproximateDecimals), RoundCoordinate(*lon, ApproximateDecimals)
	}
	return l
}

// PublicLocation is the record's public location (nil without coordinates).
func (r Record) PublicLocation() *Location {
	return publicLocation(r.Latitude, r.Longitude, r.LocationPrecision)
}

// coordinateFits reports whether a coordinate has at most six decimals.
func coordinateFits(v float64) bool {
	return math.Abs(v*1e6-math.Round(v*1e6)) < 1e-6
}

// parseCoordinate reads a JSON number of a PATCH. It returns the message of
// a field error, or "".
func parseCoordinate(raw json.RawMessage) (float64, string) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text[0] == '"' {
		return 0, MsgCoordinateNumber
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, MsgCoordinateNumber
	}
	r, ok := new(big.Rat).SetString(n.String())
	if !ok {
		return 0, MsgCoordinateNumber
	}
	if !new(big.Rat).Mul(r, big.NewRat(1e6, 1)).IsInt() {
		return 0, MsgCoordinateDigits
	}
	f, _ := r.Float64()
	if math.IsInf(f, 0) || math.IsNaN(f) || math.Abs(f) > 1000 {
		return 0, MsgCoordinateNumber
	}
	return f, ""
}

// validateLocation checks the coordinates of a content (the CHECK
// constraints of migration 0028, and (0, 0)).
func validateLocation(c Content, fields map[string]string) {
	switch NormalPrecision(c.LocationPrecision) {
	case PrecisionExact, PrecisionApproximate:
	default:
		fields["location_precision"] = MsgPrecision
	}
	switch {
	case c.Latitude == nil && c.Longitude == nil:
		return
	case c.Latitude == nil:
		fields["latitude"] = MsgCoordinatePair
		return
	case c.Longitude == nil:
		fields["longitude"] = MsgCoordinatePair
		return
	}
	lat, lon := *c.Latitude, *c.Longitude
	bad := false
	if math.IsNaN(lat) || lat < -90 || lat > 90 {
		fields["latitude"], bad = MsgLatitudeRange, true
	} else if !coordinateFits(lat) {
		fields["latitude"], bad = MsgCoordinateDigits, true
	}
	if math.IsNaN(lon) || lon < -180 || lon > 180 {
		fields["longitude"], bad = MsgLongitudeRange, true
	} else if !coordinateFits(lon) {
		fields["longitude"], bad = MsgCoordinateDigits, true
	}
	if !bad && micro(lat) == 0 && micro(lon) == 0 {
		fields["latitude"] = MsgNullIsland
	}
}

// BBox is a map area: west, south, east and north edges in decimal degrees
// (edges included). It never crosses the antimeridian (West < East).
type BBox struct {
	West, South, East, North float64
}

// Contains reports whether a point is inside the box (edges included).
func (b BBox) Contains(lat, lon float64) bool {
	return lat >= b.South && lat <= b.North && lon >= b.West && lon <= b.East
}

// MsgBBox is the field error of a bbox that is not four numbers.
const MsgBBox = "must be west,south,east,north in decimal degrees, such as 30.9,-17.9,31.2,-17.7"

// ParseBBox reads bbox=west,south,east,north. It returns the message of a
// field error, or "".
func ParseBBox(raw string) (*BBox, string) {
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		return nil, MsgBBox
	}
	var v [4]float64
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || len(p) > 32 {
			return nil, MsgBBox
		}
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, MsgBBox
		}
		v[i] = f
	}
	b := BBox{West: v[0], South: v[1], East: v[2], North: v[3]}
	switch {
	case b.West < -180 || b.West > 180 || b.East < -180 || b.East > 180:
		return nil, "west and east must be from -180 to 180"
	case b.South < -90 || b.South > 90 || b.North < -90 || b.North > 90:
		return nil, "south and north must be from -90 to 90"
	case b.South >= b.North:
		return nil, "south must be lower than north"
	case b.West >= b.East:
		return nil, "west must be lower than east (a box across the antimeridian is not supported)"
	}
	return &b, ""
}

// locationMatches applies bbox and has_location to a record, on its PUBLIC
// coordinates (an APPROXIMATE listing is where the public sees it).
func locationMatches(r Record, q Query) bool {
	if q.BBox == nil && !q.HasLocation {
		return true
	}
	l := r.PublicLocation()
	if l == nil {
		return false
	}
	return q.BBox == nil || q.BBox.Contains(l.Latitude, l.Longitude)
}
