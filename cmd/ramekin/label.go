package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/identity"
)

// recordUUID is a device's permanent UUID as far as its record knows: the
// one it reported, or, failing that, the one derived from its hardware ID.
func recordUUID(d deviceRecord) string {
	if identity.ValidUUID(d.UUID) {
		return d.UUID
	}
	return identity.DeviceUUID(d.HardwareID)
}

// deviceUUID finds a device's UUID: from its record, else by asking it
// (claimed devices sign their answer). It saves an answer it had to ask for.
func deviceUUID(ctx context.Context, d *deviceRecord) (string, error) {
	if u := recordUUID(*d); u != "" {
		return u, nil
	}
	st, _, err := d.status(ctx, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("%s's record has no UUID, and asking the device failed: %w", d.name, err)
	}
	if !identity.ValidUUID(st.UUID) {
		return "", fmt.Errorf("%s doesn't report a UUID; its ramekind is older than this ramekin (update its firmware, or run `ramekin device update %s`)", d.name, d.name)
	}
	d.UUID = st.UUID
	return st.UUID, d.save()
}

// deviceLabel writes a sticker for a device: a QR code of its short ID (see
// identity.ShortID), with the short ID printed under it.
func deviceLabel(ctx context.Context, args []string) error {
	fl := flags("device label")
	out := fl.String("o", "", "output file (default <device>-label.svg)")
	sizeArg := fl.String("size", defaultLabelSize, "label size, width x height, in in or mm (e.g. 1.5x0.75in, 38x19mm)")
	dpi := fl.Int("dpi", 300, "printer resolution: the QR code's modules are made whole printer dots")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ramekin device label <device> [-o file.svg] [-size WxH(in|mm)] [-dpi N]")
	}
	size, err := parseLabelSize(*sizeArg, *dpi)
	if err != nil {
		return err
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	d, err := resolveDevice(p, pos[0])
	if err != nil {
		return err
	}
	u, err := deviceUUID(ctx, &d)
	if err != nil {
		return err
	}
	svg, err := labelSVG(u, size)
	if err != nil {
		return err
	}
	warnShortIDClash(p, pos[0], u)
	if *out == "" {
		*out = pos[0] + "-label.svg"
	}
	if err := os.WriteFile(*out, []byte(svg), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s  %s  (UUID %s)\nwrote %s\n", pos[0], identity.ShortID(u), u, *out)
	return nil
}

// defaultLabelSize is the most common IT asset sticker.
const defaultLabelSize = "1.5x0.75in"

// labelSize is a label's size in printer dots.
type labelSize struct {
	W, H int    // dots
	DPI  int    // dots per inch
	Unit string // "in" or "mm", for the SVG's width and height
}

// parseLabelSize reads a size such as 1.5x0.75in or 38x19mm.
func parseLabelSize(s string, dpi int) (labelSize, error) {
	bad := fmt.Errorf("-size %q: want width x height in in or mm, e.g. %s or 38x19mm", s, defaultLabelSize)
	if dpi < 100 || dpi > 1200 {
		return labelSize{}, fmt.Errorf("-dpi %d: want a printer resolution between 100 and 1200", dpi)
	}
	unit, perInch := "", 0.0
	switch {
	case strings.HasSuffix(s, "in"):
		unit, perInch = "in", 1
	case strings.HasSuffix(s, "mm"):
		unit, perInch = "mm", 25.4
	default:
		return labelSize{}, bad
	}
	ws, hs, ok := strings.Cut(strings.TrimSuffix(s, unit), "x")
	w, err1 := strconv.ParseFloat(ws, 64)
	h, err2 := strconv.ParseFloat(hs, 64)
	if !ok || err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return labelSize{}, bad
	}
	size := labelSize{W: int(math.Round(w / perInch * float64(dpi))), H: int(math.Round(h / perInch * float64(dpi))), DPI: dpi, Unit: unit}
	if min(w, h)/perInch < 0.3 || max(w, h)/perInch > 20 {
		return labelSize{}, fmt.Errorf("-size %q: labels must be at least 0.3 in (7.6 mm) on their short side and at most 20 in", s)
	}
	return size, nil
}

func (l labelSize) svgLength(dots int) string {
	// Whole dots, so the printer maps the label 1:1 onto its dots; that can
	// differ from the size asked for by a fraction of a dot.
	v := float64(dots) / float64(l.DPI)
	if l.Unit == "mm" {
		v *= 25.4
	}
	return strconv.FormatFloat(math.Round(v*10000)/10000, 'f', -1, 64) + l.Unit
}

