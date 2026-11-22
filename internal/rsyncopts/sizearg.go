package rsyncopts

import (
	"fmt"
	"math"
	"strings"
)

// parseSizeArg is rsync/options.c:parse_size_arg: a size such as "700",
// "8k", "1.5M", "2KB" (powers of 1000 with a B) or "1G-1" (adjusted by one
// byte). defSuf is the unit of a bare number, 'b' for bytes. maxValue < 0
// means no limit; unlimited0 lets 0 through a minValue above it. Only "."
// is taken as the decimal point: rsync also takes the locale's.
func parseSizeArg(arg string, defSuf byte, name string, minValue, maxValue int64, unlimited0 bool) (int64, error) {
	fail := func(what string, limitName string, limit int64) error {
		msg := fmt.Sprintf("--%s=%s is %s", name, arg, what)
		if limitName != "" && limit >= 0 {
			msg += fmt.Sprintf(" (%s: %d", limitName, limit)
			if unlimited0 && limitName == "min" {
				msg += " or 0 for unlimited"
			}
			msg += ")"
		}
		return fmt.Errorf("%s", msg)
	}
	invalid := func() error { return fail("invalid", "", -1) }
	tooLarge := func() error { return fail("too large", "max", maxValue) }

	sizeMax := maxValue
	if sizeMax < 0 {
		sizeMax = math.MaxInt64
	}

	i := 0
	for i < len(arg) && isDigit(arg[i]) {
		i++
	}
	wholeDigits := arg[:i]
	rest := arg[i:]
	var fracDigits string
	if strings.HasPrefix(rest, ".") {
		rest = rest[1:]
		j := 0
		for j < len(rest) && isDigit(rest[j]) {
			j++
		}
		fracDigits, rest = rest[:j], rest[j:]
	}
	if wholeDigits == "" && fracDigits == "" {
		return 0, invalid()
	}

	suf := defSuf
	if rest != "" && rest[0] != '+' && rest[0] != '-' {
		suf, rest = rest[0], rest[1:]
	}
	reps := strings.IndexByte("bkmgtp", lower(suf))
	if reps < 0 {
		return 0, invalid()
	}
	var mult uint64
	switch {
	case strings.HasPrefix(rest, "b") || strings.HasPrefix(rest, "B"):
		mult, rest = 1000, rest[1:]
	case rest == "" || rest[0] == '+' || rest[0] == '-':
		mult = 1024
	case len(rest) >= 2 && strings.EqualFold(rest[:2], "ib"):
		mult, rest = 1024, rest[2:]
	default:
		return 0, invalid()
	}
	unit := uint64(1)
	for range reps {
		if unit > math.MaxUint64/mult {
			return 0, tooLarge()
		}
		unit *= mult
	}
	adjustment := 0
	if len(rest) >= 2 && (rest[0] == '+' || rest[0] == '-') && rest[1] == '1' {
		adjustment = 1
		if rest[0] == '-' {
			adjustment = -1
		}
		rest = rest[2:]
	}
	if rest != "" {
		return 0, invalid()
	}

	allowed := uint64(sizeMax)
	if adjustment < 0 {
		allowed++
	}
	wholeLimit := allowed / unit
	var whole uint64
	for _, c := range []byte(wholeDigits) {
		d := uint64(c - '0')
		if whole > wholeLimit/10 || (whole == wholeLimit/10 && d > wholeLimit%10) {
			return 0, tooLarge()
		}
		whole = whole*10 + d
	}
	size := whole * unit
	// The fraction, folded right to left and rounded down to bytes
	// (scale_size_fraction).
	var frac uint64
	tens, ones := unit/10, unit%10
	for k := len(fracDigits) - 1; k >= 0; k-- {
		d := uint64(fracDigits[k] - '0')
		frac = tens*d + frac/10 + (ones*d+frac%10)/10
	}
	if frac > allowed-size {
		return 0, tooLarge()
	}
	size += frac
	switch {
	case adjustment < 0:
		if size == 0 {
			return 0, tooLarge()
		}
		size--
	case adjustment > 0:
		if size == uint64(sizeMax) {
			return 0, tooLarge()
		}
		size++
	}
	if size < uint64(max(minValue, 0)) && (!unlimited0 || size != 0) {
		return 0, fail("too small", "min", minValue)
	}
	return int64(size), nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}
