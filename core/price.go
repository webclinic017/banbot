package core

import (
	"fmt"
	"github.com/banbox/banexg"
	"strings"
)

func GetPrice(symbol string) float64 {
	if strings.Contains(symbol, "USD") && !strings.Contains(symbol, "/") {
		return 1
	}
	lockPrices.RLock()
	price, ok := prices[symbol]
	lockPrices.RUnlock()
	if ok {
		return price
	}
	lockBarPrices.RLock()
	price, ok = barPrices[symbol]
	lockBarPrices.RUnlock()
	if ok {
		return price
	}
	panic(fmt.Errorf("invalid symbol for price: %s", symbol))
}

func setDataPrice(data map[string]float64, pair string, price float64) {
	data[pair] = price
	base, quote, settle, _ := SplitSymbol(pair)
	if strings.Contains(quote, "USD") && (settle == "" || settle == quote) {
		data[base] = price
	}
}

func SetBarPrice(pair string, price float64) {
	lockBarPrices.Lock()
	setDataPrice(barPrices, pair, price)
	lockBarPrices.Unlock()
}

func SetPrice(pair string, price float64) {
	lockPrices.Lock()
	setDataPrice(prices, pair, price)
	lockPrices.Unlock()
}

func IsPriceEmpty() bool {
	lockPrices.RLock()
	lockBarPrices.RLock()
	empty := len(prices) == 0 && len(barPrices) == 0
	lockBarPrices.RUnlock()
	lockPrices.RUnlock()
	return empty
}

func SetPrices(data map[string]float64) {
	lockPrices.Lock()
	for pair, price := range data {
		prices[pair] = price
		base, quote, settle, _ := SplitSymbol(pair)
		if strings.Contains(quote, "USD") && (settle == "" || settle == quote) {
			prices[base] = price
		}
	}
	lockPrices.Unlock()
}

func IsMaker(pair, side string, price float64) bool {
	curPrice := GetPrice(pair)
	isBuy := side == banexg.OdSideBuy
	isLow := price < curPrice
	return isBuy == isLow
}
