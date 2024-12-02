package goods

import (
	"fmt"

	"github.com/banbox/banbot/config"
	"github.com/banbox/banbot/core"
	"github.com/banbox/banbot/exg"
	"github.com/banbox/banbot/orm"
	"github.com/banbox/banexg"
	"github.com/banbox/banexg/errs"
	"github.com/banbox/banexg/log"
	"github.com/mitchellh/mapstructure"
)

var (
	pairProducer IProducer
	filters      = make([]IFilter, 0, 10)
	needTickers  = false
	ShowLog      = true
)

func Setup() *errs.Error {
	if len(config.PairFilters) == 0 {
		return nil
	}
	fts, err := GetPairFilters(config.PairFilters, false)
	if err != nil {
		return err
	}
	for i, flt := range fts {
		if i == 0 {
			producer, ok := flt.(IProducer)
			if !ok {
				return errs.NewMsg(core.ErrBadConfig, "first pair filter must be IProducer")
			}
			pairProducer = producer
			continue
		}
		if flt.IsNeedTickers() {
			needTickers = true
			break
		}
	}
	filters = fts[1:]
	return nil
}

func GetPairFilters(items []*config.CommonPairFilter, withInvalid bool) ([]IFilter, *errs.Error) {
	fts := make([]IFilter, 0, len(items))
	// 未启用定期刷新，则允许成交量为空的品种
	allowEmpty := config.PairMgr.Cron == ""
	for _, cfg := range items {
		var output IFilter
		var base = BaseFilter{Name: cfg.Name, AllowEmpty: allowEmpty}
		switch cfg.Name {
		case "AgeFilter":
			output = &AgeFilter{BaseFilter: base}
		case "VolumePairList":
			output = &VolumePairFilter{BaseFilter: base}
		case "PriceFilter":
			base.NeedTickers = true
			output = &PriceFilter{BaseFilter: base}
		case "RateOfChangeFilter":
			output = &RateOfChangeFilter{BaseFilter: base}
		case "VolatilityFilter":
			output = &VolatilityFilter{BaseFilter: base}
		case "SpreadFilter":
			base.NeedTickers = true
			output = &SpreadFilter{BaseFilter: base}
		case "OffsetFilter":
			output = &OffsetFilter{BaseFilter: base}
		case "ShuffleFilter":
			output = &ShuffleFilter{BaseFilter: base}
		case "CorrelationFilter":
			output = &CorrelationFilter{BaseFilter: base}
		default:
			return nil, errs.NewMsg(errs.CodeParamInvalid, "unknown symbol filter: %s", cfg.Name)
		}
		err_ := mapstructure.Decode(cfg.Items, &output)
		if err_ != nil {
			return nil, errs.New(errs.CodeUnmarshalFail, err_)
		}
		if withInvalid || !output.IsDisable() {
			fts = append(fts, output)
		}
	}
	return fts, nil
}

func RefreshPairList() ([]string, *errs.Error) {
	var pairs []string
	var allowFilter = false
	var err *errs.Error
	var tickersMap map[string]*banexg.Ticker
	if len(config.Pairs) > 0 {
		pairs = config.Pairs
		pairVols, err := getSymbolVols(pairs, "1h", 1)
		if err != nil {
			return nil, err
		}
		pairs, _ = filterByMinCost(pairVols)
		allowFilter = config.PairMgr.ForceFilters
	} else {
		allowFilter = true
		if needTickers && core.LiveMode {
			tickersMap, err = exg.GetTickers()
			if err != nil {
				return nil, err
			}
		}
		pairs, err = pairProducer.GenSymbols(tickersMap)
		if err != nil {
			return nil, err
		}
		if ShowLog {
			log.Info(fmt.Sprintf("gen symbols from %s, num: %d", pairProducer.GetName(), len(pairs)))
		}
	}
	err = orm.EnsureCurSymbols(pairs)
	if err != nil {
		return nil, err
	}
	if allowFilter {
		for _, flt := range filters {
			if flt.IsDisable() {
				continue
			}
			oldNum := len(pairs)
			pairs, err = flt.Filter(pairs, tickersMap)
			if err != nil {
				return nil, err
			}
			if oldNum > len(pairs) && ShowLog {
				log.Info(fmt.Sprintf("left %d symbols after %s", len(pairs), flt.GetName()))
			}
		}
	}
	// 数量和偏移限制
	mgrCfg := config.PairMgr
	if mgrCfg.Offset > 0 {
		if mgrCfg.Offset < len(pairs) {
			pairs = pairs[mgrCfg.Offset:]
		} else {
			pairs = nil
		}
	}
	if mgrCfg.Limit > 0 && mgrCfg.Limit < len(pairs) {
		pairs = pairs[:mgrCfg.Limit]
	}

	core.Pairs = nil
	core.PairsMap = make(map[string]bool)
	for _, p := range pairs {
		core.Pairs = append(core.Pairs, p)
		core.PairsMap[p] = true
	}

	for pair := range core.BanPairsUntil {
		if _, ok := core.PairsMap[pair]; !ok {
			delete(core.BanPairsUntil, pair)
		}
	}
	return pairs, nil
}
