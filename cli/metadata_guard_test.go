package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGuardMetadata(t *testing.T) {
	assert.NoError(t, GuardMetadata(map[string]string{"id": "exec-1", "tool": "shop.pay", "merchant_domain": "shop.example", "empty": ""}, "merchant_domain"))
	for name, fields := range map[string]map[string]string{
		"card number":     {"id": "4111 1111 1111 1111"},
		"phone":           {"task": "call+15551234567"},
		"form content":    {"tool": "pay with my card"},
		"bad host":        {"merchant_domain": "shop_example.com/"},
		"token as a host": {"merchant_domain": "-shop.example"},
	} {
		err := GuardMetadata(fields, "merchant_domain")
		if assert.Error(t, err, name) {
			for _, v := range fields {
				assert.NotContains(t, err.Error(), v, "%s: the refusal never echoes the value", name)
			}
		}
	}
	assert.True(t, IsCurrencyCode("usd"))
	assert.False(t, IsCurrencyCode("US$"))
}
