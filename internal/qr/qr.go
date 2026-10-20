// Package qr encodes up to 213 bytes as ISO/IEC 18004 level-M QR codes, versions 1 to 10.
package qr

import (
	"fmt"
	"strings"
)

// Code is a QR code's modules, row by row, without the quiet zone.
type Code struct {
	Size    int
	Version int
	Mask    int

	dark     []bool
	function []bool // finder, timing, alignment, format and version modules
}

// Dark reports whether the module in column x, row y is dark.
func (c *Code) Dark(x, y int) bool { return c.dark[y*c.Size+x] }

// Table 9 counts codewords per block; second-group data blocks are one codeword longer.
var levelM = [11]struct{ ec, blocks1, data1, blocks2, data2 int }{
	{}, {10, 1, 16, 0, 0}, {16, 1, 28, 0, 0}, {26, 1, 44, 0, 0}, {18, 2, 32, 0, 0},
	{24, 2, 43, 0, 0}, {16, 4, 27, 0, 0}, {18, 4, 31, 0, 0}, {22, 2, 38, 2, 39},
	{22, 3, 36, 2, 37}, {26, 4, 43, 1, 44},
}

// Annex E specifies these alignment centres in module coordinates.
var alignment = [11][]int{nil, nil, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34},
	{6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50}}

func dataCodewords(version int) int {
	v := levelM[version]
	return v.blocks1*v.data1 + v.blocks2*v.data2
}

func countBits(version int) int {
	if version < 10 {
		return 8
	}
	return 16
}

func capacity(version int) int {
	return (dataCodewords(version)*8 - 4 - countBits(version)) / 8
}

// Encode makes the smallest code, up to version 10, that holds text.
func Encode(text string) (*Code, error) {
	data := []byte(text)
	version := 0
	for v := 1; v <= 10; v++ {
		if len(data) <= capacity(v) {
			version = v
			break
		}
	}
	if version == 0 {
		return nil, fmt.Errorf("qr: %d bytes is more than the %d a version 10 code holds", len(data), capacity(10))
	}
	c := newCode(version)
	c.place(withErrorCorrection(version, encodeData(version, data)))
	best, bestScore := 0, -1
	for m := 0; m < 8; m++ {
		c.applyMask(m)
		c.drawFormat(m)
		if s := c.penalty(); bestScore < 0 || s < bestScore {
			best, bestScore = m, s
		}
		c.applyMask(m) // the mask is an XOR: applying it again undoes it
	}
	c.applyMask(best)
	c.drawFormat(best)
	c.Mask = best
	return c, nil
}

func encodeData(version int, data []byte) []byte {
	var bits []bool
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, v>>i&1 == 1)
		}
	}
	put(0b0100, 4) // byte mode
	put(len(data), countBits(version))
	for _, b := range data {
		put(int(b), 8)
	}
	total := dataCodewords(version) * 8
	put(0, min(4, total-len(bits)))
	put(0, (8-len(bits)%8)%8)
	out := make([]byte, 0, dataCodewords(version))
	for i := 0; i < len(bits); i += 8 {
		var b byte
		for j := 0; j < 8; j++ {
			if bits[i+j] {
				b |= 0x80 >> j
			}
		}
		out = append(out, b)
	}
	for pad := byte(0xEC); len(out) < dataCodewords(version); pad ^= 0xEC ^ 0x11 {
		out = append(out, pad)
	}
	return out
}

func withErrorCorrection(version int, data []byte) []byte {
	v := levelM[version]
	gen := rsGenerator(v.ec)
	var blocks, ecc [][]byte
	for i, off := 0, 0; i < v.blocks1+v.blocks2; i++ {
		n := v.data1
		if i >= v.blocks1 {
			n = v.data2
		}
		blocks = append(blocks, data[off:off+n])
		ecc = append(ecc, rsRemainder(data[off:off+n], gen))
		off += n
	}
	var out []byte
	for i := 0; i < max(v.data1, v.data2); i++ {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := 0; i < v.ec; i++ {
		for _, e := range ecc {
			out = append(out, e[i])
		}
	}
	return out
}

// QR requires GF(256) with polynomial x^8+x^4+x^3+x^2+1.
func gfMul(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		z = z<<1 ^ (z>>7)*0x1D
		z ^= (y >> i & 1) * x
	}
	return z
}

