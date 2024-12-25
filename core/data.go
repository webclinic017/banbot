package core

import (
	"context"
	"io"
	"sync"

	"github.com/banbox/banexg"
	"github.com/robfig/cron/v3"
)

var (
	RunMode       string                               // live / backtest / other
	RunEnv        string                               // prod / test / dry_run
	StartAt       int64                                // start timestamp(13 digits) 启动时间，13位时间戳
	EnvReal       bool                                 // Whether to actually submit the order to the exchange(run_env:prod/test) 是否是提交到交易所真实订单模式run_env:prod/test
	LiveMode      bool                                 // Whether real-time mode(real trade/dry run) 是否是实时模式：实盘+模拟运行
	TFSecs        map[string]int                       // All time frames involved 所有涉及的时间周期
	ExgName       string                               // current exchange name 交易所名称
	Market        string                               // current market name 当前市场
	IsContract    bool                                 // Is the current market a contract market? 当前市场是否是合约市场, linear/inverse/option
	CheckWallets  bool                                 // Should the wallet be updated? 当前是否应该更新钱包
	ContractType  string                               // current contract type. 当前合约类型
	StgPairTfs    = make(map[string]map[string]string) // strategy:symbols:timeframe 策略: 标的: 周期
	Pairs         []string                             // All global symbols, in the order after the targets are refreshed 全局所有的标的，按标的刷新后的顺序
	PairsMap      = make(map[string]bool)              // All global symbols 全局所有的标的
	BanPairsUntil = make(map[string]int64)             // symbols not allowed for trading before the specified timestamp 在指定时间戳前禁止交易的品种
	NoEnterUntil  = make(map[string]int64)             // account: The 13-digit timestamp before the account is allowed to trade 禁止开单的截止13位时间戳
	BookPairs     = make(map[string]bool)              // Monitor the currency of the trading pair 监听交易对的币种
	PairCopiedMs  = map[string][2]int64{}              // The latest time that all targets received K lines from the crawler, as well as the waiting interval, are used to determine whether there are any that have not been received for a long time. 所有标的从爬虫收到K线的最新时间，以及等待间隔，用于判断是否有长期未收到的。
	TfPairHits    = map[string]map[string]int{}        // tf[pair[hits]]The number of bars for each currency in each period within a period of time, used for timing output 一段时间内各周期各币种的bar数量，用于定时输出
	JobPerfs      = make(map[string]*JobPerf)          // stagy_pair_tf: JobPerf Record the billing amount ratio of the task. If the winning rate is low, the billing amount should be reduced. 记录任务的开单金额比率，胜率低的要减少开单金额
	StratPerfSta  = make(map[string]*PerfSta)          // stagy: Job任务状态
	LastBarMs     int64                                // The end time of the last bar received, a 13-digit timestamp 上次收到bar的结束时间，13位时间戳
	OdBooks       = map[string]*banexg.OrderBook{}     // Cache all order books received from crawler 缓存所有从爬虫收到的订单簿
	NumTaCache    = 1500                               // The number of historical values cached during indicator calculation, default 1500 指标计算时缓存的历史值数量，默认1500
	Cron          = cron.New(cron.WithSeconds())       // Use cron to run tasks regularly 使用cron定时运行任务

	ExitCalls []func()  // CALLBACK TO STOP EXECUTION 停止执行的回调
	MemOut    io.Writer // Output memory profile 进行内存profile的输出

	ConcurNum     = 2 // The maximum number of K-line tasks to be downloaded at the same time. If it is too high, a 429 current limit will occur. 最大同时下载K线任务数，过大会出现429限流
	Version       = "0.1.7"
	LogFile       string
	DevDbPath     string
	HeavyTask     string // 后台排他性耗时任务名称
	HeavyProgress int    // 后台排他性耗时任务进度
	heavyLock     sync.Mutex
	HeavyTriggers []PrgCB
)

