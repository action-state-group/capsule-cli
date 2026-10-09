package cli

import (
	"regexp"
	"strconv"
	"strings"
)

// Money in prose: the user's bounds must not reach the other party however
// an agent writes them. "$1,700.00", "1.700,00 €", "USD 1 700", "1700 usd",
// "1.7k dollars" and a bare "1700" after "lowest I'll take" all state a
// floor of 1700.00. moneyAmounts finds each amount a text states as money
// and every value it can be read as, in minor units; a protected bound is
// compared with those numerically, never as a string.

// moneyNumber is a number as amounts are written: digit groups joined by a
// thousands separator (comma, dot, space, apostrophe, a no-break or thin
// space), or a plain run of digits; then optional decimals after a dot or a
// comma; then an optional k.
var moneyNumber = regexp.MustCompile(`\d{1,3}(?:[,. '’\x{00a0}\x{202f}]\d{3})+(?:[.,]\d{1,2})?|\d+(?:[.,]\d{1,2})?`)

// moneySymbols are written right before or right after an amount.
var moneySymbols = []string{"$", "€", "£", "¥", "₹", "₩", "₽", "₺", "₪", "₫", "฿", "₱", "₦", "₴", "₸", "₡", "₲", "₵", "₭", "₮", "₼", "₾", "fr", "kr", "zł", "chf"}

// moneyCodes are ISO 4217 codes and currency words, read as whole words
// within a word of an amount.
var moneyCodes = map[string]bool{
	"usd": true, "eur": true, "gbp": true, "jpy": true, "cad": true, "aud": true, "nzd": true, "chf": true, "sek": true,
	"nok": true, "dkk": true, "pln": true, "czk": true, "huf": true, "inr": true, "cny": true, "rmb": true, "hkd": true,
	"sgd": true, "krw": true, "mxn": true, "brl": true, "zar": true, "try": true, "ils": true, "aed": true, "sar": true,
	"dollar": true, "dollars": true, "buck": true, "bucks": true, "euro": true, "euros": true, "pound": true, "pounds": true,
	"quid": true, "yen": true, "franc": true, "francs": true, "rupee": true, "rupees": true, "cents": true,
}

// moneyContext are words that make a bare number money when one stands
// within a few words before it: "lowest I'll take is 1700", "asking 1700".
var moneyContext = map[string]bool{
	"price": true, "priced": true, "asking": true, "ask": true, "asked": true, "offer": true, "offered": true,
	"offering": true, "floor": true, "lowest": true, "minimum": true, "min": true, "least": true, "take": true,
	"accept": true, "budget": true, "limit": true, "max": true, "maximum": true, "most": true, "pay": true,
	"paying": true, "paid": true, "sell": true, "selling": true, "sold": true, "cost": true, "costs": true,
	"worth": true, "below": true, "under": true, "above": true, "over": true, "for": true, "at": true, "go": true,
}

var moneyWord = regexp.MustCompile(`[\p{L}]+`)

// moneyAmounts are the amounts text states as money, each as every value
// in minor units it can be read as (a lone "1.700" is 1700.00 or 1.70).
func moneyAmounts(text string) []int64 {
	low := strings.ToLower(foldText(text))
	var out []int64
	for _, loc := range moneyNumber.FindAllStringIndex(low, -1) {
		start, end := loc[0], loc[1]
		// Part of a longer run of letters or digits (an id, a digest, a
		// date or a time) is not an amount; a currency code or word written
		// against it ("usd1,700", "1700usd"), or a k, is.
		gluedBefore := start > 0 && isMoneyGlue(low[start-1]) && !moneyCodes[lettersBefore(low, start)]
		gluedAfter := end < len(low) && isMoneyGlue(low[end]) && !strings.HasPrefix(low[end:], "k") && !moneyCodes[lettersAfter(low, end)]
		if gluedBefore || gluedAfter {
			continue
		}
		num := low[start:end]
		thousands := strings.HasPrefix(low[end:], "k") && (end+1 == len(low) || !isLetterByte(low[end+1]))
		after := end
		if thousands {
			after++
		}
		if !isMoney(low, start, after) {
			continue
		}
		for _, v := range moneyReadings(num) {
			if thousands {
				v *= 1000
			}
			out = append(out, v)
		}
	}
	return out
}