// Coefficients are highest power first, with the implicit leading 1 omitted.
func rsGenerator(degree int) []byte {
	g := make([]byte, degree)
	g[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range g {
			g[j] = gfMul(g[j], root)
			if j+1 < degree {
				g[j] ^= g[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return g
}

func rsRemainder(data, gen []byte) []byte {
	r := make([]byte, len(gen))
	for _, b := range data {
		factor := b ^ r[0]
		copy(r, r[1:])
		r[len(r)-1] = 0
		for i := range r {
			r[i] ^= gfMul(gen[i], factor)
		}
	}
	return r
}

// Level M format information requires a BCH(15,5) remainder and the standard's XOR mask.
func formatBits(mask int) uint32 {
	data := uint32(0b00<<3 | mask) // level M is 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	return (data<<10 | rem&0x3FF) ^ 0x5412
}

func versionBits(version int) uint32 {
	rem := uint32(version)
	for i := 0; i < 12; i++ {
		rem = rem<<1 ^ (rem>>11)*0x1F25
	}
	return uint32(version)<<12 | rem&0xFFF
}

func newCode(version int) *Code {
	n := 17 + 4*version
	c := &Code{Size: n, Version: version, dark: make([]bool, n*n), function: make([]bool, n*n)}
	for i := 0; i < n; i++ { // timing patterns
		c.setFunction(6, i, i%2 == 0)
		c.setFunction(i, 6, i%2 == 0)
	}
	c.finder(3, 3)
	c.finder(n-4, 3)
	c.finder(3, n-4)
	pos := alignment[version]
	for i := range pos {
		for j := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
				continue // where the finders are
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					c.setFunction(pos[i]+dx, pos[j]+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	c.drawFormat(0) // reserves the format modules; Encode draws the real ones
	if version >= 7 {
		bits := versionBits(version)
		for i := 0; i < 18; i++ {
			dark := bits>>i&1 == 1
			a, b := n-11+i%3, i/3
			c.setFunction(a, b, dark)
			c.setFunction(b, a, dark)
		}
	}
	return c
}

func (c *Code) setFunction(x, y int, dark bool) {
	c.dark[y*c.Size+x] = dark
	c.function[y*c.Size+x] = true
}

func (c *Code) finder(x, y int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			xx, yy := x+dx, y+dy
			if xx >= 0 && xx < c.Size && yy >= 0 && yy < c.Size {
				d := max(abs(dx), abs(dy))
				c.setFunction(xx, yy, d != 2 && d != 4)
			}
		}
	}
}

func (c *Code) drawFormat(mask int) {
	bits := formatBits(mask)
	at := func(i int) bool { return bits>>i&1 == 1 }
	for i := 0; i <= 5; i++ {
		c.setFunction(8, i, at(i))
	}
	c.setFunction(8, 7, at(6))
	c.setFunction(8, 8, at(7))
	c.setFunction(7, 8, at(8))
	for i := 9; i < 15; i++ {
		c.setFunction(14-i, 8, at(i))
	}
	n := c.Size
	for i := 0; i < 8; i++ {
		c.setFunction(n-1-i, 8, at(i))
	}
	for i := 8; i < 15; i++ {
		c.setFunction(8, n-15+i, at(i))
	}
	c.setFunction(8, n-8, true) // always dark
}

// Leave remainder bits light and skip function modules in the standard's zigzag traversal.
func (c *Code) place(data []byte) {
	n, i := c.Size, 0
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // the vertical timing pattern takes column 6
		}
		for vert := 0; vert < n; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = n - 1 - vert // upward
				}
				if !c.function[y*n+x] && i < len(data)*8 {
					c.dark[y*n+x] = data[i>>3]>>(7-i&7)&1 == 1
					i++
				}
			}
		}
	}
}

func (c *Code) applyMask(mask int) {
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			var flip bool
			switch mask {
			case 0:
				flip = (x+y)%2 == 0
			case 1:
				flip = y%2 == 0
			case 2:
				flip = x%3 == 0
			case 3:
				flip = (x+y)%3 == 0
			case 4:
				flip = (x/3+y/2)%2 == 0
			case 5:
				flip = x*y%2+x*y%3 == 0
			case 6:
				flip = (x*y%2+x*y%3)%2 == 0
			case 7:
				flip = ((x+y)%2+x*y%3)%2 == 0
			}
			if flip && !c.function[y*c.Size+x] {
				c.dark[y*c.Size+x] = !c.dark[y*c.Size+x]
			}
		}
	}
}

func (c *Code) penalty() int {
	n, score, dark := c.Size, 0, 0
	line := func(get func(i int) bool) {
		run := 1
		for i := 1; i <= n; i++ {
			if i < n && get(i) == get(i-1) {
				run++
				continue
			}
			if run >= 5 {
				score += 3 + run - 5
			}
			run = 1
		}
		// 1:1:3:1:1 finder-like patterns with four light modules on a side.
		a := []bool{true, false, true, true, true, false, true, false, false, false, false}
		for i := 0; i+11 <= n; i++ {
			fwd, back := true, true
			for k := 0; k < 11; k++ {
				fwd = fwd && get(i+k) == a[k]
				back = back && get(i+k) == a[10-k]
			}
			if fwd {
				score += 40
			}
			if back {
				score += 40
			}
		}
	}
	for y := 0; y < n; y++ {
		line(func(i int) bool { return c.Dark(i, y) })
	}
	for x := 0; x < n; x++ {
		line(func(i int) bool { return c.Dark(x, i) })
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if c.Dark(x, y) {
				dark++
			}
			if x+1 < n && y+1 < n {
				d := c.Dark(x, y)
				if c.Dark(x+1, y) == d && c.Dark(x, y+1) == d && c.Dark(x+1, y+1) == d {
					score += 3
				}
			}
		}
	}
	pct := dark * 100 / (n * n)
	score += min(abs(pct-pct%5-50), abs(pct-pct%5+5-50)) / 5 * 10
	return score
}

// SVGPath draws dark modules with a quiet-module margin on a light square of Size+2*quiet units.
func (c *Code) SVGPath(quiet int) string {
	var b strings.Builder
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; {
			if !c.Dark(x, y) {
				x++
				continue
			}
			w := 1
			for x+w < c.Size && c.Dark(x+w, y) {
				w++
			}
			fmt.Fprintf(&b, "M%d,%dh%dv1h-%dz", x+quiet, y+quiet, w, w)
			x += w
		}
	}
	return b.String()
}

// Terminal draws light half-blocks on a dark background with a four-module quiet zone.
func (c *Code) Terminal() string {
	const quiet = 4
	n := c.Size + 2*quiet
	light := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		return x < 0 || y < 0 || x >= c.Size || y >= c.Size || !c.Dark(x, y)
	}
	var b strings.Builder
	for y := 0; y < n; y += 2 {
		for x := 0; x < n; x++ {
			top, bottom := light(x, y), y+1 < n && light(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
