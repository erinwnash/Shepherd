package tui

import (
	"fmt"
	"strings"

	"rsc.io/qr"
)

// qrQuiet is the white margin, in modules, drawn around the code so scanners can find its edges.
const qrQuiet = 2

// qrBoxMinWidth keeps Box from falling back to its 80-column default for narrow content.
const qrBoxMinWidth = 40

// renderQR draws text as a QR code using half-block characters (two modules per terminal row).
// Colours are explicit (black on bright white) so the code scans on dark and light themes alike.
// It returns nil when the text cannot be encoded.
func renderQR(text string) []string {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return nil
	}
	size := code.Size
	dark := func(x, y int) bool {
		x -= qrQuiet
		y -= qrQuiet
		return x >= 0 && y >= 0 && x < size && y < size && code.Black(x, y)
	}
	total := size + 2*qrQuiet
	var out []string
	for y := 0; y < total; y += 2 {
		var b strings.Builder
		for x := 0; x < total; x++ {
			fg, bg := 97, 107 // light module
			if dark(x, y) {
				fg = 30
			}
			if dark(x, y+1) {
				bg = 40
			}
			fmt.Fprintf(&b, "\x1b[%d;%dm▀", fg, bg)
		}
		b.WriteString(Reset)
		out = append(out, b.String())
	}
	return out
}

// qrBox returns the QR panel for the ntfy topic and its width, or nil when there is nothing to show.
func qrBox(st DashboardState) ([]string, int) {
	if st.QRHidden || st.Status.NtfyTopic == "" {
		return nil, 0
	}
	body := renderQR(st.Status.NtfyTopic)
	if body == nil {
		return nil, 0
	}
	width := VisibleLen(body[0]) + 4
	if width < qrBoxMinWidth {
		width = qrBoxMinWidth
	}
	// Centre the code inside the panel.
	pad := strings.Repeat(" ", (width-4-VisibleLen(body[0]))/2)
	for i := range body {
		body[i] = pad + body[i]
	}
	body = append(body, Dim+"scan to subscribe in ntfy"+Reset)
	return Box("NTFY QR", body, width), width
}

// joinColumns places right beside left, padding the shorter block so columns stay aligned.
func joinColumns(left []string, leftWidth int, right []string, gap int) []string {
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	rightWidth := 0
	if len(right) > 0 {
		rightWidth = VisibleLen(right[0])
	}
	out := make([]string, n)
	for i := range out {
		l := strings.Repeat(" ", leftWidth)
		if i < len(left) {
			l = left[i]
			if pad := leftWidth - VisibleLen(l); pad > 0 {
				l += strings.Repeat(" ", pad)
			}
		}
		r := strings.Repeat(" ", rightWidth)
		if i < len(right) {
			r = right[i]
		}
		out[i] = l + strings.Repeat(" ", gap) + r
	}
	return out
}