// qrPath draws a QR code as SVG path data: modules of m dots, at (x, y).
func qrPath(c *qr.Code, m, x, y int) string {
	var b strings.Builder
	for row := 0; row < c.Size; row++ {
		for col := 0; col < c.Size; {
			if !c.Black(col, row) {
				col++
				continue
			}
			run := 1
			for col+run < c.Size && c.Black(col+run, row) {
				run++
			}
			fmt.Fprintf(&b, "M%d %dh%dv%dh-%dz", x+col*m, y+row*m, run*m, m, run*m)
			col += run
		}
	}
	return b.String()
}

// qrQuiet is the margin a QR code needs around it, in modules.
const qrQuiet = 4

// labelSVG lays out a sticker for the device with UUID u: a QR code of its
// short ID, which fits the smallest QR code, and the short ID in large type.
// On a wide label (such as the default 1.5 x 0.75 in) the ID is to the right
// of the code, on two lines; otherwise it's under it. The code's modules are
// whole printer dots, so it prints sharp. The device's name isn't on it:
// names can change, and the short ID identifies the device.
func labelSVG(u string, l labelSize) (string, error) {
	id := identity.ShortID(u)
	if id == "" {
		return "", fmt.Errorf("%q isn't a UUID", u)
	}
	// Q: a quarter of the code can be scratched off a sticker and it still scans.
	c, err := qr.Encode(id, qr.Q)
	if err != nil {
		return "", err
	}
	modules := c.Size + 2*qrQuiet
	wide := l.W*2 >= l.H*3 // 1.5 x 0.75 in is 2:1
	var lines []string
	var m, qx, qy int // module size, and where the code (inside its quiet zone) goes
	var textX, textW, textY, textH float64
	if wide {
		m = l.H / modules
		qx, qy = (l.H-m*modules)/2, (l.H-m*modules)/2
		textX = float64(qx + m*modules)
		textW = float64(l.W) - textX - float64(m*qrQuiet)/2
		textY, textH = 0, float64(l.H)
		lines = []string{id[:6], id[6:]} // "Q3NFR-", "E51A1"
	} else {
		m = min(l.W, l.H*3/4) / modules
		qx, qy = (l.W-m*modules)/2, 0
		textX, textW = 0, float64(l.W)*0.92
		textY, textH = float64(m*modules), float64(l.H-m*modules)
		lines = []string{id}
		textX = (float64(l.W) - textW) / 2
	}
	if m < 1 {
		return "", fmt.Errorf("the label is too small for a QR code at %d dpi", l.DPI)
	}
	// The ID as large as fits its area: monospace glyphs are about 0.6em
	// wide, and lines are 1.15em apart.
	longest := 0
	for _, s := range lines {
		longest = max(longest, len(s))
	}
	fontSize := min(textW/(0.6*float64(longest)), textH*0.85/(1.15*float64(len(lines))))
	if fontSize < 4 {
		return "", errors.New("the label is too small to print the ID legibly")
	}
	block := fontSize * 1.15 * float64(len(lines))
	baseline := textY + (textH-block)/2 + fontSize*0.9
	var b strings.Builder
	// White behind everything, in two ways: some viewers ignore one or the
	// other (a percentage-sized rect came out a fraction of the label).
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%s" height="%s" viewBox="0 0 %d %d" style="background-color:#fff">`+"\n", l.svgLength(l.W), l.svgLength(l.H), l.W, l.H)
	fmt.Fprintf(&b, "<title>%s (UUID %s)</title>\n", id, u)
	fmt.Fprintf(&b, `<rect x="0" y="0" width="%d" height="%d" fill="#fff"/>`+"\n", l.W, l.H)
	fmt.Fprintf(&b, `<path fill="#000" shape-rendering="crispEdges" d="%s"/>`+"\n", qrPath(c, m, qx+qrQuiet*m, qy+qrQuiet*m))
	for i, s := range lines {
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="%.1f" font-weight="bold" font-family="monospace" text-anchor="middle" fill="#000">%s</text>`+"\n",
			textX+textW/2, baseline+float64(i)*fontSize*1.15, fontSize, s)
	}
	b.WriteString("</svg>\n")
	return b.String(), nil
}

// warnShortIDClash warns if another device in the project has the same
// short ID as this one (and a different UUID): labels couldn't tell them apart.
func warnShortIDClash(p *config.Project, name, u string) {
	uuids := map[string]string{name: u}
	for _, n := range deviceNames(p) {
		if d, err := resolveDevice(p, n); err == nil && n != name {
			uuids[n] = recordUUID(d)
		}
	}
	if names := identity.ShortIDClashes(uuids)[identity.ShortID(u)]; len(names) > 0 {
		fmt.Fprintf(os.Stderr, "warning: %s share the short ID %s; their labels can't be told apart (their UUIDs differ)\n", strings.Join(names, ", "), identity.ShortID(u))
	}
}

func mustLabelSize(s string, dpi int) labelSize {
	l, err := parseLabelSize(s, dpi)
	if err != nil {
		panic(err)
	}
	return l
}
