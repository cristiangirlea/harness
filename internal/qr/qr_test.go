package qr

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// The worked example in ISO/IEC 18004 and every tutorial after it: "HELLO
// WORLD" at 1-M, whose 16 data codewords get these 10 error correction ones.
func TestReedSolomonGivesTheStandardsHelloWorldCodewords(t *testing.T) {
	data := []byte{0x20, 0x5B, 0x0B, 0x78, 0xD1, 0x72, 0xDC, 0x4D, 0x43, 0x40, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11}
	want := []byte{0xC4, 0x23, 0x27, 0x77, 0xEB, 0xD7, 0xE7, 0xE2, 0x5D, 0x17}
	if got := rsRemainder(data, rsGenerator(10)); !bytes.Equal(got, want) {
		t.Fatalf("error correction = % X\nwant               % X", got, want)
	}
}

// The 15-bit format strings for level M from the standard's table.
func TestFormatBitsMatchThePublishedTable(t *testing.T) {
	for mask, want := range map[int]uint32{0: 0b101010000010010, 1: 0b101000100100101, 4: 0b100010111111001} {
		if got := formatBits(mask); got != want {
			t.Errorf("mask %d: %015b, want %015b", mask, got, want)
		}
	}
}

func TestVersionBitsMatchThePublishedTable(t *testing.T) {
	if got := versionBits(7); got != 0x07C94 {
		t.Errorf("version 7: %018b, want %018b", got, 0x07C94)
	}
}

func TestTheSmallestVersionThatHoldsTheTextIsChosen(t *testing.T) {
	for n, want := range map[int]int{1: 1, 14: 1, 15: 2, 62: 4, 63: 5, 122: 7, 213: 10} {
		c, err := Encode(strings.Repeat("a", n))
		if err != nil || c.Version != want || c.Size != 17+4*want {
			t.Errorf("%d bytes: version %v, %v; want %d", n, c, err, want)
		}
	}
	if _, err := Encode(strings.Repeat("a", 214)); err == nil {
		t.Error("214 bytes fit no version up to 10; want an error")
	}
}

// The fixed parts every reader looks for first.
func TestACodeHasItsFinderTimingAndFormatPatterns(t *testing.T) {
	for _, text := range []string{"http://192.168.1.40:8089/", strings.Repeat("x", 150)} {
		c, err := Encode(text)
		if err != nil {
			t.Fatal(err)
		}
		n := c.Size
		finder := []string{"#######", "#.....#", "#.###.#", "#.###.#", "#.###.#", "#.....#", "#######"}
		for _, corner := range [][2]int{{0, 0}, {n - 7, 0}, {0, n - 7}} {
			for dy, row := range finder {
				for dx, m := range row {
					if c.Dark(corner[0]+dx, corner[1]+dy) != (m == '#') {
						t.Fatalf("version %d: finder at %v wrong at (%d,%d)", c.Version, corner, dx, dy)
					}
				}
			}
		}
		for i := 8; i < n-8; i++ {
			if c.Dark(i, 6) != (i%2 == 0) || c.Dark(6, i) != (i%2 == 0) {
				t.Fatalf("version %d: timing pattern wrong at %d", c.Version, i)
			}
		}
		if !c.Dark(8, n-8) {
			t.Errorf("version %d: the module beside the bottom-left finder is always dark", c.Version)
		}
		// Both copies of the format information hold the mask that was chosen.
		var first, second uint32
		for i := 0; i <= 5; i++ {
			first |= bit(c.Dark(8, i)) << i
		}
		first |= bit(c.Dark(8, 7))<<6 | bit(c.Dark(8, 8))<<7 | bit(c.Dark(7, 8))<<8
		for i := 9; i < 15; i++ {
			first |= bit(c.Dark(14-i, 8)) << i
		}
		for i := 0; i < 8; i++ {
			second |= bit(c.Dark(n-1-i, 8)) << i
		}
		for i := 8; i < 15; i++ {
			second |= bit(c.Dark(8, n-15+i)) << i
		}
		if first != second || first != formatBits(c.Mask) {
			t.Errorf("version %d: format copies %015b and %015b; mask %d gives %015b", c.Version, first, second, c.Mask, formatBits(c.Mask))
		}
	}
}

func TestTheTerminalPictureHasAQuietZoneAndTwoRowsALine(t *testing.T) {
	c, err := Encode("hrn")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(c.Terminal(), "\n"), "\n")
	if want := (c.Size + 8 + 1) / 2; len(lines) != want {
		t.Fatalf("%d lines, want %d for %d rows with the quiet zone", len(lines), want, c.Size+8)
	}
	for _, l := range lines {
		if n := len([]rune(l)); n != c.Size+8 {
			t.Fatalf("a line is %d wide, want %d", n, c.Size+8)
		}
	}
	if strings.Trim(lines[0], "█") != "" || strings.Trim(lines[len(lines)-1], "█▀") != "" {
		t.Error("the quiet zone above and below is light")
	}
}

func TestTheSVGPathDrawsEveryDarkModuleOnce(t *testing.T) {
	c, err := Encode("hrn watch")
	if err != nil {
		t.Fatal(err)
	}
	dark := 0
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Dark(x, y) {
				dark++
			}
		}
	}
	// Each run is "M x,y h n v1 h-n z"; the widths add up to the dark modules.
	path, sum := c.SVGPath(4), 0
	for _, run := range strings.Split(path, "z")[:strings.Count(path, "z")] {
		var x, y, w, back int
		if _, err := fmtSscanf(run, &x, &y, &w, &back); err != nil || w != back || x < 4 || y < 4 {
			t.Fatalf("run %q: %v", run, err)
		}
		sum += w
	}
	if sum != dark {
		t.Fatalf("path covers %d modules, the code has %d dark", sum, dark)
	}
}

func fmtSscanf(run string, x, y, w, back *int) (int, error) {
	return fmt.Sscanf(run, "M%d,%dh%dv1h-%d", x, y, w, back)
}

func bit(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}
