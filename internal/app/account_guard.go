package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-usage-keeper/internal/poller"
	"cpa-usage-keeper/internal/quota"
	"cpa-usage-keeper/internal/repository"
	"cpa-usage-keeper/internal/service"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// accountGuardDisableCooldown 是同一账号两次自动禁用之间的最小间隔，避免坏号每次请求都触发禁用调用。
const accountGuardDisableCooldown = 5 * time.Minute

// accountGuardSubscribeRetryInterval 是 error 订阅断线后的固定重连间隔。
const accountGuardSubscribeRetryInterval = 10 * time.Second

// accountGuardDisabledPollInterval 是守护处于关闭态时，订阅环重新检查启用开关的轮询间隔。
const accountGuardDisabledPollInterval = 10 * time.Second

// accountGuardProbePollInterval 是等待被禁号探测任务 settle 时的轮询间隔。
const accountGuardProbePollInterval = 2 * time.Second

// accountGuardProbeTimeout 是单轮被禁号探测等待 settle 的上限，超时后用已有结果出报告。
const accountGuardProbeTimeout = 2 * time.Minute

// accountProber 抽象对指定账号的限额探测入口，便于测试替换 quota.Service。
type accountProber interface {
	Refresh(ctx context.Context, request quota.RefreshRequest) (quota.RefreshResponse, error)
	GetRefreshTaskByAuthIndex(ctx context.Context, authIndex string) (quota.RefreshTaskResponse, error)
}

// accountDisabler 抽象禁用入口，便于测试替换 CPA 客户端。
type accountDisabler interface {
	SetAuthFilesDisabled(ctx context.Context, names []string, disabled bool) (service.AuthFilesManagementResponse, error)
}

// disabledAccount 是一个被禁用账号的探测视角快照。
type disabledAccount struct {
	AuthIndex string
	FileName  string
	Name      string
}

// healthSnapshot 是号池健康度计数，纯由本地 metadata 统计得到，无需探测。
type healthSnapshot struct {
	Total    int
	Disabled int
}

// Normal 返回正常(未禁用)账号数。
func (h healthSnapshot) Normal() int {
	if h.Total < h.Disabled {
		return 0
	}
	return h.Total - h.Disabled
}

// disabledLister 列出当前被禁用的 Auth File 账号，供恢复探测使用。
type disabledLister func(ctx context.Context) ([]disabledAccount, error)

// healthCounter 统计号池健康度计数(总数/被禁数)。
type healthCounter func(ctx context.Context) (healthSnapshot, error)

// fileNameResolver 把 error 事件里的 auth_index 翻译成 CPA auth-file 的 FileName。
// ok=false 表示无法定位可禁用的 FileName（未同步、或 OAuth 类无文件名），调用方据此只告警不禁用。
type fileNameResolver func(ctx context.Context, authIndex string) (fileName string, ok bool, err error)

// guardSettingsProvider 提供运行时设置快照，runner 每次处理事件/每轮告警都读它实现热生效。
type guardSettingsProvider interface {
	Snapshot() service.AccountGuardRuntime
}

// AccountGuardRunner 订阅 CPA error 事件实现 401/402 秒级禁用，并周期巡检 + 汇总告警。
type AccountGuardRunner struct {
	// subSource 建立 CPA errors channel 订阅连接。
	subSource poller.UsageSubscriptionSource
	// prober 对指定账号触发限额探测，用于检测被禁号是否恢复。
	prober accountProber
	// disabler 执行账号禁用动作。
	disabler accountDisabler
	// listDisabled 列出当前被禁用账号(恢复探测对象)。
	listDisabled disabledLister
	// healthCounts 统计号池健康度计数。
	healthCounts healthCounter
	// resolve 把 auth_index 翻译成可禁用的 FileName。
	resolve fileNameResolver
	// settings 提供运行时设置快照（启用开关、状态码集合、告警间隔、Notifier）。
	settings guardSettingsProvider
	// now 允许测试控制时间。
	now func() time.Time
	// sleep 允许测试替换可被 context 打断的等待。
	sleep func(context.Context, time.Duration) bool
	// onSubscribeReady 仅供测试确认订阅环已就绪。
	onSubscribeReady func()

	// mu 保护去重表和周期计数器。
	mu sync.Mutex
	// lastDisabledAt 记录每个 FileName 最近一次禁用尝试时间，用于冷却去重。
	lastDisabledAt map[string]time.Time
	// periodDisabled 是本告警周期内被自动禁用的 FileName 集合（用于报告明细）。
	periodDisabled map[string]struct{}
	// periodDisableFailed 是本周期禁用调用失败次数。
	periodDisableFailed int
	// periodUnresolved 是本周期收到但无法定位 FileName 的禁用码事件数。
	periodUnresolved int
	// periodMonitored 是本周期命中监测码（只告警不禁用）的事件数。
	periodMonitored int
}

