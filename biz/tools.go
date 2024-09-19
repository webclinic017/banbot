package biz

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"fmt"
	"github.com/banbox/banbot/btime"
	"github.com/banbox/banbot/config"
	"github.com/banbox/banbot/core"
	"github.com/banbox/banbot/data"
	"github.com/banbox/banbot/exg"
	"github.com/banbox/banbot/goods"
	"github.com/banbox/banbot/orm"
	"github.com/banbox/banbot/utils"
	"github.com/banbox/banexg"
	"github.com/banbox/banexg/errs"
	"github.com/banbox/banexg/log"
	"go.uber.org/zap"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

func LoadZipKline(inPath string, fid int, file *zip.File, arg interface{}) *errs.Error {
	cleanName := strings.Split(filepath.Base(file.Name), ".")[0]
	exArgs := arg.([]string)
	exgName, market := exArgs[0], exArgs[1]
	exchange, err := exg.GetWith(exgName, market, exArgs[2])
	if err != nil {
		return err
	}
	exInfo := exchange.Info()
	yearStr := strings.Split(filepath.Base(inPath), ".")[0]
	year, _ := strconv.Atoi(yearStr)
	mar, err := exchange.MapMarket(cleanName, year)
	if err != nil {
		log.Warn("symbol invalid", zap.String("id", cleanName), zap.String("err", err.Short()))
		return nil
	}
	exs := &orm.ExSymbol{Symbol: mar.Symbol, Exchange: exgName, ExgReal: mar.ExgReal, Market: market}
	err = orm.EnsureSymbols([]*orm.ExSymbol{exs})
	if err != nil {
		return err
	}
	fReader, err_ := file.Open()
	if err_ != nil {
		return errs.New(errs.CodeIOReadFail, err_)
	}
	rows, err_ := csv.NewReader(fReader).ReadAll()
	if err_ != nil {
		return errs.New(errs.CodeIOReadFail, err_)
	}
	if len(rows) <= 1 {
		return nil
	}
	klines := make([]*banexg.Kline, 0, len(rows))
	lastMS := int64(0)
	tfMSecs := int64(math.MaxInt64)
	for _, r := range rows {
		barTime, _ := strconv.ParseInt(r[0], 10, 64)
		o, _ := strconv.ParseFloat(r[1], 64)
		h, _ := strconv.ParseFloat(r[2], 64)
		l, _ := strconv.ParseFloat(r[3], 64)
		c, _ := strconv.ParseFloat(r[4], 64)
		v, _ := strconv.ParseFloat(r[5], 64)
		var d float64
		if len(r) > 6 {
			d, _ = strconv.ParseFloat(r[6], 64)
		}
		if barTime == 0 {
			continue
		}
		timeDiff := barTime - lastMS
		lastMS = barTime
		if timeDiff > 0 && timeDiff < tfMSecs {
			tfMSecs = timeDiff
		}
		klines = append(klines, &banexg.Kline{
			Time:   barTime,
			Open:   o,
			High:   h,
			Low:    l,
			Close:  c,
			Volume: v,
			Info:   d,
		})
	}
	sort.Slice(klines, func(i, j int) bool {
		return klines[i].Time < klines[j].Time
	})
	startMS, endMS := klines[0].Time, klines[len(klines)-1].Time
	timeFrame := utils.SecsToTF(int(tfMSecs / 1000))
	timeFrame, err = orm.GetDownTF(timeFrame)
	if err != nil {
		log.Warn("get down tf fail", zap.Int64("ms", tfMSecs), zap.String("id", exs.Symbol),
			zap.String("path", inPath), zap.String("err", err.Short()))
		return nil
	}
	tfMSecs = int64(utils.TFToSecs(timeFrame) * 1000)
	ctx := context.Background()
	sess, conn, err := orm.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// 过滤非交易时间段，成交量为0的
	// 由于历史数据中部分交易日未录入，故不适用交易日历过滤K线
	holes, err := sess.GetExSHoles(exchange, exs, startMS, endMS, true)
	if err != nil {
		return err
	}
	holeNum := len(holes)
	if holeNum > 0 {
		newKlines := make([]*banexg.Kline, 0, len(klines))
		hi := 0
		var half *banexg.Kline
		unExpNum := 0
		dayMSecs := int64(utils.TFToSecs("1d") * 1000)
		for i, k := range klines {
			for hi < holeNum && holes[hi][1] <= k.Time {
				if unExpNum > 0 {
					h := holes[hi]
					if h[1]-h[0] >= dayMSecs {
						// 非交易时间段超过1天
						expNum := int((h[1] - h[0]) / tfMSecs)
						if unExpNum*20 > expNum {
							// 非交易时间内，有效bar数量至少5%，则输出警告
							startStr := btime.ToDateStr(h[0], "")
							endStr := btime.ToDateStr(h[1], "")
							log.Warn("find klines in non-trade time", zap.Int32("sid", exs.ID),
								zap.Int("num", unExpNum), zap.String("start", startStr),
								zap.String("end", endStr))
						} else if unExpNum > 20 {
							half = nil
						}
					}
					unExpNum = 0
				}
				hi += 1
			}
			if hi >= holeNum {
				newKlines = append(newKlines, klines[i:]...)
				break
			}
			if half != nil {
				// 有前面额外的，合并到一起
				if k.High < half.High {
					k.High = half.High
				}
				if k.Low > half.Low {
					k.Low = half.Low
				}
				k.Open = half.Open
				k.Volume += half.Volume
				half = nil
			}
			h := holes[hi]
			if k.Time < h[0] {
				// 有效时间内
				newKlines = append(newKlines, k)
			} else if k.Volume > 0 {
				// 非交易时间段内，但有成交量，合并到最近有效bar
				unExpNum += 1
				if h[1]-k.Time < k.Time-h[0] {
					//离后面更近，合并到下一个
					if h[1]-k.Time < tfMSecs*10 {
						half = k
					}
				} else if len(newKlines) > 0 {
					// 离前面更近，合并到上一个（最多10个）
					p := newKlines[len(newKlines)-1]
					if k.Time-p.Time < tfMSecs*10 {
						if p.High < k.High {
							p.High = k.High
						}
						if p.Low > k.Low {
							p.Low = k.Low
						}
						p.Close = k.Close
						p.Volume += k.Volume
						p.Info = k.Info
					}
				}
			}
		}
		if len(newKlines) == 0 {
			return nil
		}
		klines = newKlines
	}
	oldStart, oldEnd := sess.GetKlineRange(exs.ID, timeFrame)
	if oldStart <= startMS && endMS <= oldEnd {
		// 都已存在，无需写入
		return nil
	}
	if oldStart > 0 {
		newKlines := make([]*banexg.Kline, 0, len(klines))
		for _, k := range klines {
			if k.Time < oldStart || k.Time >= oldEnd {
				newKlines = append(newKlines, k)
			}
		}
		if len(newKlines) == 0 {
			return nil
		}
		klines = newKlines
	}
	startMS, endMS = klines[0].Time, klines[len(klines)-1].Time
	startDt := btime.ToDateStr(startMS, "")
	endDt := btime.ToDateStr(endMS, "")
	log.Info("insert", zap.String("symbol", exs.Symbol), zap.Int32("sid", exs.ID),
		zap.Int("num", len(klines)), zap.String("start", startDt), zap.String("end", endDt))
	// 这里不自动归集，因有些bar成交量为0，不可使用数据库默认的归集策略；应调用BuildOHLCVOff归集
	num, err := sess.InsertKLinesAuto(timeFrame, exs.ID, klines, false)
	if err == nil && num > 0 {
		// 插入更大周期
		aggList := orm.GetKlineAggs()
		klines1m := klines
		for _, agg := range aggList[1:] {
			if agg.MSecs <= tfMSecs {
				continue
			}
			offMS := int64(exg.GetAlignOff(exInfo.ID, int(agg.MSecs/1000)) * 1000)
			klines, _ = utils.BuildOHLCV(klines1m, agg.MSecs, 0, nil, tfMSecs, offMS)
			if len(klines) == 0 {
				continue
			}
			num, err = sess.InsertKLinesAuto(agg.TimeFrame, exs.ID, klines, false)
			if err != nil {
				log.Error("insert kline fail", zap.String("id", mar.ID),
					zap.String("tf", agg.TimeFrame), zap.Error(err))
			}
			if num == 0 {
				break
			}
		}
	}
	return err
}

