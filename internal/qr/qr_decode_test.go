package qr

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// decode reads a code back the way a reader does, from the modules alone:
// the format information gives the mask, the mask comes off by the
// standard's own formulas (written here apart from the encoder's), the
// codewords come out of the zigzag, the blocks come apart, each block's error
// correction must match its data, the leftover modules must be light, and
// the data must be byte mode with its count.
func decode(c *Code) (string, error) {
	n := c.Size
	var format uint32
	for i := 0; i <= 5; i++ {
		format |= bit(c.Dark(8, i)) << i
	}
	format |= bit(c.Dark(8, 7))<<6 | bit(c.Dark(8, 8))<<7 | bit(c.Dark(7, 8))<<8
	for i := 9; i < 15; i++ {
		format |= bit(c.Dark(14-i, 8)) << i
	}
	mask := -1
	for m := 0; m < 8; m++ {
		if formatBits(m) == format {
			mask = m
		}
	}
	if mask < 0 {
		return "", fmt.Errorf("format %015b is no level-M format string", format)
	}
	masked := func(i, j int) bool { // i row, j column, as the standard writes them
		switch mask {
		case 0:
			return (i+j)%2 == 0
		case 1:
			return i%2 == 0
		case 2:
			return j%3 == 0
		case 3:
			return (i+j)%3 == 0
		case 4:
			return (i/2+j/3)%2 == 0
		case 5:
			return (i*j)%2+(i*j)%3 == 0
		case 6:
			return ((i*j)%2+(i*j)%3)%2 == 0
		default:
			return ((i+j)%2+(i*j)%3)%2 == 0
		}
	}
	function := newCode(c.Version).function
	var bits []bool
	upward := true
	for right := n - 1; right > 0; right -= 2 {
		if right == 6 {
			right--
		}
		for k := 0; k < n; k++ {
			row := k
			if upward {
				row = n - 1 - k
			}
			for _, col := range []int{right, right - 1} {
				if !function[row*n+col] {
					bits = append(bits, c.Dark(col, row) != masked(row, col))
				}
			}
		}
		upward = !upward
	}
	v := levelM[c.Version]
	total := v.blocks1*(v.data1+v.ec) + v.blocks2*(v.data2+v.ec)
	if len(bits) < total*8 {
		return "", fmt.Errorf("%d data modules for %d codewords", len(bits), total)
	}
	for _, b := range bits[total*8:] {
		if b {
			return "", fmt.Errorf("a remainder bit is dark")
		}
	}
	codewords := make([]byte, total)
	for i := range codewords {
		for j := 0; j < 8; j++ {
			if bits[i*8+j] {
				codewords[i] |= 0x80 >> j
			}
		}
	}
	nb := v.blocks1 + v.blocks2
	data := make([][]byte, nb)
	k := 0
	for i := 0; i < max(v.data1, v.data2); i++ {
		for b := 0; b < nb; b++ {
			if b < v.blocks1 && i >= v.data1 {
				continue
			}
			data[b] = append(data[b], codewords[k])
			k++
		}
	}
	ecc := make([][]byte, nb)
	for i := 0; i < v.ec; i++ {
		for b := 0; b < nb; b++ {
			ecc[b] = append(ecc[b], codewords[k])
			k++
		}
	}
	var all []byte
	for b := 0; b < nb; b++ {
		if !bytes.Equal(rsRemainder(data[b], rsGenerator(v.ec)), ecc[b]) {
			return "", fmt.Errorf("block %d's error correction does not match its data", b)
		}
		all = append(all, data[b]...)
	}
	read := func(at, n int) int {
		v := 0
		for i := 0; i < n; i++ {
			v = v<<1 | int(all[(at+i)/8]>>(7-(at+i)%8)&1)
		}
		return v
	}
	if read(0, 4) != 0b0100 {
		return "", fmt.Errorf("mode %04b, not byte mode", read(0, 4))
	}
	count := read(4, countBits(c.Version))
	out := make([]byte, count)
	for i := range out {
		out[i] = byte(read(4+countBits(c.Version)+8*i, 8))
	}
	return string(out), nil
}

// withMask is the code with another mask, for decoding every mask.
func withMask(c *Code, m int) *Code {
	cc := *c
	cc.dark = append([]bool(nil), c.dark...)
	cc.applyMask(c.Mask)
	cc.applyMask(m)
	cc.drawFormat(m)
	cc.Mask = m
	return &cc
}

func TestEveryLengthDecodesBackUnderEveryMask(t *testing.T) {
	for n := 0; n <= capacity(10); n++ {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(byte(' ' + (i*7+n)%95))
		}
		text := b.String()
		c, err := Encode(text)
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		for m := 0; m < 8; m++ {
			got, err := decode(withMask(c, m))
			if err != nil || got != text {
				t.Fatalf("%d bytes, version %d, mask %d: %q, %v", n, c.Version, m, got, err)
			}
		}
	}
}

func TestVersionBitsForEightToTenMatchThePublishedTable(t *testing.T) {
	for v, want := range map[int]uint32{8: 0x085BC, 9: 0x09A99, 10: 0x0A4D3} {
		if got := versionBits(v); got != want {
			t.Errorf("version %d: %018b, want %018b", v, got, want)
		}
	}
}

func TestEmptyTextIsAVersionOneCodeThatSaysSo(t *testing.T) {
	c, err := Encode("")
	if err != nil || c.Version != 1 {
		t.Fatalf("Encode(\"\") = %v, %v", c, err)
	}
	if got, err := decode(c); err != nil || got != "" {
		t.Fatalf("decoded %q, %v", got, err)
	}
}
