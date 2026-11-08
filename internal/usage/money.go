package usage

import (
	"math"
	"strconv"
	"strings"
)

// FormatMoney is the server's form of a cost or budget, the same as the SPA's
// fmtMoney (ui/util.js): "¥1,234" for JPY (no decimals), "$1.23" for USD, "EUR 1.23"
// for any other code.
func FormatMoney(currency string, x float64) string {
	switch currency {
	case "JPY":
		return "¥" + groupThousands(strconv.FormatFloat(math.Round(x), 'f', 0, 64))
	case "USD":
		return "$" + groupThousands(strconv.FormatFloat(x, 'f', 2, 64))
	}
	return currency + " " + groupThousands(strconv.FormatFloat(x, 'f', 2, 64))
}

// groupThousands puts commas into the integer part of a formatted number.
func groupThousands(s string) string {
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	intPart, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if hasFrac {
		return sign + b.String() + "." + frac
	}
	return sign + b.String()
}