// AccountGuardDeps 是构造 runner 的依赖集合。
type AccountGuardDeps struct {
	SubSource    poller.UsageSubscriptionSource
	Prober       accountProber
	Disabler     accountDisabler
	ListDisabled disabledLister
	HealthCounts healthCounter
	Resolve      fileNameResolver
	Settings     guardSettingsProvider
}

// NewAccountGuardRunner 构造 runner；只保存依赖，不主动联网。
func NewAccountGuardRunner(deps AccountGuardDeps) *AccountGuardRunner {
	return &AccountGuardRunner{
		subSource:      deps.SubSource,
		prober:         deps.Prober,
		disabler:       deps.Disabler,
		listDisabled:   deps.ListDisabled,
		healthCounts:   deps.HealthCounts,
		resolve:        deps.Resolve,
		settings:       deps.Settings,
		now:            time.Now,
		sleep:          sleepContext,
		lastDisabledAt: make(map[string]time.Time),
		periodDisabled: make(map[string]struct{}),
	}
}

// NewDBDisabledLister 返回基于本地 usage_identities 表的被禁用账号列举器。
func NewDBDisabledLister(db *gorm.DB) disabledLister {
	return func(ctx context.Context) ([]disabledAccount, error) {
		identities, err := repository.ListDisabledAuthFileIdentities(ctx, db)
		if err != nil {
			return nil, err
		}
		accounts := make([]disabledAccount, 0, len(identities))
		for _, identity := range identities {
			fileName := ""
			if identity.FileName != nil {
				fileName = strings.TrimSpace(*identity.FileName)
			}
			accounts = append(accounts, disabledAccount{
				AuthIndex: strings.TrimSpace(identity.Identity),
				FileName:  fileName,
				Name:      identity.Name,
			})
		}
		return accounts, nil
	}
}

// NewDBHealthCounter 返回基于本地 usage_identities 表的健康度计数器。
func NewDBHealthCounter(db *gorm.DB) healthCounter {
	return func(ctx context.Context) (healthSnapshot, error) {
		counts, err := repository.CountAuthFileIdentityHealth(ctx, db)
		if err != nil {
			return healthSnapshot{}, err
		}
		return healthSnapshot{Total: int(counts.Total), Disabled: int(counts.Disabled)}, nil
	}
}

// NewDBFileNameResolver 返回基于本地 usage_identities 表的 auth_index→FileName 翻译器。
func NewDBFileNameResolver(db *gorm.DB) fileNameResolver {
	return func(ctx context.Context, authIndex string) (string, bool, error) {
		identity, err := repository.GetActiveAuthFileUsageIdentityByAuthIndex(ctx, db, strings.TrimSpace(authIndex))
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// 账号尚未同步进本地表，交给巡检兜底，不视为错误。
				return "", false, nil
			}
			return "", false, err
		}
		if identity.FileName == nil {
			// OAuth 类账号无文件名，无法走 auth-files 禁用接口。
			return "", false, nil
		}
		fileName := strings.TrimSpace(*identity.FileName)
		if fileName == "" {
			return "", false, nil
		}
		return fileName, true, nil
	}
}

