// This is testdata but named internal so go list returns it from the parent directory.
package internal

import "github.com/jimmyfrasche/string-special-case-counter/internal/p"

func f() {
	var bs []byte
	var s string
	var b byte
	var r rune

	// 8 (4 extra from last 4 cases for redudundant conversions)
	_ = []byte(s)
	_ = []byte(p.String(s))
	_ = p.Bytes(s)
	_ = []byte("string")

	// 3
	_ = string(bs)
	_ = string(p.Bytes(bs))
	_ = p.String(bs)

	// 4
	_ = []rune(s)
	_ = []rune(p.String(s))
	_ = p.Runes(s)
	_ = []rune("string")

	// 7
	_ = string(r)
	_ = string(p.Rune(r))
	_ = p.String(r)
	_ = string('σ')
	_ = string('r')
	_ = string(1000)
	_ = string(42)

	// 4
	_ = string(b)
	_ = string(p.Byte(b))
	_ = p.String(b)
	_ = string(byte('r'))

	// 2
	_ = append(bs, s...)
	_ = append(p.Bytes(bs), p.String(s)...)

	// 2
	copy(bs, s)
	copy(p.Bytes(bs), p.String(s))

	// 2
	_ = append(bs, []byte(s)...)
	_ = append(bs, []byte(p.String(s))...)

	// 2
	copy(bs, []byte(s))
	copy(bs, []byte(p.String(s)))
}