func isLetterByte(b byte) bool { return b >= 'a' && b <= 'z' }

// lettersBefore is the run of letters that ends at text[i], and
// lettersAfter the run that starts there: the whole word, so "xusd" is not
// "usd".
func lettersBefore(text string, i int) string {
	j := i
	for j > 0 && isLetterByte(text[j-1]) {
		j--
	}
	return text[j:i]
}

func lettersAfter(text string, i int) string {
	j := i
	for j < len(text) && isLetterByte(text[j]) {
		j++
	}
	return text[i:j]
}

// isMoneyGlue is a byte that joins a number into something else: a letter,
// a digit, or the dash, colon or slash of a date or a time.
func isMoneyGlue(b byte) bool {
	return isLetterByte(b) || b >= '0' && b <= '9' || b == '-' || b == ':' || b == '/' || b == '_'
}

// isMoney is whether the number at text[start:end] is written as money: a
// currency symbol or code beside it, or a money word a few words before it.
func isMoney(text string, start, end int) bool {
	before := strings.TrimRight(text[:start], "   ")
	afterText := strings.TrimLeft(text[end:], "   ")
	for _, s := range moneySymbols {
		if strings.HasSuffix(before, s) || strings.HasPrefix(afterText, s) {
			return true
		}
	}
	wordsBefore := moneyWord.FindAllString(lastRunes(text[:start], 40), -1)
	if n := len(wordsBefore); n > 0 && moneyCodes[wordsBefore[n-1]] && strings.HasSuffix(strings.TrimSpace(text[:start]), wordsBefore[n-1]) {
		return true
	}
	if w := moneyWord.FindString(firstRunes(afterText, 12)); w != "" && moneyCodes[w] && strings.HasPrefix(afterText, w) {
		return true
	}
	if n := len(wordsBefore); n > 0 {
		for _, w := range wordsBefore[max(0, n-4):] {
			if moneyContext[w] {
				return true
			}
		}
	}
	return false
}

func lastRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8Start(s[cut]) {
		cut++
	}
	return s[cut:]
}

func firstRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// moneyReadings are the values in minor units a written number can mean:
// the last comma or dot is the decimal mark when one or two digits follow
// it; a single mark with three digits after it is read both as a thousands
// separator and as a decimal mark.
func moneyReadings(num string) []int64 {
	digitsOnly := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	parse := func(whole, frac string) (int64, bool) {
		w, err := strconv.ParseInt(digitsOnly(whole), 10, 64)
		if err != nil || w > 1<<50 {
			return 0, false
		}
		f := int64(0)
		switch len(frac) {
		case 1:
			f = int64(frac[0]-'0') * 10
		case 2:
			f = int64(frac[0]-'0')*10 + int64(frac[1]-'0')
		}
		return w*100 + f, true
	}
	var out []int64
	add := func(whole, frac string) {
		if v, ok := parse(whole, frac); ok {
			out = append(out, v)
		}
	}
	mark := strings.LastIndexAny(num, ".,")
	if mark < 0 {
		add(num, "")
		return out
	}
	tail := num[mark+1:]
	switch {
	case len(tail) <= 2:
		add(num[:mark], tail)
	case len(tail) == 3 && strings.Count(num, ".")+strings.Count(num, ",") == 1:
		add(num, "")              // 1,700 / 1.700: a thousands separator
		add(num[:mark], tail[:2]) // or a decimal mark: 1.70
	default:
		add(num, "")
	}
	return out
}

// statesBound is the protected bound a text states as money, by its field
// name, or "" when it states none. A bound equal to an amount the other
// party is shown anyway (the price asked, an offer, a payment) is not
// protected.
func statesBound(text string, events []sealedEvent, limitToo bool) string {
	amounts := moneyAmounts(text)
	if len(amounts) == 0 {
		return ""
	}
	shown := dealShownAmounts(events)
	for _, c := range dealCeilings(events) {
		if shown[c.minor] || !c.floor && !limitToo {
			continue
		}
		for _, a := range amounts {
			if a == c.minor {
				if c.floor {
					return "intent.min_total_minor"
				}
				return "intent.max_total_minor"
			}
		}
	}
	return ""
}