func (r *AccountGuardRunner) validate() error {
	if r == nil {
		return fmt.Errorf("account guard runner is nil")
	}
	if r.subSource == nil {
		return fmt.Errorf("account guard subscribe source is nil")
	}
	if r.prober == nil {
		return fmt.Errorf("account guard prober is nil")
	}
	if r.disabler == nil {
		return fmt.Errorf("account guard disabler is nil")
	}
	if r.listDisabled == nil {
		return fmt.Errorf("account guard disabled lister is nil")
	}
	if r.healthCounts == nil {
		return fmt.Errorf("account guard health counter is nil")
	}
	if r.resolve == nil {
		return fmt.Errorf("account guard resolver is nil")
	}
	if r.settings == nil {
		return fmt.Errorf("account guard settings provider is nil")
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.sleep == nil {
		r.sleep = sleepContext
	}
	if r.lastDisabledAt == nil {
		r.lastDisabledAt = make(map[string]time.Time)
	}
	if r.periodDisabled == nil {
		r.periodDisabled = make(map[string]struct{})
	}
	return nil
}

// Run 启动号池守护：实时 error 订阅环与周期巡检/告警环并行运行，直到 ctx 取消。
func (r *AccountGuardRunner) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	logrus.Info("account guard task started")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.runSubscribeLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		r.runAlertLoop(ctx)
	}()
	wg.Wait()
	return nil
}

// runSubscribeLoop 维持 errors channel 订阅，逐条处理 401/402 事件，断线后固定间隔重连。
func (r *AccountGuardRunner) runSubscribeLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		// 守护关闭态下不建立订阅连接，固定间隔轮询启用开关，开启后立即恢复订阅。
		if !r.settings.Snapshot().Enabled {
			if !r.sleep(ctx, accountGuardDisabledPollInterval) {
				return
			}
			continue
		}
		sub, err := r.subSource.Subscribe(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logrus.WithError(err).Warn("account guard error subscribe failed, will retry")
			if !r.sleep(ctx, accountGuardSubscribeRetryInterval) {
				return
			}
			continue
		}
		if r.onSubscribeReady != nil {
			r.onSubscribeReady()
		}
		r.receiveErrorEvents(ctx, sub)
		_ = sub.Close()
		if ctx.Err() != nil {
			return
		}
		// 订阅断开后固定间隔重连；断连期间漏掉的事件由周期巡检兜底。
		if !r.sleep(ctx, accountGuardSubscribeRetryInterval) {
			return
		}
	}
}

// receiveErrorEvents 在单条订阅连接上阻塞接收，直到连接断开或 ctx 取消。
func (r *AccountGuardRunner) receiveErrorEvents(ctx context.Context, sub poller.UsageSubscription) {
	for {
		payload, err := sub.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logrus.WithError(err).Warn("account guard error subscription disconnected")
			}
			return
		}
		event, ok := parseCPAErrorEvent(payload)
		if !ok {
			continue
		}
		r.handleErrorEvent(ctx, event)
	}
}

// handleErrorEvent 处理单条 error 事件：命中监测码只计数告警；命中禁用码则翻译 FileName 并秒级禁用。
func (r *AccountGuardRunner) handleErrorEvent(ctx context.Context, event cpaErrorEvent) {
	rt := r.settings.Snapshot()
	if !rt.Enabled {
		return
	}
	statusCode := event.StatusCode
	if rt.MonitorCodes[statusCode] {
		// 监测码（如 429 限流）只统计进告警，不禁号。
		r.recordMonitored()
	}
	if !rt.DisableCodes[statusCode] {
		return
	}
	authIndex := strings.TrimSpace(event.AuthIndex)
	if authIndex == "" {
		return
	}
	fileName, ok, err := r.resolve(ctx, authIndex)
	if err != nil {
		logrus.WithError(err).WithField("auth_index", authIndex).Warn("account guard resolve file name failed")
		r.recordUnresolved()
		return
	}
	if !ok {
		// 无法定位 FileName（未同步或 OAuth），只计数告警，交给巡检兜底。
		r.recordUnresolved()
		return
	}
	r.disableFileName(ctx, fileName, statusCode)
}

// disableFileName 在冷却窗口外对单个 FileName 执行禁用，并累计周期计数。
func (r *AccountGuardRunner) disableFileName(ctx context.Context, fileName string, statusCode int) {
	if !r.shouldDisable(fileName) {
		return
	}
	if _, err := r.disabler.SetAuthFilesDisabled(ctx, []string{fileName}, true); err != nil {
		logrus.WithError(err).WithField("file_name", fileName).Warn("account guard disable failed")
		r.recordDisableFailed()
		return
	}
	logrus.WithFields(logrus.Fields{"file_name": fileName, "status_code": statusCode}).Info("account guard disabled account")
	r.recordDisabled(fileName)
}

