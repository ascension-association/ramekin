package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"strings"
	"time"

	"rsc.io/qr"

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

// deviceLabel writes a sticker for a device: a QR code of its permanent
// UUID, with the UUID and the device's name printed under it.
func deviceLabel(ctx context.Context, args []string) error {
	fl := flags("device label")
	out := fl.String("o", "", "output file (default <device>-label.svg)")
	size := fl.Float64("size", 30, "label width in millimetres")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ramekin device label <device> [-o file.svg] [-size mm]")
	}
	if *size < 10 || *size > 500 {
		return errors.New("-size must be between 10 and 500 mm")
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
	svg, err := labelSVG(u, pos[0], *size)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = pos[0] + "-label.svg"
	}
	if err := os.WriteFile(*out, []byte(svg), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s  %s\nwrote %s\n", pos[0], u, *out)
	return nil
}

// qrSVG draws a QR code of text as SVG path data, one unit per module,
// offset by quiet modules on every side. It returns the path and the side
// length including the quiet zone.
func qrSVG(text string, quiet int) (path string, side int, err error) {
	// Q: a quarter of the code can be scratched off a sticker and it still scans.
	c, err := qr.Encode(text, qr.Q)
	if err != nil {
		return "", 0, err
	}
	var b strings.Builder
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; {
			if !c.Black(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < c.Size && c.Black(x+run, y) {
				run++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", x+quiet, y+quiet, run, run)
			x += run
		}
	}
	return b.String(), c.Size + 2*quiet, nil
}

// labelSVG lays out a sticker widthMM wide: the QR code, then the device
// name and its UUID on two lines.
func labelSVG(u, name string, widthMM float64) (string, error) {
	if !identity.ValidUUID(u) {
		return "", fmt.Errorf("%q isn't a UUID", u)
	}
	path, side, err := qrSVG(u, 4)
	if err != nil {
		return "", err
	}
	// Text below the code and its quiet zone (scanners need that margin
	// clear), in module units: the name, then the UUID on two lines, split
	// after its third group, keeping the hyphen so it reads back exactly.
	w := float64(side)
	nameSize, idSize := w/11, w/15
	nameY := w + nameSize*0.75
	id1Y := nameY + idSize*1.3
	id2Y := id1Y + idSize*1.15
	h := id2Y + idSize*0.7
	heightMM := widthMM * h / w
	first, rest := u[:19], u[19:]
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.2fmm" height="%.2fmm" viewBox="0 0 %d %.2f">`+"\n", widthMM, heightMM, side, h)
	fmt.Fprintf(&b, "<title>%s %s</title>\n", html.EscapeString(name), u)
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" fill="#fff"/>`+"\n")
	fmt.Fprintf(&b, `<path fill="#000" shape-rendering="crispEdges" d="%s"/>`+"\n", path)
	text := func(y, size float64, weight, family, s string) {
		fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" font-size="%.2f" font-weight="%s" font-family="%s" text-anchor="middle" fill="#000">%s</text>`+"\n",
			w/2, y, size, weight, family, html.EscapeString(s))
	}
	text(nameY, nameSize, "bold", "sans-serif", name)
	text(id1Y, idSize, "normal", "monospace", first)
	text(id2Y, idSize, "normal", "monospace", rest)
	b.WriteString("</svg>\n")
	return b.String(), nil
}