type PrgCB func(done int, total int)

const (
	MSMinStamp = int64(1001894400000) // 2001-10-01T00:00:00.000Z
)

const (
	MinStakeAmount = 10 // Minimum billing amount 最小开单金额
	StepTotal      = 1000
	KBatchSize     = 900 // The maximum number of K lines returned by the exchange in a single request. When 1000, the API weight is too large. 单次请求交易所最大返回K线数量, 1000时api权重过大
	DefaultDateFmt = "2006-01-02 15:04:05"
	DelayBatchMS   = 3000  // Number of milliseconds to defer batch logic 批量逻辑推迟的毫秒数
	PrefMinRate    = 0.001 // Job minimum opening ratio, directly use MinStakeAmount to open a position job最低开仓比率，直接使用MinStakeAmount开仓
	AmtDust        = 1e-8
	DownKNumMin    = 100000 // 经测试，单个goroutine每分钟下载K线约100k个
)

const (
	RunModeLive     = "live"
	RunModeBackTest = "backtest"
	RunModeOther    = "other"
)

const (
	RunEnvProd   = "prod"
	RunEnvTest   = "test"
	RunEnvDryRun = "dry_run"
)

const (
	OdDirtShort = iota - 1
	OdDirtBoth
	OdDirtLong
)

const (
	EnterTagUnknown  = "unknown"
	EnterTagUserOpen = "user_open"
	EnterTagThird    = "third"
)

const (
	ExitTagUnknown     = "unknown"
	ExitTagCancel      = "cancel"
	ExitTagBotStop     = "bot_stop"
	ExitTagForceExit   = "force_exit"
	ExitTagUserExit    = "user_exit"
	ExitTagThird       = "third"
	ExitTagFatalErr    = "fatal_err"
	ExitTagPairDel     = "pair_del"
	ExitTagStopLoss    = "stop_loss"
	ExitTagSLTake      = "sl_take"
	ExitTagTakeProfit  = "take_profit"
	ExitTagDrawDown    = "draw_down"
	ExitTagDataStuck   = "data_stuck"
	ExitTagLiquidation = "liquidation"
	ExitTagEnvEnd      = "env_end"
	ExitTagEntExp      = "ent_exp" // enter limit expired
)

var (
	barPrices     = make(map[string]float64) // Latest price of each coin from bar, only for backtesting etc. The key can be a trading pair or a coin code 来自bar的每个币的最新价格，仅用于回测等。键可以是交易对，也可以是币的code
	prices        = make(map[string]float64) // The latest order book price of the trading pair is only used for real-time simulation or real trading. The key can be a trading pair or a coin code 交易对的最新订单簿价格，仅用于实时模拟或实盘。键可以是交易对，也可以是币的code
	lockPrices    sync.RWMutex
	lockBarPrices sync.RWMutex
	Ctx           context.Context // Used to stop all goroutines at the same time 用于全部goroutine同时停止
	StopAll       func()          // Stop all robot threads 停止全部机器人线程
	BotRunning    bool            // Is the robot running? 机器人是否正在运行
)

var (
	OrderTypeEnums = []string{"", banexg.OdTypeMarket, banexg.OdTypeLimit, banexg.OdTypeStopLoss,
		banexg.OdTypeStopLossLimit, banexg.OdTypeTakeProfit, banexg.OdTypeTakeProfitLimit,
		banexg.OdTypeStop, banexg.OdTypeLimitMaker}
)

const (
	OrderTypeEmpty = iota
	OrderTypeMarket
	OrderTypeLimit
	OrderTypeLimitMaker
)

const (
	AdjNone   = 1
	AdjFront  = 2
	AdjBehind = 3
)

const (
	VTypeUniform = iota // UNIFORM LINEAR DISTRIBUTION 均匀线性分布
	VTypeNorm           // Normal distribution, specifying mean and standard deviation 正态分布，指定均值和标准差
)