// shouldDisable 在冷却窗口内对同一 FileName 去重，避免坏号刷爆禁用调用；返回 true 时已登记本次尝试时间。
func (r *AccountGuardRunner) shouldDisable(fileName string) bool {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastDisabledAt[fileName]; ok && now.Sub(last) < accountGuardDisableCooldown {
		return false
	}
	r.lastDisabledAt[fileName] = now
	return true
}

func (r *AccountGuardRunner) recordDisabled(fileName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.periodDisabled[fileName] = struct{}{}
}

func (r *AccountGuardRunner) recordDisableFailed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.periodDisableFailed++
}

func (r *AccountGuardRunner) recordUnresolved() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.periodUnresolved++
}

func (r *AccountGuardRunner) recordMonitored() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.periodMonitored++
}

// runAlertLoop 每隔告警间隔触发一轮被禁号恢复探测并发送健康度汇总告警；间隔与启用态实时取自设置快照。
func (r *AccountGuardRunner) runAlertLoop(ctx context.Context) {
	timer := time.NewTimer(r.alertInterval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.runAlertCycle(ctx)
			timer.Reset(r.alertInterval())
		}
	}
}

// alertInterval 从设置快照取告警间隔，非正值时回退到 15 分钟。
func (r *AccountGuardRunner) alertInterval() time.Duration {
	interval := r.settings.Snapshot().AlertInterval
	if interval <= 0 {
		return 15 * time.Minute
	}
	return interval
}

// runAlertCycle 执行一轮：探测被禁号是否恢复 → 统计健康度 → 组装并发送报告；守护关闭时跳过本轮。
func (r *AccountGuardRunner) runAlertCycle(ctx context.Context) {
	rt := r.settings.Snapshot()
	if !rt.Enabled {
		// 守护关闭：不探测、不告警，仅清空可能残留的周期计数。
		r.resetPeriod()
		return
	}
	// 列出当前被禁号(恢复探测对象);失败时降级为空列表,仍出健康度报告。
	disabled, err := r.listDisabled(ctx)
	if err != nil {
		logrus.WithError(err).Warn("account guard list disabled accounts failed")
		disabled = nil
	}
	// 探测被禁号,挑出已恢复的(只告警建议手动启用,不自动启用)。
	recovered := r.probeRecovered(ctx, disabled)

	// 健康度纯由本地 metadata 统计,无需探测全池。
	health, err := r.healthCounts(ctx)
	if err != nil {
		logrus.WithError(err).Warn("account guard health counts failed")
	}

	report := r.composeReport(health, len(disabled), recovered)
	rt.Notifier.Notify(ctx, report)
	r.resetPeriod()
}

// probeRecovered 对被禁号触发限额探测,返回探测结果为正常(已恢复)的账号。
func (r *AccountGuardRunner) probeRecovered(ctx context.Context, disabled []disabledAccount) []disabledAccount {
	if len(disabled) == 0 {
		return nil
	}
	authIndexes := make([]string, 0, len(disabled))
	for _, account := range disabled {
		if account.AuthIndex != "" {
			authIndexes = append(authIndexes, account.AuthIndex)
		}
	}
	if len(authIndexes) == 0 {
		return nil
	}
	// source=manual 会对已失败任务重建并重新探测,不吃自动刷新的 401 缓存。
	if _, err := r.prober.Refresh(ctx, quota.RefreshRequest{AuthIndexes: authIndexes, Source: quota.RefreshSourceManual}); err != nil {
		logrus.WithError(err).Warn("account guard probe refresh failed")
		return nil
	}
	deadline := r.now().Add(accountGuardProbeTimeout)
	recovered := make([]disabledAccount, 0)
	for _, account := range disabled {
		if account.AuthIndex == "" {
			continue
		}
		if r.waitProbeRecovered(ctx, account.AuthIndex, deadline) {
			recovered = append(recovered, account)
		}
	}
	return recovered
}