func LoadCalendars(args *config.CmdArgs) *errs.Error {
	core.SetRunMode(core.RunModeOther)
	err := SetupComs(args)
	if err != nil {
		return err
	}
	if args.InPath == "" {
		return errs.NewMsg(errs.CodeParamRequired, "--in is required")
	}
	rows, err := utils.ReadCSV(args.InPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	sess, conn, err := orm.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	lastExg := ""
	dateList := make([][2]int64, 0)
	dtLay := "2006-01-02"
	for _, row := range rows {
		startMS := btime.ParseTimeMSBy(dtLay, row[1])
		stopMS := btime.ParseTimeMSBy(dtLay, row[2])
		if lastExg == "" {
			lastExg = row[0]
		}
		if lastExg != row[0] {
			if len(dateList) > 0 {
				err = sess.SetCalendars(lastExg, dateList)
				if err != nil {
					log.Error("save calendars fail", zap.String("exg", lastExg), zap.Error(err))
				}
				dateList = make([][2]int64, 0)
			}
			lastExg = row[0]
		}
		dateList = append(dateList, [2]int64{startMS, stopMS})
	}
	if len(dateList) > 0 {
		err = sess.SetCalendars(lastExg, dateList)
		if err != nil {
			log.Error("save calendars fail", zap.String("exg", lastExg), zap.Error(err))
		}
	}
	log.Info("load calendars success", zap.Int("num", len(rows)))
	return nil
}

var adjMap = map[string]int{
	"pre":  core.AdjFront,
	"post": core.AdjBehind,
	"none": core.AdjNone,
	"":     0,
}

func ExportKlines(args *config.CmdArgs) *errs.Error {
	core.SetRunMode(core.RunModeOther)
	err := SetupComs(args)
	if err != nil {
		return err
	}
	if args.OutPath == "" {
		return errs.NewMsg(errs.CodeParamRequired, "--out is required")
	}
	if len(args.Pairs) == 0 {
		// 未提供标的，导出当前市场所有
		err = orm.LoadAllExSymbols()
		if err != nil {
			return err
		}
		exsList := orm.GetAllExSymbols()
		for _, exs := range exsList {
			if exs.Exchange != core.ExgName || exs.Market != core.Market {
				continue
			}
			args.Pairs = append(args.Pairs, exs.Symbol)
		}
		if len(args.Pairs) == 0 {
			return errs.NewMsg(errs.CodeParamRequired, "--pairs is required")
		}
		sort.Strings(args.Pairs)
	}
	if len(args.TimeFrames) == 0 {
		return errs.NewMsg(errs.CodeParamRequired, "--timeframes is required")
	}
	adjVal, adjValid := adjMap[args.AdjType]
	if !adjValid {
		return errs.NewMsg(errs.CodeParamRequired, "--adj should be pre/post/none")
	}
	ctx := context.Background()
	sess, conn, err := orm.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	err_ := utils.EnsureDir(args.OutPath, 0755)
	if err_ != nil {
		return errs.New(errs.CodeIOWriteFail, err_)
	}
	start, stop := config.TimeRange.StartMS, config.TimeRange.EndMS
	loc, err := args.ParseTimeZone()
	if err != nil {
		return err
	}
	err = orm.InitExg(exg.Default)
	if err != nil {
		return err
	}
	startStr := btime.ToTime(start).In(loc).Format(core.DefaultDateFmt)
	endStr := btime.ToTime(stop).In(loc).Format(core.DefaultDateFmt)
	log.Info("export kline", zap.Strings("tf", args.TimeFrames), zap.String("dt", startStr+" - "+endStr),
		zap.String("adj", args.AdjType), zap.Int("num", len(args.Pairs)))
	names, err := data.FindPathNames(args.OutPath, ".zip")
	if err != nil {
		return err
	}
	handles := make(map[string]bool)
	for _, n := range names {
		parts := strings.Split(strings.ReplaceAll(n, ".zip", ""), "_")
		handles[strings.Join(parts[:len(parts)-1], "_")] = true
	}
	for _, symbol := range args.Pairs {
		clean := strings.ReplaceAll(strings.ReplaceAll(symbol, "/", "_"), ":", "_")
		if _, ok := handles[clean]; ok {
			log.Info("skip exist", zap.String("symbol", symbol))
			continue
		}
		log.Info("handle", zap.String("symbol", symbol))
		exs, err := orm.GetExSymbolCur(symbol)
		if err != nil {
			log.Warn("export fail", zap.String("symbol", symbol), zap.Error(err))
			continue
		}
		for _, tf := range args.TimeFrames {
			adjs, klines, err := sess.GetOHLCV(exs, tf, start, stop, 0, false)
			if err != nil {
				return err
			}
			klines = orm.ApplyAdj(adjs, klines, adjVal, 0, 0)
			rows := utils.KlineToStr(klines, loc)
			path := filepath.Join(args.OutPath, fmt.Sprintf("%s_%s.csv", clean, tf))
			err = utils.WriteCsvFile(path, rows, true)
			if err != nil {
				return err
			}
		}
	}
	log.Info("export kline complete")
	return nil
}

func PurgeKlines(args *config.CmdArgs) *errs.Error {
	core.SetRunMode(core.RunModeOther)
	err := SetupComs(args)
	if err != nil {
		return err
	}
	sess, conn, err := orm.Conn(nil)
	if err != nil {
		return err
	}
	defer conn.Release()
	ctx := context.Background()
	infos, err_ := sess.ListKInfos(ctx)
	if err_ != nil {
		return orm.NewDbErr(core.ErrDbReadFail, err_)
	}
	infoMap := make(map[int32]*orm.KInfo)
	for _, i := range infos {
		infoMap[i.Sid] = i
	}
	exchange := exg.Default
	err = orm.InitExg(exchange)
	if err != nil {
		return err
	}
	// 搜索需要删除的标的
	exsList := make([]*orm.ExSymbol, 0)
	if len(config.Pairs) > 0 {
		for _, symbol := range config.Pairs {
			exs, err := orm.GetExSymbol(exchange, symbol)
			if err != nil {
				return err
			}
			exsList = append(exsList, exs)
		}
	} else {
		exInfo := exchange.Info()
		exMap := orm.GetExSymbols(exInfo.ID, exInfo.MarketType)
		for _, exs := range exMap {
			exsList = append(exsList, exs)
		}
	}
	if args.ExgReal != "" {
		filtered := make([]*orm.ExSymbol, 0, len(exsList))
		for _, exs := range exsList {
			if exs.ExgReal == args.ExgReal {
				filtered = append(filtered, exs)
			}
		}
		exsList = filtered
	}
	if len(exsList) == 0 {
		return errs.NewMsg(errs.CodeRunTime, "pairs is required")
	}
	// 输出信息要求确认
	pairs := make([]string, 0, len(exsList))
	for _, exs := range exsList {
		pairs = append(pairs, exs.Symbol)
	}
	tfList := args.TimeFrames
	isAllTf := false
	if len(tfList) == 0 {
		isAllTf = true
		aggs := orm.GetKlineAggs()
		for _, a := range aggs {
			tfList = append(tfList, a.TimeFrame)
		}
	}
	isOk := utils.ReadConfirm([]string{
		fmt.Sprintf("exchange: %s, exg_real: %s", config.Exchange.Name, args.ExgReal),
		fmt.Sprintf("date range: all"),
		fmt.Sprintf("timeFrames: %s", strings.Join(tfList, ", ")),
		fmt.Sprintf("symbols(%v): %s", len(exsList), strings.Join(pairs, ", ")),
		"input `y` to delete, `n` to cancel:",
	}, "y", "n", true)
	if !isOk {
		return nil
	}
	// 删除符合要求的数据
	pBar := utils.NewPrgBar(len(exsList), "purge")
	defer pBar.Close()
	for _, exs := range exsList {
		pBar.Add(1)
		for _, tf := range tfList {
			err = sess.DelKLines(exs.ID, tf)
			if err != nil {
				return err
			}
			err = sess.DelKInfo(exs.ID, tf)
			if err != nil {
				return err
			}
			err = sess.DelKHoles(exs.ID, tf)
			if err != nil {
				return err
			}
			err = sess.DelKLineUn(exs.ID, tf)
			if err != nil {
				return err
			}
		}
		if isAllTf {
			err = sess.DelFactors(exs.ID)
			if err != nil {
				return err
			}
		}
	}
	log.Info("all purge complete")
	return nil
}

func ExportAdjFactors(args *config.CmdArgs) *errs.Error {
	core.SetRunMode(core.RunModeOther)
	err := SetupComs(args)
	if err != nil {
		return err
	}
	if args.OutPath == "" {
		return errs.NewMsg(errs.CodeParamRequired, "--out is required")
	}
	if len(args.Pairs) == 0 {
		return errs.NewMsg(errs.CodeParamRequired, "--pairs is required")
	}
	ctx := context.Background()
	sess, conn, err := orm.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	err_ := utils.EnsureDir(args.OutPath, 0755)
	if err_ != nil {
		return errs.New(errs.CodeIOWriteFail, err_)
	}
	loc, err := args.ParseTimeZone()
	if err != nil {
		return err
	}
	err = orm.InitExg(exg.Default)
	if err != nil {
		return err
	}
	for _, symbol := range args.Pairs {
		log.Info("handle", zap.String("symbol", symbol))
		exs, err := orm.GetExSymbolCur(symbol)
		if err != nil {
			return err
		}
		facs, err_ := sess.GetAdjFactors(ctx, exs.ID)
		if err_ != nil {
			return orm.NewDbErr(core.ErrDbReadFail, err_)
		}
		sort.Slice(facs, func(i, j int) bool {
			return facs[i].StartMs < facs[j].StartMs
		})
		rows := make([][]string, 0, len(facs))
		for _, f := range facs {
			dateStr := btime.ToTime(f.StartMs).In(loc).Format(core.DefaultDateFmt)
			subCode := ""
			if f.SubID > 0 {
				it := orm.GetSymbolByID(f.SubID)
				if it != nil {
					subCode = it.Symbol
				}
			}
			row := []string{
				subCode,
				dateStr,
				strconv.FormatFloat(f.Factor, 'f', -1, 64),
			}
			rows = append(rows, row)
		}
		path := filepath.Join(args.OutPath, symbol+"_adj.csv")
		err = utils.WriteCsvFile(path, rows, false)
		if err != nil {
			return err
		}
	}
	return nil
}

/*
CalcCorrelation calculate correlation for pairs; generate csv or images
*/
func CalcCorrelation(args *config.CmdArgs) *errs.Error {
	core.SetRunMode(core.RunModeOther)
	err := SetupComs(args)
	if err != nil {
		return err
	}
	err = orm.InitExg(exg.Default)
	if err != nil {
		return err
	}
	if len(args.TimeFrames) == 0 {
		return errs.NewMsg(errs.CodeParamRequired, "--timeframes is required")
	}
	if args.BatchSize <= 1 {
		return errs.NewMsg(errs.CodeParamRequired, "--batch-size is required")
	}
	if args.RunEveryTF == "" {
		return errs.NewMsg(errs.CodeParamRequired, "--run-every is required")
	}
	if args.OutPath == "" {
		return errs.NewMsg(errs.CodeParamRequired, "--out is required")
	}
	pairs, err := goods.RefreshPairList()
	if err != nil {
		return err
	}
	slices.Sort(pairs)
	exsList := make([]*orm.ExSymbol, 0, len(pairs))
	for _, pair := range pairs {
		exs, err := orm.GetExSymbolCur(pair)
		if err != nil {
			log.Warn("get exs fail, skip", zap.String("code", pair), zap.Error(err))
			continue
		}
		exsList = append(exsList, exs)
	}
	tf := args.TimeFrames[0]
	tfMSecs := int64(utils.TFToSecs(tf) * 1000)
	gapTFMSecs := int64(utils.TFToSecs(args.RunEveryTF) * 1000)
	if int(gapTFMSecs/tfMSecs) < args.BatchSize {
		log.Error("run-every is too small for current batch-size and timeframe")
		return nil
	}
	startMs := config.TimeRange.StartMS
	klineNum := args.BatchSize + 1
	pBar := utils.NewPrgBar(int((config.TimeRange.EndMS-startMs)/gapTFMSecs)+1, "Corr")
	defer pBar.Close()
	var csvRows [][]string
	codes := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		id, _, _, _ := core.SplitSymbol(pair)
		codes = append(codes, id)
	}
	emptyRow := make(map[string]string)
	// make csv head; first avg corr for each code
	var head []string
	head = append(head, "date")
	emptyRow["date"] = "-"
	for _, id := range codes {
		emptyRow[id] = "-"
		head = append(head, id)
	}
	// detail corr for each pair
	for i, id := range codes {
		for j := i + 1; j < len(codes); j++ {
			key := fmt.Sprintf("%s/%s", id, codes[j])
			emptyRow[key] = "-"
			head = append(head, key)
		}
	}
	csvRows = append(csvRows, head)
	for {
		if startMs >= config.TimeRange.EndMS {
			break
		}
		pBar.Add(1)
		// Calculate logarithmic rate of return
		names := make([]string, 0, len(exsList))
		dataArr := make([][]float64, 0, len(exsList))
		var lacks []string
		for i, exs := range exsList {
			_, klines, err := orm.GetOHLCV(exs, tf, startMs, startMs+gapTFMSecs, klineNum, false)
			if err != nil {
				log.Warn("get kline fail, skip", zap.String("code", exs.Symbol), zap.Error(err))
				continue
			}
			if len(klines) >= klineNum {
				prices := make([]float64, 0, len(klines))
				for _, b := range klines {
					prices = append(prices, b.Close)
				}
				names = append(names, codes[i])
				dataArr = append(dataArr, prices[:klineNum])
			} else {
				lacks = append(lacks, exs.Symbol)
			}
		}
		dateStr := btime.ToDateStr(startMs, "20060102")
		if len(lacks) > 0 {
			log.Warn("skip no enough kline", zap.String("dt", dateStr), zap.Strings("codes", lacks))
		}
		startMs += gapTFMSecs
		if len(names) == 0 {
			continue
		}
		// Calculate the Pearson correlation matrix
		corrMat, avgs, err_ := utils.CalcCorrMat(dataArr, true)
		if err_ != nil {
			return errs.New(errs.CodeRunTime, err_)
		}
		if args.OutType == "csv" {
			row := make(map[string]string)
			for k, v := range emptyRow {
				row[k] = v
			}
			row["date"] = btime.ToDateStr(startMs, "2006-01-02 15:04")
			for i, id := range names {
				for j := i + 1; j < len(names); j++ {
					val := corrMat.At(i, j)
					key := fmt.Sprintf("%s/%s", id, names[j])
					row[key] = strconv.FormatFloat(val, 'f', 3, 64)
				}
				row[id] = strconv.FormatFloat(avgs[i], 'f', 3, 64)
			}
			arr := make([]string, 0, len(head))
			for _, name := range head {
				arr = append(arr, row[name])
			}
			csvRows = append(csvRows, arr)
		} else {
			imgData, err_ := utils.GenCorrImg(corrMat, dateStr, names, "", 0)
			if err_ != nil {
				return errs.New(errs.CodeRunTime, err_)
			}
			err = utils.WriteFile(filepath.Join(args.OutPath, dateStr+".png"), imgData)
			if err != nil {
				return err
			}
		}
	}
	if args.OutType == "csv" {
		outName := fmt.Sprintf("corr_%s_every_%s.csv", tf, args.RunEveryTF)
		return utils.WriteCsvFile(filepath.Join(args.OutPath, outName), csvRows, false)
	}
	return nil
}
