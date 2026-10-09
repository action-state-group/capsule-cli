package settlement

import (
	"encoding/json"
	"math/big"
	"regexp"
)

// valuePattern is the amount value grammar (§6): 0, or a digit 1-9 followed by
// digits. No sign, leading zero, decimal point, exponent or whitespace.
var valuePattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// Amount is an exact amount (§6): Value / 10^Scale units of Asset.
type Amount struct {
	Value *big.Int
	Asset string
	Scale int
}

// parseAmount reads an amount member. ok is false when the member is not an
// object of exactly value, assetCode and assetScale with value a string of the
// grammar and assetScale a JSON integer from 0 to 255 (amount_not_exact).
func parseAmount(raw any) (Amount, bool) {
	object, isObject := raw.(map[string]any)
	if !isObject || len(object) != 3 {
		return Amount{}, false
	}
	value, isString := object["value"].(string)
	if !isString || !valuePattern.MatchString(value) {
		return Amount{}, false
	}
	asset, isString := object["assetCode"].(string)
	if !isString || asset == "" {
		return Amount{}, false
	}
	scale, isScale := integerScale(object["assetScale"])
	if !isScale {
		return Amount{}, false
	}
	parsed, _ := new(big.Int).SetString(value, 10)
	return Amount{Value: parsed, Asset: asset, Scale: scale}, true
}

// integerScale accepts a JSON integer from 0 to 255 and nothing else: not a
// float, not a string, not a boolean.
func integerScale(raw any) (int, bool) {
	number, isNumber := raw.(json.Number)
	if !isNumber {
		return 0, false
	}
	text := number.String()
	if text == "0" {
		return 0, true
	}
	if !valuePattern.MatchString(text) || len(text) > 3 {
		return 0, false
	}
	scale := 0
	for _, digit := range text {
		scale = scale*10 + int(digit-'0')
	}
	return scale, scale <= 255
}

// at returns the amount's value at a scale no smaller than its own, exactly.
func (a Amount) at(scale int) *big.Int {
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale-a.Scale)), nil)
	return new(big.Int).Mul(a.Value, factor)
}

// Equal is §6 rules 3 and 4: the same asset string and the same value at the
// larger scale, in exact integer arithmetic.
func (a Amount) Equal(b Amount) bool {
	if a.Asset != b.Asset {
		return false
	}
	scale := max(a.Scale, b.Scale)
	return a.at(scale).Cmp(b.at(scale)) == 0
}

// amountRuleHolds is §9.2's amount rule: sent = received + receive fee, all in
// one asset, exactly at the largest scale of the three. There is no tolerance.
func amountRuleHolds(sent, received, receiveFee Amount) bool {
	if sent.Asset != received.Asset || received.Asset != receiveFee.Asset {
		return false
	}
	scale := max(sent.Scale, received.Scale, receiveFee.Scale)
	sum := new(big.Int).Add(received.at(scale), receiveFee.at(scale))
	return sent.at(scale).Cmp(sum) == 0
}