// waitProbeRecovered 轮询单个账号的探测任务直到 settle,completed 视为已恢复。
func (r *AccountGuardRunner) waitProbeRecovered(ctx context.Context, authIndex string, deadline time.Time) bool {
	for {
		resp, err := r.prober.GetRefreshTaskByAuthIndex(ctx, authIndex)
		if err == nil {
			switch resp.Status {
			case quota.RefreshTaskStatusCompleted:
				// 探测成功(凭证有效)即视为已恢复。
				return true
			case quota.RefreshTaskStatusFailed:
				// 仍 401/402 或其它失败,保持禁用。
				return false
			}
		}
		// queued/running 或读取出错时继续等待,直到 settle 或超时。
		if !r.now().Before(deadline) {
			return false
		}
		if !r.sleep(ctx, accountGuardProbePollInterval) {
			return false
		}
	}
}

// composeReport 基于健康度计数和周期统计组装中文汇总文本。
func (r *AccountGuardRunner) composeReport(health healthSnapshot, disabledCount int, recovered []disabledAccount) string {
	r.mu.Lock()
	newlyDisabled := make([]string, 0, len(r.periodDisabled))
	for fileName := range r.periodDisabled {
		newlyDisabled = append(newlyDisabled, fileName)
	}
	disableFailed := r.periodDisableFailed
	unresolved := r.periodUnresolved
	monitored := r.periodMonitored
	r.mu.Unlock()
	sort.Strings(newlyDisabled)

	normal := health.Normal()
	normalPct := percent(normal, health.Total)
	disabledPct := percent(health.Disabled, health.Total)

	var b strings.Builder
	b.WriteString("🛡️ 号池守护 · 健康度汇总\n\n")
	b.WriteString("📊 总览\n")
	fmt.Fprintf(&b, "  • 总数 %d\n", health.Total)
	fmt.Fprintf(&b, "  • 正常 %d (%.1f%%)\n", normal, normalPct)
	fmt.Fprintf(&b, "  • 禁用 %d (%.1f%%)", health.Disabled, disabledPct)

	if len(newlyDisabled) > 0 {
		fmt.Fprintf(&b, "\n\n🚫 本周期新禁用 %d 个", len(newlyDisabled))
		for _, name := range newlyDisabled {
			fmt.Fprintf(&b, "\n  • %s", name)
		}
	} else {
		b.WriteString("\n\n🚫 本周期新禁用 0 个")
	}

	if len(recovered) > 0 {
		names := make([]string, 0, len(recovered))
		for _, account := range recovered {
			names = append(names, recoveredLabel(account))
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "\n\n♻️ 恢复探测 %d 个 · 已恢复 %d 个", disabledCount, len(recovered))
		b.WriteString("\n  建议手动启用:")
		for _, name := range names {
			fmt.Fprintf(&b, "\n  • %s", name)
		}
	} else if disabledCount > 0 {
		fmt.Fprintf(&b, "\n\n♻️ 恢复探测 %d 个 · 无恢复", disabledCount)
	}

	if monitored > 0 {
		fmt.Fprintf(&b, "\n\n🔭 监测命中(不禁用) %d 次", monitored)
	}
	if unresolved > 0 {
		fmt.Fprintf(&b, "\n⚠️ 无法禁用(无 FileName/未同步) %d 个", unresolved)
	}
	if disableFailed > 0 {
		fmt.Fprintf(&b, "\n❌ 禁用失败 %d 次", disableFailed)
	}
	return b.String()
}

// recoveredLabel 选用 FileName 作为恢复账号的展示标签,缺失时回退到 Name/AuthIndex。
func recoveredLabel(account disabledAccount) string {
	if account.FileName != "" {
		return account.FileName
	}
	if account.Name != "" {
		return account.Name
	}
	return account.AuthIndex
}

// resetPeriod 在每轮告警发送后清空周期计数，开始新周期统计。
func (r *AccountGuardRunner) resetPeriod() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.periodDisabled = make(map[string]struct{})
	r.periodDisableFailed = 0
	r.periodUnresolved = 0
	r.periodMonitored = 0
	// 顺带清理过期的冷却记录，避免长期运行内存增长。
	cutoff := r.now().Add(-accountGuardDisableCooldown)
	for fileName, at := range r.lastDisabledAt {
		if at.Before(cutoff) {
			delete(r.lastDisabledAt, fileName)
		}
	}
}

// percent 计算占比，分母为 0 时返回 0，避免除零。
func percent(part, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}
