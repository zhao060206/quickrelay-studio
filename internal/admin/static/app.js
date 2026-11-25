"use strict";

const translations = {
  title: ["QuickWork Console", "QuickWork 控制台"],
  language: ["Language", "语言"],
  skipContent: ["Skip to content", "跳至主要内容"],
  console: ["Management Console", "管理控制台"],
  loginIntro: ["Manage upstream accounts, edit settings, and try the OpenAI-compatible API.", "管理上游账号、调整设置，并测试 OpenAI 兼容接口。"],
  checkingService: ["Checking service status…", "正在检查服务状态…"],
  adminPassword: ["Console password", "管理密码"],
  signIn: ["Sign in", "登录"],
  reconnect: ["Reconnect", "重新连接"],
  loginSecurity: ["The administrator password is separate from inference API keys. Use HTTPS for remote access.", "管理密码与推理 API Key 分开使用。远程访问请使用 HTTPS。"],
  consoleHome: ["QuickWork console home", "QuickWork 控制台首页"],
  workspace: ["Workspace", "工作空间"],
  managementPages: ["Management pages", "管理页面"],
  overview: ["Overview", "概览"],
  accounts: ["Accounts", "账号管理"],
  settings: ["Settings", "服务设置"],
  playground: ["Playground", "Playground"],
  serverCredentials: ["Credentials stay on the server", "凭证仅保存在服务端"],
  nativeGo: ["Native Go · OpenAI compatible", "原生 Go · OpenAI 兼容"],
  darkMode: ["Dark mode", "深色模式"],
  lightMode: ["Light mode", "浅色模式"],
  switchDark: ["Switch to dark mode", "切换深色模式"],
  switchLight: ["Switch to light mode", "切换浅色模式"],
  refresh: ["Refresh", "刷新"],
  signOut: ["Sign out", "退出登录"],
  restartNotice: ["Settings saved. Restart the service to apply the changes.", "设置已保存。重新启动服务后，新设置才会生效。"],
  serviceOverview: ["Service overview", "服务概览"],
  overviewIntro: ["View real upstream attempts, recorded token usage, and account availability.", "查看真实上游调用、已记录 Token 用量和账号可用状态。"],
  loading: ["Loading", "正在加载"],
  storedAccounts: ["Stored accounts", "已保存账号"],
  sqliteAccounts: ["Upstream accounts in SQLite", "SQLite 中的上游账号"],
  enabledAccounts: ["Enabled accounts", "启用账号"],
  rotationAccounts: ["Participating in request rotation", "参与请求轮询"],
  expiredTokens: ["Expired tokens", "Token 已过期"],
  autoRefreshTokens: ["Refresh is attempted on requests", "请求时尝试自动刷新"],
  accountsWithErrors: ["Accounts with errors", "存在错误的账号"],
  manualRefreshHint: ["Refresh from the Accounts page", "可在账号页面手动刷新"],
  runtime: ["Runtime status", "运行状态"],
  currentProcess: ["Current process", "当前进程"],
  service: ["Service", "服务"],
  startedAt: ["Started at", "启动时间"],
  accountUses: ["Account uses", "账号使用次数"],
  inferenceAuth: ["Inference API authentication", "推理 API 鉴权"],
  managementUI: ["Management UI", "管理界面"],
  modelAliases: ["Available model aliases", "可用模型别名"],
  modelsHint: ["Aliases come from the running configuration, not a live upstream model or quota lookup.", "这些别名来自当前运行配置，并非上游实时模型或额度查询。"],
  openPlayground: ["Open Playground", "打开 Playground"],
  getStarted: ["Get started", "开始使用"],
  startWithAccount: ["Start with an upstream account", "从一个上游账号开始"],
  getStartedHint: ["Sign in from Accounts, then test chat with a separate inference API key.", "在账号管理完成网页登录，再用独立的推理 API Key 测试聊天。"],
  manageAccounts: ["Manage accounts", "管理账号"],
  upstreamAccounts: ["Upstream accounts", "上游账号"],
  accountsIntro: ["Enabled accounts rotate in order. Disabling an account keeps its stored credentials.", "启用账号依次参与轮询；停用不会删除已保存的凭证。"],
  refreshAllTokens: ["Refresh all tokens", "刷新全部 Token"],
  account: ["Account", "账号"],
  providerRegion: ["Provider / region", "提供者 / 区域"],
  status: ["Status", "状态"],
  tokenExpires: ["Token expires", "Token 到期时间"],
  uses: ["Uses", "使用次数"],
  actions: ["Actions", "操作"],
  noAccounts: ["No accounts yet. Connect an upstream account using the sign-in flow below.", "尚未添加账号。请使用下方登录流程连接上游账号。"],
  addAccount: ["Add or reauthenticate an account", "添加 / 重新登录账号"],
  addAccountHint: ["Sign in on the official website. The server exchanges credentials and stores the account.", "通过官方网站完成登录，凭证由服务器交换并写入账号数据库。"],
  provider: ["Provider", "提供者"],
  environment: ["Environment", "环境"],
  regionHint: ["Region (optional, e.g. us-east-1)", "区域（选填，例如 us-east-1）"],
  startSignIn: ["Start sign-in", "开始登录"],
  waitingSignIn: ["Waiting for sign-in", "等待登录"],
  discoveryStep: ["Open discovery and sign in", "打开 discovery 页面并登录"],
  authorizeStep: ["Paste the callback to get an authorization link", "粘贴回调以获取授权链接"],
  completeStep: ["Authorize, then paste the final callback", "完成授权后粘贴最终回调"],
  remoteCallbackHint: ["An unreachable localhost or 127.0.0.1 page is expected in remote / Docker mode. Copy the complete URL from the browser address bar and paste it below. No public callback port is needed.", "浏览器跳转到无法连接的 localhost 或 127.0.0.1 页面是远程 / Docker 模式的正常情况。直接复制地址栏的完整 URL 并粘贴到下方，无需对外开放回调端口。"],
  openOfficialSignIn: ["Open official sign-in", "打开官方登录页面"],
  openOfficialAuthorization: ["Open official authorization", "打开官方授权页面"],
  copyLink: ["Copy link", "复制链接"],
  fullCallbackURL: ["Complete callback URL from the browser address bar", "浏览器地址栏的完整回调 URL"],
  callbackPrivacy: ["Include all query parameters. The field is cleared on submission and never saved in browser storage.", "包含全部查询参数。提交后立即清空，不会记录到浏览器存储空间。"],
  submitCallback: ["Submit callback URL", "提交回调 URL"],
  updateSignIn: ["Update sign-in status", "更新登录状态"],
  cancelSignIn: ["Cancel this sign-in", "取消本次登录"],
  serviceSettings: ["Service settings", "服务设置"],
  settingsIntro: ["Changes are saved to disk, not hot-reloaded into the running process.", "设置仅保存到文件，不会在当前进程中热更新。"],
  restartRequired: ["Restart required after saving", "保存后需要重启"],
  configuration: ["Configuration", "配置内容"],
  reload: ["Reload", "重新读取"],
  settingsWarning: ["Keep the complete JSON structure. Listener, database, and WebUI changes require a restart. Disabling the WebUI will make this console unavailable.", "请保留完整 JSON 结构。修改监听地址、数据库或关闭 WebUI 都需要重新启动；关闭后将无法使用本控制台。"],
  listenerOverride: ["The listener is overridden by a startup argument. Editing the configuration will not override that argument.", "当前监听地址由启动参数覆盖；修改配置文件不会覆盖该启动参数。"],
  settingsJSON: ["Settings JSON", "配置 JSON"],
  inferenceKeys: ["Inference API keys", "推理 API Keys"],
  keysHint: ["Existing keys are never sent to the browser. Leave blank to keep them; new values replace all keys.", "现有密钥不会返回浏览器。留空保留原有密钥，输入新值将替换全部密钥。"],
  replacementKeys: ["Replacement keys (one per line, optional)", "替换密钥（每行一个，选填）"],
  clearAllKeys: ["Clear all API keys (disable key authentication for inference)", "清空全部 API Keys（关闭推理接口的密钥鉴权）"],
  saveHint: ["After a successful save, restart the service to apply the new settings.", "成功保存后，重新启动服务以应用新设置。"],
  saveSettings: ["Save settings", "保存设置"],
  playgroundIntro: ["Send a real non-streaming upstream request. This may consume account quota.", "通过非流式聊天接口发送真实上游请求，可能消耗账号额度。"],
  sendMessage: ["Send a message", "发送消息"],
  modelAlias: ["Model alias", "模型别名"],
  inferenceKeyLabel: ["Inference API key (not the administrator password)", "推理 API Key（不是管理密码）"],
  keyMemoryHint: ["The key stays in this page's memory and is cleared on sign-out.", "密钥仅保留在本页内存中，退出登录时清除。"],
  message: ["Message", "消息"],
  sendRequest: ["Send request", "发送请求"],
  cancelRequest: ["Cancel request", "取消请求"],
  modelResponse: ["Model response", "模型回复"],
  clearOutput: ["Clear output", "清空输出"],
  noRequest: ["No request sent", "尚未发送请求"],
  footer: ["QuickWork OpenAI · Upstream access credentials are never displayed in the console.", "QuickWork OpenAI · 控制台不会显示上游访问凭证。"],
  genericError: ["The request failed. Please try again later.", "请求失败，请稍后重试。"],
  loginReady: ["Enter the management password configured on the server. This is not an inference API key.", "请输入服务器配置的管理密码。这不是推理 API Key。"],
  loginUnconfigured: ["Sign-in is disabled because no management password is configured. Set the management password environment variable on the server, then restart.", "尚未配置管理密码，登录已停用。请在服务器设置管理密码环境变量后重新启动。"],
  sessionChanged: ["The management session changed", "管理会话已变更"],
  sessionExpired: ["The management session expired. Please sign in again.", "管理会话已失效，请重新登录。"],
  httpError: ["Request failed (HTTP {status})", "请求失败（HTTP {status}）"],
  invalidResponse: ["The service did not return a valid JSON response.", "服务未返回有效的 JSON 响应。"],
  connectionFailed: ["The management service is temporarily unreachable.", "暂时无法连接管理服务。"],
  authEnabled: ["API key authentication enabled", "已启用 API Key 鉴权"],
  authDisabled: ["Not enabled; restrict access", "未启用，请限制访问范围"],
  enabled: ["Enabled", "已启用"],
  disabled: ["Disabled", "已停用"],
  serviceReachable: ["Service reachable", "服务可连接"],
  defaultModel: ["Default: {model}", "默认：{model}"],
  playgroundKeyRequired: ["API key authentication is enabled. Use an inference key, not the management password. The key stays only in this page's memory.", "此服务已启用 API Key 鉴权。请填写推理密钥而非管理密码，密钥仅保留在本页内存中。"],
  playgroundKeyOptional: ["API key authentication is disabled, so this field may be empty. Do not enter the management password here.", "此服务未启用 API Key 鉴权，可留空。请勿在此填写管理密码。"],
  confirmDelete: ["Delete account “{name}”? This removes its local credentials. You will need to sign in again.", "确定删除账号“{name}”？这会移除本地凭证，之后需要重新登录。"],
  accountUpdated: ["Account status updated.", "账号状态已更新。"],
  accountID: ["Account #{id}", "账号 #{id}"],
  noRegion: ["No region specified", "未指定区域"],
  hasError: ["Has error", "存在错误"],
  expired: ["Expired", "已过期"],
  lastUsed: ["Last: {time}", "最近：{time}"],
  neverUsed: ["Not used yet", "尚未使用"],
  enable: ["Enable", "启用"],
  disable: ["Disable", "停用"],
  delete: ["Delete", "删除"],
  keyCount: ["{count} stored keys", "已保存 {count} 个密钥"],
  flowDiscovery: ["Step 1: Open discovery, sign in, and paste the complete callback URL", "步骤 1：打开 discovery 链接，登录后粘贴完整回调 URL"],
  flowAuthorize: ["Step 2: Open authorization, approve, and paste the final callback URL", "步骤 2：打开授权链接，授权后粘贴最终回调 URL"],
  flowComplete: ["Account sign-in complete; saved on the server", "账号登录完成，已保存至服务器"],
  flowExpired: ["This sign-in expired. Please start again.", "本次登录已过期，请重新开始。"],
  flowCancelled: ["This sign-in was cancelled", "本次登录已取消"],
  flowUpdating: ["Updating sign-in status", "正在更新登录状态"],
  flowExpires: ["Expires {time}", "有效期至 {time}"],
  signInSuccess: ["Upstream account signed in successfully.", "上游账号登录成功。"],
  signInIncomplete: ["Sign-in did not complete. Please try again.", "登录未完成，请重新尝试。"],
  signingOut: ["Signing out securely…", "正在安全退出…"],
  signOutUnconfirmed: ["Page data cleared, but server sign-out was not confirmed. Close the browser or try again later.", "本页数据已清除，但未确认服务器退出成功。请关闭浏览器或稍后重试。"],
  confirmDiscard: ["Reloading will discard unsaved settings. Continue?", "重新读取会丢弃尚未保存的设置，确定继续？"],
  pageUpdated: ["Page updated.", "当前页面已更新。"],
  tokensRefreshed: ["Refresh complete: {successful} succeeded, {failed} failed.", "刷新完成：{successful} 个成功，{failed} 个失败。"],
  confirmRestartSignIn: ["A sign-in is still in progress. Cancel it and start again?", "当前还有未完成的登录，是否取消并重新开始？"],
  signInCancelled: ["This sign-in was cancelled.", "已取消本次登录。"],
  linkCopied: ["Sign-in link copied.", "登录链接已复制。"],
  copyUnavailable: ["Automatic copying is unavailable. Right-click the official sign-in button and choose “Copy link address”.", "当前浏览器无法自动复制。请右键点击官方登录按钮并选择“复制链接地址”。"],
  settingsReloaded: ["Configuration reloaded.", "配置已重新读取。"],
  jsonObjectRequired: ["Settings must be a JSON object.", "配置必须是 JSON 对象。"],
  invalidSettings: ["Invalid JSON configuration. Check the syntax and use a JSON object.", "JSON 配置无效。请检查语法，并使用 JSON 对象。"],
  confirmSave: ["Changes require a manual service restart after saving. Save settings?", "保存后需要手动重新启动服务才会生效。确定保存？"],
  warnDisableWebUI: ["Warning: the management UI will be disabled after restarting.", "警告：重新启动后将关闭管理界面。"],
  warnClearKeys: ["Warning: all API keys will be cleared and inference requests will no longer require a key.", "警告：全部 API Keys 将被清空，推理接口将不再要求密钥。"],
  settingsSaved: ["Settings saved. Restart the service manually to apply the changes.", "配置已保存，请手动重新启动服务以应用变更。"],
  waitingResponse: ["Waiting for the upstream response…", "正在等待上游响应…"],
  noResponseText: ["The response contains no text.", "响应没有文本内容。"],
  requestComplete: ["Request complete", "请求已完成"],
  requestCancelled: ["Request cancelled", "请求已取消"],
  requestFailed: ["Request failed", "请求失败"],
  networkError: ["The service could not be reached. Check the connection and try again.", "无法连接服务，请检查连接后重试。"],
  fieldRequired: ["Please complete this field.", "请填写此字段。"],
  validURLRequired: ["Please enter a complete URL.", "请输入完整 URL。"],
  invalidCredentials: ["The management password is incorrect.", "管理密码不正确。"],
  authenticationRequired: ["Sign in to continue. Your session may have expired.", "请先登录，管理会话可能已过期。"],
  invalidCSRF: ["The security token is invalid. Sign out and sign in again.", "安全令牌无效，请退出后重新登录。"],
  adminNotConfigured: ["No management password is configured. Configure it on the server and restart.", "尚未配置管理密码，请在服务器配置后重新启动。"],
  rateLimited: ["Too many requests. Please wait before trying again.", "请求过于频繁，请稍后重试。"],
  sessionLimit: ["The management session limit was reached. Try again later.", "管理会话数已达上限，请稍后重试。"],
  refreshFailed: ["The account token could not be refreshed. Try signing in to the upstream account again.", "账号 Token 刷新失败，请尝试重新登录上游账号。"],
  loginLimit: ["Too many sign-ins are in progress. Cancel an existing attempt or wait for it to expire.", "进行中的登录过多，请取消已有登录或等待其过期。"],
  loginNotFound: ["This sign-in is no longer available. Start a new sign-in.", "本次登录已不存在，请重新开始登录。"],
  configSaveFailed: ["The configuration could not be saved. Check the server's file permissions and try again.", "配置保存失败，请检查服务器文件权限后重试。"],
  databaseError: ["A database operation failed. Check the service state and try again.", "数据库操作失败，请检查服务状态后重试。"],
  upstreamCalls: ["Upstream calls", "上游调用次数"],
  cumulativeDuration: ["Cumulative call time", "累计调用耗时"],
  attemptTimeHint: ["Attempt wall time, excluding backoff and refresh", "单次尝试耗时，不含退避和 Token 刷新"],
  reportedTokens: ["Reported tokens", "官方上报 Token"],
  reportedCost: ["Reported cost (USD)", "官方上报费用（USD）"],
  accountSelections: ["Account selections (legacy)", "账号选择次数（旧计数）"],
  callsAndDuration: ["Calls / time", "调用 / 耗时"],
  tokensAndCost: ["Tokens / credits / USD", "Token / 积分 / 美元"],
  officialMinutes: ["Official minutes quota", "官方分钟额度"],
  syncAllUsage: ["Sync all usage", "同步全部用量"],
  syncUsage: ["Sync usage", "同步用量"],
  refreshToken: ["Refresh token", "刷新 Token"],
  officialUsageSync: ["Official usage sync", "官方用量同步"],
  configureUsageSync: ["Configure sync", "配置同步"],
  quotaUnitHint: ["The official profile reports monthly minutes. ACP credits and USD costs are separate measurements.", "官方 profile 返回月度分钟额度。ACP 积分和美元费用属于不同统计维度。"],
  automaticSync: ["Automatic sync", "自动同步"],
  syncInterval: ["Interval", "同步间隔"],
  syncAttempts: ["Sync attempts", "最多尝试次数"],
  nextSync: ["Next run", "下次同步"],
  syncState: ["Scheduler", "后台任务"],
  syncRunning: ["Running", "正在运行"],
  syncIdle: ["Idle", "空闲"],
  usageSyncHint: ["Automatic sync covers enabled accounts. Manual sync still works when automatic sync is off. Configure settings.usage and restart to apply changes.", "自动同步针对启用账号。关闭自动同步后仍可手动同步。修改 settings.usage 并重启后生效。"],
  usageSettingsHint: ["Official usage sync is controlled by usage.enabled, usage.refresh_interval (1m–24h, default 5m), and usage.max_attempts (1–5, default 3). Disabling it stops automatic sync, not manual sync. Save and restart to apply.", "官方用量同步由 usage.enabled、usage.refresh_interval（1m–24h，默认 5m）和 usage.max_attempts（1–5，默认 3）控制。关闭后仅停止自动同步，不影响手动同步。保存后重启生效。"],
  usageStatisticsHint: ["Each real upstream attempt, including failures and retries, counts once. Statistics start when recording is enabled; older history is not reconstructed. Token totals use official turn_usage only. Missing quota or cost values mean unknown, not zero.", "每次真实上游尝试计一次，包含失败与重试。统计从启用记录时开始，不回溯旧历史。Token 仅使用官方 turn_usage。缺失额度或费用表示未知，不代表零。"],
  callOutcomes: ["{success} succeeded · {failure} failed", "{success} 次成功 · {failure} 次失败"],
  durationSeconds: ["{value}s", "{value} 秒"],
  durationMinutes: ["{value} min", "{value} 分钟"],
  durationHours: ["{value} h", "{value} 小时"],
  inputOutputTokens: ["Input {input} · Output {output}", "输入 {input} · 输出 {output}"],
  totalTokens: ["{count} tokens", "{count} Token"],
  cacheTokens: ["Cache read {read} · write {write}", "缓存读取 {read} · 写入 {write}"],
  costCoverage: ["Cost reported for {reported} / {calls} calls", "{reported} / {calls} 次调用上报费用"],
  partialCost: ["Partial coverage", "费用覆盖不完整"],
  unknown: ["Unknown", "未知"],
  selections: ["Selections: {count}", "选择次数：{count}"],
  statsSince: ["Recorded since {time}", "记录起始：{time}"],
  monthlyQuota: ["Monthly allowance", "月度额度"],
  overageQuota: ["Monthly overage", "月度超额用量"],
  quotaUsed: ["Used {value}", "已用 {value}"],
  quotaLimit: ["Limit {value}", "总额 {value}"],
  quotaRemaining: ["Remaining {value}", "剩余 {value}"],
  minutesValue: ["{value} min", "{value} 分钟"],
  quotaResets: ["Resets {time}", "重置时间：{time}"],
  quotaSynced: ["Synced {time}", "同步时间：{time}"],
  quotaAttempted: ["Last attempt {time}", "上次尝试：{time}"],
  quotaSubscription: ["Subscription: {type}", "订阅类型：{type}"],
  quotaAvailable: ["Available", "已同步"],
  quotaStale: ["Stale; previous values retained", "已过期，保留上次数据"],
  quotaError: ["Sync failed", "同步失败"],
  quotaUnsupported: ["Not supported", "暂不支持"],
  quotaErrorCode: ["Sync error: {code}", "同步错误：{code}"],
  quotaSyncedSuccess: ["Official usage synchronized.", "官方用量同步完成。"],
  quotasSynced: ["Usage sync complete: {successful} succeeded, {failed} failed. Failed accounts retain their previous quota values when available.", "用量同步完成：{successful} 个成功，{failed} 个失败。失败账号在有历史数据时保留上次额度。"],
  usageUnavailable: ["Official usage is unavailable for this account. Missing values remain unknown.", "此账号暂时无法获取官方用量，缺失数据保持未知。"],
  usageUnauthorized: ["Official usage access was denied. Reauthenticate the account and try again.", "官方用量访问被拒绝，请重新登录账号后重试。"],
  usageFailed: ["Official usage sync failed. Previous values are retained when available.", "官方用量同步失败，有历史数据时保留上次结果。"],
  usageBusy: ["An official usage sync is already running. Try again shortly.", "已有官方用量同步正在运行，请稍后重试。"],
  officialCredits: ["Official reported credits", "官方上报积分"],
  separateUnits: ["Units are counted separately", "按不同单位独立累计"],
  creditsHint: ["Credit amounts come only from official ACP metadata. Ordinary chat may not report them. Missing values are unknown; credits, USD, and minutes are never converted into one another.", "积分仅来自官方 ACP metadata，普通聊天可能不返回。缺失表示未知，积分、美元和分钟之间不进行换算。"],
  creditsNotReported: ["Credits not reported", "未上报积分"],
  creditsCoverage: ["Reported for {reported} / {calls} calls", "{reported} / {calls} 次调用上报"],
  partialCredits: ["Partial credit coverage", "积分覆盖不完整"],
  creditAmount: ["{amount} {unit}", "{amount} {unit}"]
};
const apiErrorTranslations = {
  invalid_credentials: "invalidCredentials",
  unauthorized: "authenticationRequired",
  invalid_csrf: "invalidCSRF",
  admin_not_configured: "adminNotConfigured",
  rate_limited: "rateLimited",
  session_limit: "sessionLimit",
  refresh_failed: "refreshFailed",
  login_limit: "loginLimit",
  login_not_found: "loginNotFound",
  config_save_failed: "configSaveFailed",
  database_error: "databaseError",
  usage_unavailable: "usageUnavailable",
  usage_unauthorized: "usageUnauthorized",
  usage_failed: "usageFailed",
  usage_busy: "usageBusy"
};
let language = readPreference("language") === "zh-CN" ? "zh-CN" : "en";
const translationParameters = new WeakMap();

function readPreference(name) {
  try {
    return window.localStorage.getItem(`quickwork.${name}`);
  } catch {
    return null;
  }
}

function savePreference(name, value) {
  try {
    window.localStorage.setItem(`quickwork.${name}`, value);
  } catch {
    return;
  }
}

function formatNumber(value) {
  return new Intl.NumberFormat(language).format(value);
}

function formatOptionalNumber(value, precise = false) {
  if (typeof value !== "number" || !Number.isFinite(value)) return translate("unknown");
  return precise ? new Intl.NumberFormat(language, { maximumSignificantDigits: 12 }).format(value) : formatNumber(value);
}

function formatDuration(milliseconds) {
  if (typeof milliseconds !== "number" || !Number.isFinite(milliseconds)) return translate("unknown");
  const seconds = Math.max(0, milliseconds) / 1000;
  if (seconds < 60) return translate("durationSeconds", { value: Number(seconds.toFixed(1)) });
  if (seconds < 3600) return translate("durationMinutes", { value: Number((seconds / 60).toFixed(1)) });
  return translate("durationHours", { value: Number((seconds / 3600).toFixed(2)) });
}

function formatUSD(value) {
  return typeof value === "number" && Number.isFinite(value)
    ? new Intl.NumberFormat(language, { style: "currency", currency: "USD", currencyDisplay: "code", maximumSignificantDigits: 12 }).format(value)
    : translate("unknown");
}

function translate(key, parameters = {}) {
  const template = translations[key]?.[language === "zh-CN" ? 1 : 0] || translations[key]?.[0] || key;
  return template.replace(/\{([a-zA-Z]+)\}/g, (match, name) => Object.hasOwn(parameters, name) ? typeof parameters[name] === "number" ? formatNumber(parameters[name]) : String(parameters[name]) : match);
}

function setTranslation(element, key, parameters = {}) {
  element.dataset.i18n = key;
  translationParameters.set(element, parameters);
  element.textContent = translate(key, parameters);
}

function setRawText(element, value) {
  delete element.dataset.i18n;
  translationParameters.delete(element);
  element.textContent = value;
}

function localizedError(key, parameters = {}) {
  const error = new Error(translate(key, parameters));
  error.translationKey = key;
  error.translationParameters = parameters;
  return error;
}

function showErrorAt(element, error) {
  if (error.translationKey) setTranslation(element, error.translationKey, error.translationParameters);
  else setRawText(element, error.message || translate("genericError"));
}

function setLanguage(value) {
  language = value === "zh-CN" ? "zh-CN" : "en";
  document.documentElement.lang = language;
  document.title = translate("title");
  for (const element of document.querySelectorAll("[data-i18n]")) element.textContent = translate(element.dataset.i18n, translationParameters.get(element));
  for (const element of document.querySelectorAll("[data-i18n-aria-label]")) element.setAttribute("aria-label", translate(element.dataset.i18nAriaLabel));
  for (const select of document.querySelectorAll("[data-language-select]")) select.value = language;
  for (const field of document.querySelectorAll("input, textarea, select")) field.setCustomValidity("");
  setTheme(document.body.dataset.theme === "dark");
  setTranslation(elements["page-title"], pageTitles[state.page]);
  if (state.status) renderStatus(state.status);
  if (state.authenticated) renderAccounts();
  if (state.config) renderConfigMetadata(state.config);
  if (state.login) renderLogin(state.login);
}

const elements = Object.fromEntries(Array.from(document.querySelectorAll("[id]")).map(element => [element.id, element]));
const state = {
  authenticated: false,
  configured: false,
  csrf: "",
  generation: 0,
  requests: new Set(),
  page: "overview",
  status: null,
  config: null,
  accounts: [],
  login: null,
  loginRevision: 0,
  playgroundController: null
};
const pageTitles = { overview: "overview", accounts: "accounts", config: "settings", playground: "playground" };
const initialDark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;

function setTheme(dark) {
  document.body.dataset.theme = dark ? "dark" : "light";
  setTranslation(elements["theme-toggle"], dark ? "lightMode" : "darkMode");
  elements["theme-toggle"].dataset.i18nAriaLabel = dark ? "switchLight" : "switchDark";
  elements["theme-toggle"].setAttribute("aria-label", translate(dark ? "switchLight" : "switchDark"));
}

function showNotice(message, kind = "") {
  setRawText(elements["app-notice"], message);
  elements["app-notice"].className = `notice${kind ? ` ${kind}` : ""}`;
  elements["app-notice"].hidden = !message;
}

function showMessage(key, kind = "", parameters = {}) {
  showNotice(translate(key, parameters), kind);
  setTranslation(elements["app-notice"], key, parameters);
}

function reportError(error) {
  if (error.name !== "AbortError" && state.authenticated) {
    if (error.translationKey) showMessage(error.translationKey, "error", error.translationParameters);
    else if (error.message) showNotice(error.message, "error");
    else showMessage("genericError", "error");
  }
}

function abortRequests() {
  for (const controller of state.requests) controller.abort();
  state.requests.clear();
  state.playgroundController = null;
}

function clearSession() {
  state.generation++;
  state.loginRevision++;
  abortRequests();
  state.authenticated = false;
  state.csrf = "";
  state.status = null;
  state.config = null;
  state.accounts = [];
  state.login = null;
  elements["login-password"].value = "";
  elements["playground-api-key"].value = "";
  elements["playground-prompt"].value = "";
  setRawText(elements["playground-output"], "");
  setTranslation(elements["playground-state"], "noRequest");
  elements["playground-model"].replaceChildren();
  elements["config-json"].value = "";
  elements["replacement-api-keys"].value = "";
  elements["clear-api-keys"].checked = false;
  elements["replacement-api-keys"].disabled = false;
  elements["oauth-callback"].value = "";
  elements["oauth-region"].value = "";
  elements["oauth-link"].removeAttribute("href");
  elements["oauth-flow"].hidden = true;
  for (const id of ["start-login", "submit-callback", "check-login", "cancel-login"]) elements[id].disabled = false;
  elements["accounts-body"].replaceChildren();
  elements["model-list"].replaceChildren();
  elements["overview-credits"].replaceChildren();
  for (const name of ["total", "enabled", "expired", "errors"]) elements[`metric-${name}`].textContent = "—";
  for (const id of ["service-name", "service-started", "service-uses", "service-api-auth", "service-webui", "default-model", "api-key-count", "metric-calls", "metric-duration", "metric-tokens", "metric-cost", "metric-call-outcomes", "metric-token-breakdown", "metric-cost-coverage", "usage-sync-enabled", "usage-sync-interval", "usage-sync-attempts", "usage-sync-next", "usage-sync-state"]) setRawText(elements[id], "—");
  setTranslation(elements["service-badge"], "loading");
  elements["accounts-empty"].hidden = true;
  elements["config-override"].hidden = true;
  elements["restart-notice"].hidden = true;
  elements["app-view"].hidden = true;
  elements["login-view"].hidden = false;
  elements["cancel-playground"].disabled = true;
  elements["send-playground"].disabled = false;
  showNotice("");
  updateLoginAvailability();
}

function updateLoginAvailability() {
  elements["login-password"].disabled = !state.configured;
  elements["login-submit"].disabled = !state.configured;
  setTranslation(elements["login-notice"], state.configured ? "loginReady" : "loginUnconfigured");
  elements["login-notice"].className = state.configured ? "notice" : "notice warning";
}

async function requestJSON(path, options = {}) {
  const generation = state.generation;
  const controller = options.controller || new AbortController();
  const method = options.method || "GET";
  const management = options.management !== false;
  const headers = { Accept: "application/json", ...options.headers };
  if (method !== "GET" && method !== "HEAD") {
    headers["Content-Type"] = "application/json";
    if (management && state.csrf) headers["X-CSRF-Token"] = state.csrf;
  }
  state.requests.add(controller);
  try {
    const response = await fetch(path, {
      method,
      headers,
      credentials: "same-origin",
      cache: "no-store",
      signal: controller.signal,
      body: options.body === undefined ? undefined : JSON.stringify(options.body)
    });
    const data = response.status === 204 ? null : await response.json().catch(() => null);
    if (generation !== state.generation) throw new DOMException(translate("sessionChanged"), "AbortError");
    if (!response.ok) {
      if (response.status === 401 && management && path !== "/api/auth/login" && path !== "/api/auth/session") {
        clearSession();
        setTranslation(elements["login-error"], "sessionExpired");
      }
      const translationKey = management ? apiErrorTranslations[data?.error?.code] : null;
      const error = translationKey ? localizedError(translationKey) : data?.error?.message ? new Error(data.error.message) : localizedError("httpError", { status: response.status });
      error.status = response.status;
      error.code = data?.error?.code || "";
      throw error;
    }
    if (response.status !== 204 && data === null) throw localizedError("invalidResponse");
    return data;
  } catch (error) {
    if (error instanceof TypeError) throw localizedError("networkError");
    throw error;
  } finally {
    state.requests.delete(controller);
  }
}

async function checkSession() {
  const generation = state.generation;
  elements["login-retry"].hidden = true;
  setRawText(elements["login-error"], "");
  try {
    const session = await requestJSON("/api/auth/session");
    if (generation !== state.generation) return;
    state.configured = Boolean(session.configured);
    if (session.authenticated) {
      enterApp(session);
      await loadWorkspace();
    } else {
      updateLoginAvailability();
    }
  } catch (error) {
    if (error.name === "AbortError" || generation !== state.generation) return;
    if (state.authenticated) {
      reportError(error);
    } else {
      setTranslation(elements["login-notice"], "connectionFailed");
      showErrorAt(elements["login-error"], error);
      elements["login-retry"].hidden = false;
    }
  }
}

function enterApp(session) {
  state.authenticated = true;
  state.csrf = session.csrf_token || "";
  elements["login-password"].value = "";
  setRawText(elements["login-error"], "");
  elements["login-view"].hidden = true;
  elements["app-view"].hidden = false;
  openPage("overview");
}

function openPage(page) {
  if (!Object.hasOwn(pageTitles, page)) return;
  state.page = page;
  for (const name of Object.keys(pageTitles)) elements[`page-${name}`].hidden = name !== page;
  for (const button of document.querySelectorAll("[data-page]")) {
    button.classList.toggle("active", button.dataset.page === page);
    if (button.dataset.page === page) button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
  }
  setTranslation(elements["page-title"], pageTitles[page]);
  showNotice("");
}

function formatTime(epoch) {
  if (!epoch) return "—";
  const date = new Date(Number(epoch) * 1000);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString(language, { hour12: false });
}

function textElement(tag, text, className) {
  const element = document.createElement(tag);
  element.textContent = text;
  if (className) element.className = className;
  return element;
}

async function loadStatus() {
  const generation = state.generation;
  const status = await requestJSON("/api/status");
  if (!state.authenticated || generation !== state.generation) return;
  state.status = status;
  renderStatus(status);
}

function renderStatus(status) {
  const accounts = status.accounts || {};
  for (const name of ["total", "enabled", "expired", "errors"]) elements[`metric-${name}`].textContent = formatNumber(accounts[name] || 0);
  elements["service-name"].textContent = status.service || "quickwork-openai";
  elements["service-started"].textContent = formatTime(status.started_at);
  elements["service-uses"].textContent = formatNumber(accounts.uses || 0);
  setTranslation(elements["service-api-auth"], status.api_keys_configured ? "authEnabled" : "authDisabled");
  setTranslation(elements["service-webui"], status.webui_enabled ? "enabled" : "disabled");
  setTranslation(elements["service-badge"], "serviceReachable");
  setRawText(elements["metric-calls"], formatOptionalNumber(accounts.calls));
  setRawText(elements["metric-duration"], formatDuration(accounts.total_duration_ms));
  setRawText(elements["metric-tokens"], formatOptionalNumber(accounts.total_tokens));
  setRawText(elements["metric-cost"], formatUSD(accounts.cost_usd));
  setTranslation(elements["metric-call-outcomes"], "callOutcomes", { success: formatOptionalNumber(accounts.successes), failure: formatOptionalNumber(accounts.failures) });
  setTranslation(elements["metric-token-breakdown"], "inputOutputTokens", { input: formatOptionalNumber(accounts.prompt_tokens), output: formatOptionalNumber(accounts.completion_tokens) });
  setTranslation(elements["metric-cost-coverage"], "costCoverage", { reported: formatOptionalNumber(accounts.cost_reported_calls), calls: formatOptionalNumber(accounts.calls) });
  elements["overview-credits"].replaceChildren(...renderCredits(accounts.credits, accounts.calls));
  const sync = status.usage_sync || {};
  if (typeof sync.enabled === "boolean") setTranslation(elements["usage-sync-enabled"], sync.enabled ? "enabled" : "disabled");
  else setTranslation(elements["usage-sync-enabled"], "unknown");
  setRawText(elements["usage-sync-interval"], sync.refresh_interval || "—");
  setRawText(elements["usage-sync-attempts"], formatOptionalNumber(sync.max_attempts));
  setRawText(elements["usage-sync-next"], formatTime(sync.next_run_at));
  setTranslation(elements["usage-sync-state"], sync.running ? "syncRunning" : "syncIdle");
  setTranslation(elements["default-model"], "defaultModel", { model: status.default_model || "—" });
  elements["model-list"].replaceChildren(...(status.models || []).map(model => textElement("span", model, "chip")));
  const previousModel = elements["playground-model"].value;
  elements["playground-model"].replaceChildren(...(status.models || []).map(model => {
    const option = textElement("option", model);
    option.value = model;
    return option;
  }));
  elements["playground-model"].value = (status.models || []).includes(previousModel) ? previousModel : status.default_model || status.models?.[0] || "";
  setTranslation(elements["playground-auth-hint"], status.api_keys_configured ? "playgroundKeyRequired" : "playgroundKeyOptional");
}

function makeAccountAction(label, action, account, danger = false) {
  const button = textElement("button", label, `button small subtle${danger ? " danger" : ""}`);
  button.type = "button";
  button.addEventListener("click", async () => {
    const generation = state.generation;
    const display = account.display_name || account.email || `#${account.id}`;
    if (action === "delete" && !window.confirm(translate("confirmDelete", { name: display }))) return;
    button.disabled = true;
    try {
      const path = `/api/accounts/${encodeURIComponent(account.id)}`;
      if (action === "delete") await requestJSON(path, { method: "DELETE" });
      if (action === "toggle") await requestJSON(path, { method: "PATCH", body: { disabled: !account.disabled } });
      if (action === "refresh") await requestJSON(`${path}/refresh`, { method: "POST", body: {} });
      if (action === "usage") await requestJSON(`${path}/usage/refresh`, { method: "POST", body: {} });
      if (!state.authenticated || generation !== state.generation) return;
      await Promise.all([loadAccounts(), loadStatus()]);
      if (state.authenticated && generation === state.generation) showMessage(action === "usage" ? "quotaSyncedSuccess" : "accountUpdated", "success");
    } catch (error) {
      if (action === "usage" && state.authenticated && generation === state.generation) {
        try {
          await Promise.all([loadAccounts(), loadStatus()]);
        } catch (reloadError) {
          if (reloadError.name === "AbortError") return;
        }
      }
      if (state.authenticated && generation === state.generation) reportError(error);
    } finally {
      button.disabled = false;
    }
  });
  return button;
}

async function loadAccounts() {
  const generation = state.generation;
  const data = await requestJSON("/api/accounts");
  if (!state.authenticated || generation !== state.generation) return;
  state.accounts = data.accounts || [];
  renderAccounts();
}

function renderAccounts() {
  const rows = state.accounts.map(account => {
    const row = document.createElement("tr");
    const identity = document.createElement("td");
    identity.append(textElement("div", account.display_name || account.email || translate("accountID", { id: account.id }), "account-label"));
    identity.append(textElement("div", `#${account.id}${account.email ? ` · ${account.email}` : ""}`, "cell-note"));
    const provider = document.createElement("td");
    provider.append(textElement("div", `${account.provider || "—"} / ${account.build_stage || "prod"}`));
    provider.append(textElement("div", account.region || translate("noRegion"), "cell-note"));
    const status = document.createElement("td");
    const badges = textElement("div", "", "status-badges");
    badges.append(textElement("span", translate(account.disabled ? "disabled" : "enabled"), `badge${account.disabled ? " neutral" : ""}`));
    if (account.has_error) badges.append(textElement("span", translate("hasError"), "badge error"));
    if (account.expires_at && Number(account.expires_at) * 1000 <= Date.now()) badges.append(textElement("span", translate("expired"), "badge warning"));
    status.append(badges);
    const expires = textElement("td", formatTime(account.expires_at));
    const stats = account.usage || {};
    const usage = textElement("td", "", "usage-cell");
    usage.append(textElement("div", `${formatOptionalNumber(stats.call_count)} · ${formatDuration(stats.total_duration_ms)}`, "usage-value"));
    usage.append(textElement("div", translate("callOutcomes", { success: formatOptionalNumber(stats.success_count), failure: formatOptionalNumber(stats.failure_count) }), "cell-note"));
    usage.append(textElement("div", translate("selections", { count: account.use_count || 0 }), "cell-note"));
    usage.append(textElement("div", account.last_used_at ? translate("lastUsed", { time: formatTime(account.last_used_at) }) : translate("neverUsed"), "cell-note"));
    if (stats.stats_since) usage.append(textElement("div", translate("statsSince", { time: formatTime(stats.stats_since) }), "cell-note"));
    const tokens = textElement("td", "", "usage-cell");
    tokens.append(textElement("div", translate("totalTokens", { count: formatOptionalNumber(stats.total_tokens) }), "usage-value"));
    tokens.append(textElement("div", translate("inputOutputTokens", { input: formatOptionalNumber(stats.prompt_tokens), output: formatOptionalNumber(stats.completion_tokens) }), "cell-note"));
    tokens.append(textElement("div", translate("cacheTokens", { read: formatOptionalNumber(stats.cache_read_tokens), write: formatOptionalNumber(stats.cache_write_tokens) }), "cell-note"));
    tokens.append(textElement("div", formatUSD(stats.cost_usd), "usage-value"));
    tokens.append(textElement("div", translate("costCoverage", { reported: formatOptionalNumber(stats.cost_reported_calls), calls: formatOptionalNumber(stats.call_count) }), "cell-note"));
    if (typeof stats.cost_usd === "number" && stats.cost_reported_calls < stats.call_count) tokens.append(textElement("span", translate("partialCost"), "badge warning"));
    const creditDetails = document.createElement("details");
    creditDetails.className = "credit-details";
    creditDetails.open = Array.isArray(stats.credits) && stats.credits.length > 0;
    creditDetails.append(textElement("summary", translate("officialCredits")), ...renderCredits(stats.credits, stats.call_count));
    tokens.append(creditDetails);
    const quota = renderQuota(account.quota);
    const actions = document.createElement("td");
    const actionGroup = textElement("div", "", "table-actions");
    actionGroup.append(makeAccountAction(translate("syncUsage"), "usage", account), makeAccountAction(translate("refreshToken"), "refresh", account), makeAccountAction(translate(account.disabled ? "enable" : "disable"), "toggle", account), makeAccountAction(translate("delete"), "delete", account, true));
    actions.append(actionGroup);
    row.append(identity, provider, status, expires, usage, tokens, quota, actions);
    return row;
  });
  elements["accounts-body"].replaceChildren(...rows);
  elements["accounts-empty"].hidden = rows.length > 0;
}

function renderCredits(credits, calls) {
  if (!Array.isArray(credits) || credits.length === 0) return [textElement("p", translate("creditsNotReported"), "muted")];
  return credits.map(credit => {
    const item = textElement("div", "", "credit-item");
    item.append(textElement("strong", translate("creditAmount", { amount: formatOptionalNumber(credit.amount, true), unit: credit.unit || translate("unknown") }), "usage-value"));
    item.append(textElement("div", translate("creditsCoverage", { reported: formatOptionalNumber(credit.reported_calls), calls: formatOptionalNumber(calls) }), "cell-note"));
    if (typeof calls === "number" && credit.reported_calls < calls) item.append(textElement("div", translate("partialCredits"), "cell-note quota-warning"));
    return item;
  });
}

function renderQuota(quota) {
  const cell = textElement("td", "", "quota-cell");
  const statusKeys = { available: "quotaAvailable", stale: "quotaStale", error: "quotaError", unsupported: "quotaUnsupported", unknown: "unknown" };
  const status = quota?.status || "unknown";
  const badgeKind = status === "available" ? "" : status === "stale" ? " warning" : status === "error" ? " error" : " neutral";
  cell.append(textElement("span", translate(statusKeys[status] || "unknown"), `badge${badgeKind}`));
  for (const [field, title] of [["monthly", "monthlyQuota"], ["overage_monthly", "overageQuota"]]) {
    const values = quota?.[field];
    if (field === "overage_monthly" && !values) continue;
    const section = textElement("div", "", "quota-section");
    section.append(textElement("strong", translate(title)));
    if (!values || quota?.unit !== "minutes") {
      section.append(textElement("div", translate("unknown"), "cell-note"));
    } else {
      const detail = textElement("div", "", "quota-values");
      for (const [name, label] of [["used", "quotaUsed"], ["limit", "quotaLimit"], ["remaining", "quotaRemaining"]]) {
        const value = typeof values[name] === "number" && Number.isFinite(values[name]) ? translate("minutesValue", { value: formatOptionalNumber(values[name], true) }) : translate("unknown");
        detail.append(textElement("span", translate(label, { value })));
      }
      section.append(detail);
      if (values.resets_at) section.append(textElement("div", translate("quotaResets", { time: formatTime(values.resets_at) }), "cell-note"));
      if (values.warning) section.append(textElement("div", values.warning, "cell-note quota-warning"));
    }
    cell.append(section);
  }
  if (quota?.subscription_type) cell.append(textElement("div", translate("quotaSubscription", { type: quota.subscription_type }), "cell-note"));
  cell.append(textElement("div", translate("quotaSynced", { time: formatTime(quota?.synced_at) }), "cell-note"));
  if (quota?.last_attempt_at) cell.append(textElement("div", translate("quotaAttempted", { time: formatTime(quota.last_attempt_at) }), "cell-note"));
  if (quota?.error_code) cell.append(textElement("div", translate("quotaErrorCode", { code: quota.error_code }), "cell-note quota-warning"));
  return cell;
}

function renderConfig(data) {
  state.config = data;
  elements["config-json"].value = JSON.stringify(data.settings || {}, null, 2);
  renderConfigMetadata(data);
}

function renderConfigMetadata(data) {
  setTranslation(elements["api-key-count"], "keyCount", { count: data.api_keys_count || 0 });
  elements["config-override"].hidden = !data.overrides?.addr;
  elements["restart-notice"].hidden = !data.restart_required;
}

async function loadConfig() {
  const generation = state.generation;
  const data = await requestJSON("/api/config");
  if (!state.authenticated || generation !== state.generation) return;
  renderConfig(data);
}

async function loadWorkspace() {
  await Promise.all([loadStatus(), loadAccounts(), loadConfig()]);
}

function renderLogin(flow) {
  state.login = flow;
  elements["oauth-flow"].hidden = false;
  const stages = {
    discovery: "flowDiscovery",
    authorize: "flowAuthorize",
    complete: "flowComplete",
    expired: "flowExpired",
    cancelled: "flowCancelled"
  };
  setTranslation(elements["oauth-flow-title"], stages[flow.stage] || "flowUpdating");
  setRawText(elements["oauth-flow-expires"], flow.expires_at ? translate("flowExpires", { time: formatTime(flow.expires_at) }) : "");
  const active = flow.stage === "discovery" || flow.stage === "authorize";
  let safeURL = "";
  if (active && flow.url) {
    try {
      const parsed = new URL(flow.url);
      if (parsed.protocol === "https:") safeURL = parsed.href;
    } catch {
      safeURL = "";
    }
  }
  elements["oauth-link-group"].hidden = !safeURL;
  if (safeURL) elements["oauth-link"].href = safeURL;
  else elements["oauth-link"].removeAttribute("href");
  setTranslation(elements["oauth-link"], flow.stage === "authorize" ? "openOfficialAuthorization" : "openOfficialSignIn");
  elements["oauth-callback-form"].hidden = !active;
  elements["cancel-login"].hidden = !active;
  elements["check-login"].hidden = !active;
  elements["oauth-step-discovery"].className = flow.stage === "discovery" ? "current" : flow.stage === "authorize" || flow.stage === "complete" ? "complete" : "";
  elements["oauth-step-authorize"].className = flow.stage === "authorize" ? "current" : flow.stage === "complete" ? "complete" : "";
  elements["oauth-step-complete"].className = flow.stage === "authorize" ? "current" : flow.stage === "complete" ? "complete" : "";
}

function loginMatches(revision, id) {
  return state.authenticated && state.loginRevision === revision && state.login?.id === id;
}

async function updateLoginFlow(flow, revision, id) {
  if (!loginMatches(revision, id)) return;
  if (state.login.stage === "authorize" && flow.stage === "discovery") return;
  if (["complete", "expired", "cancelled"].includes(state.login.stage) && ["discovery", "authorize"].includes(flow.stage)) return;
  renderLogin(flow);
  if (flow.stage === "complete") {
    elements["oauth-callback"].value = "";
    await Promise.all([loadAccounts(), loadStatus()]);
    if (loginMatches(revision, id)) showMessage("signInSuccess", "success");
  }
}

elements["login-form"].addEventListener("submit", async event => {
  event.preventDefault();
  const generation = state.generation;
  const password = elements["login-password"].value;
  elements["login-password"].value = "";
  elements["login-submit"].disabled = true;
  setRawText(elements["login-error"], "");
  try {
    const session = await requestJSON("/api/auth/login", { method: "POST", body: { password } });
    if (generation !== state.generation) return;
    if (!session.authenticated) throw localizedError("signInIncomplete");
    state.configured = Boolean(session.configured);
    enterApp(session);
    await loadWorkspace();
  } catch (error) {
    if (error.name !== "AbortError" && generation === state.generation) {
      if (state.authenticated) reportError(error);
      else showErrorAt(elements["login-error"], error);
    }
  } finally {
    if (generation === state.generation) elements["login-submit"].disabled = !state.configured;
  }
});

elements["login-retry"].addEventListener("click", checkSession);
elements["theme-toggle"].addEventListener("click", () => {
  const dark = document.body.dataset.theme !== "dark";
  setTheme(dark);
  savePreference("theme", dark ? "dark" : "light");
});
for (const select of document.querySelectorAll("[data-language-select]")) select.addEventListener("change", () => {
  setLanguage(select.value);
  savePreference("language", language);
});
for (const button of document.querySelectorAll("[data-page], [data-open-page]")) button.addEventListener("click", () => openPage(button.dataset.page || button.dataset.openPage));

elements["logout"].addEventListener("click", async () => {
  elements["logout"].disabled = true;
  const csrf = state.csrf;
  clearSession();
  elements["login-password"].disabled = true;
  elements["login-submit"].disabled = true;
  setTranslation(elements["login-notice"], "signingOut");
  try {
    await requestJSON("/api/auth/logout", { method: "POST", body: {}, headers: { "X-CSRF-Token": csrf } });
    setRawText(elements["login-error"], "");
  } catch (error) {
    if (error.name === "AbortError") setRawText(elements["login-error"], "");
    else setTranslation(elements["login-error"], "signOutUnconfirmed");
  } finally {
    updateLoginAvailability();
    elements["logout"].disabled = false;
  }
});

elements["refresh-page"].addEventListener("click", async () => {
  const button = elements["refresh-page"];
  if (state.page === "config" && state.config && elements["config-json"].value !== JSON.stringify(state.config.settings || {}, null, 2) && !window.confirm(translate("confirmDiscard"))) return;
  button.disabled = true;
  try {
    if (state.page === "accounts") await Promise.all([loadAccounts(), loadStatus()]);
    else if (state.page === "config") await loadConfig();
    else await loadStatus();
    showMessage("pageUpdated", "success");
  } catch (error) {
    reportError(error);
  } finally {
    button.disabled = false;
  }
});

elements["refresh-all-accounts"].addEventListener("click", async () => {
  const button = elements["refresh-all-accounts"];
  button.disabled = true;
  try {
    const data = await requestJSON("/api/accounts/refresh", { method: "POST", body: {} });
    await Promise.all([loadAccounts(), loadStatus()]);
    const results = data.results || [];
    const successful = results.filter(result => result.ok).length;
    showMessage("tokensRefreshed", successful === results.length ? "success" : "warning", { successful, failed: results.length - successful });
  } catch (error) {
    reportError(error);
  } finally {
    button.disabled = false;
  }
});

elements["sync-all-usage"].addEventListener("click", async () => {
  const generation = state.generation;
  const button = elements["sync-all-usage"];
  button.disabled = true;
  try {
    const data = await requestJSON("/api/accounts/usage/refresh", { method: "POST", body: {} });
    if (!state.authenticated || generation !== state.generation) return;
    await Promise.all([loadAccounts(), loadStatus()]);
    if (!state.authenticated || generation !== state.generation) return;
    const results = data.results || [];
    const successful = results.filter(result => result.ok).length;
    showMessage("quotasSynced", successful === results.length ? "success" : "warning", { successful, failed: results.length - successful });
  } catch (error) {
    if (state.authenticated && generation === state.generation) {
      try {
        await Promise.all([loadAccounts(), loadStatus()]);
      } catch (reloadError) {
        if (reloadError.name === "AbortError") return;
      }
      if (state.authenticated && generation === state.generation) reportError(error);
    }
  } finally {
    button.disabled = false;
  }
});

elements["upstream-login-form"].addEventListener("submit", async event => {
  event.preventDefault();
  const previousFlow = state.login;
  const activePreviousFlow = previousFlow && ["discovery", "authorize"].includes(previousFlow.stage);
  if (activePreviousFlow && !window.confirm(translate("confirmRestartSignIn"))) return;
  const revision = ++state.loginRevision;
  const button = elements["start-login"];
  button.disabled = true;
  state.login = null;
  elements["oauth-flow"].hidden = true;
  elements["oauth-link"].removeAttribute("href");
  elements["oauth-callback"].value = "";
  let previousRemoved = !activePreviousFlow;
  try {
    if (activePreviousFlow) {
      try {
        await requestJSON(`/api/logins/${encodeURIComponent(previousFlow.id)}`, { method: "DELETE" });
      } catch (error) {
        if (error.status !== 404) throw error;
      }
      previousRemoved = true;
    }
    if (!state.authenticated || revision !== state.loginRevision) return;
    const flow = await requestJSON("/api/logins", {
      method: "POST",
      body: { provider: elements["oauth-provider"].value, stage: elements["oauth-stage"].value, region: elements["oauth-region"].value.trim() }
    });
    if (!state.authenticated || revision !== state.loginRevision) return;
    for (const id of ["submit-callback", "check-login", "cancel-login"]) elements[id].disabled = false;
    renderLogin(flow);
    showNotice("");
  } catch (error) {
    if (state.authenticated && revision === state.loginRevision) {
      if (!previousRemoved && previousFlow) {
        for (const id of ["submit-callback", "check-login", "cancel-login"]) elements[id].disabled = false;
        renderLogin(previousFlow);
      }
      reportError(error);
    }
  } finally {
    if (revision === state.loginRevision) button.disabled = false;
  }
});

elements["oauth-callback-form"].addEventListener("submit", async event => {
  event.preventDefault();
  if (!state.login) return;
  const revision = state.loginRevision;
  const id = state.login.id;
  const callback = elements["oauth-callback"].value.trim();
  elements["oauth-callback"].value = "";
  const button = elements["submit-callback"];
  button.disabled = true;
  try {
    const flow = await requestJSON(`/api/logins/${encodeURIComponent(id)}/callback`, { method: "POST", body: { url: callback } });
    await updateLoginFlow(flow, revision, id);
  } catch (error) {
    if (loginMatches(revision, id)) reportError(error);
  } finally {
    if (loginMatches(revision, id)) button.disabled = false;
  }
});

elements["check-login"].addEventListener("click", async () => {
  if (!state.login) return;
  const revision = state.loginRevision;
  const id = state.login.id;
  const button = elements["check-login"];
  button.disabled = true;
  try {
    await updateLoginFlow(await requestJSON(`/api/logins/${encodeURIComponent(id)}`), revision, id);
  } catch (error) {
    if (loginMatches(revision, id)) reportError(error);
  } finally {
    if (loginMatches(revision, id)) button.disabled = false;
  }
});

elements["cancel-login"].addEventListener("click", async () => {
  if (!state.login) return;
  const flow = state.login;
  const revision = ++state.loginRevision;
  const button = elements["cancel-login"];
  button.disabled = true;
  state.login = null;
  elements["oauth-flow"].hidden = true;
  elements["oauth-link"].removeAttribute("href");
  elements["oauth-callback"].value = "";
  try {
    let cancelledFlow;
    try {
      cancelledFlow = await requestJSON(`/api/logins/${encodeURIComponent(flow.id)}`, { method: "DELETE" });
    } catch (error) {
      if (error.status !== 404) throw error;
      cancelledFlow = { ...flow, stage: "cancelled", url: "" };
    }
    if (!state.authenticated || revision !== state.loginRevision) return;
    renderLogin(cancelledFlow);
    if (cancelledFlow.stage === "complete") {
      await Promise.all([loadAccounts(), loadStatus()]);
      if (loginMatches(revision, flow.id)) showMessage("signInSuccess", "success");
    } else if (cancelledFlow.stage === "cancelled") {
      showMessage("signInCancelled", "success");
    }
  } catch (error) {
    if (state.authenticated && revision === state.loginRevision) {
      for (const id of ["submit-callback", "check-login"]) elements[id].disabled = false;
      renderLogin(state.login || flow);
      reportError(error);
    }
  } finally {
    if (revision === state.loginRevision) button.disabled = false;
  }
});

elements["copy-oauth-link"].addEventListener("click", async () => {
  const link = elements["oauth-link"].getAttribute("href");
  if (!link) return;
  try {
    if (!navigator.clipboard?.writeText) throw new Error("clipboard_unavailable");
    await navigator.clipboard.writeText(link);
    showMessage("linkCopied", "success");
  } catch {
    showMessage("copyUnavailable", "warning");
  }
});

elements["reload-config"].addEventListener("click", async () => {
  if (!window.confirm(translate("confirmDiscard"))) return;
  const button = elements["reload-config"];
  button.disabled = true;
  try {
    await loadConfig();
    elements["replacement-api-keys"].value = "";
    elements["clear-api-keys"].checked = false;
    elements["replacement-api-keys"].disabled = false;
    showMessage("settingsReloaded", "success");
  } catch (error) {
    reportError(error);
  } finally {
    button.disabled = false;
  }
});

elements["clear-api-keys"].addEventListener("change", () => {
  elements["replacement-api-keys"].disabled = elements["clear-api-keys"].checked;
  if (elements["clear-api-keys"].checked) elements["replacement-api-keys"].value = "";
});

elements["config-form"].addEventListener("submit", async event => {
  event.preventDefault();
  const generation = state.generation;
  const button = elements["save-config"];
  let settings;
  try {
    settings = JSON.parse(elements["config-json"].value);
    if (settings === null || typeof settings !== "object" || Array.isArray(settings)) throw new Error(translate("jsonObjectRequired"));
  } catch {
    showMessage("invalidSettings", "error");
    elements["config-json"].focus();
    return;
  }
  const warnings = [translate("confirmSave")];
  if (settings.webui?.enabled === false) warnings.push(translate("warnDisableWebUI"));
  if (elements["clear-api-keys"].checked) warnings.push(translate("warnClearKeys"));
  if (!window.confirm(warnings.join("\n\n"))) return;
  const body = { settings };
  const replacement = elements["replacement-api-keys"].value.split(/\r?\n/).map(value => value.trim()).filter(Boolean);
  if (elements["clear-api-keys"].checked) body.api_keys = [];
  else if (replacement.length) body.api_keys = replacement;
  elements["replacement-api-keys"].value = "";
  button.disabled = true;
  try {
    const data = await requestJSON("/api/config", { method: "PUT", body });
    if (!state.authenticated || generation !== state.generation) return;
    renderConfig(data);
    elements["clear-api-keys"].checked = false;
    elements["replacement-api-keys"].disabled = false;
    showMessage("settingsSaved", "success");
  } catch (error) {
    reportError(error);
  } finally {
    button.disabled = false;
  }
});

elements["playground-form"].addEventListener("submit", async event => {
  event.preventDefault();
  if (state.playgroundController) return;
  const prompt = elements["playground-prompt"].value.trim();
  if (!prompt) return;
  const controller = new AbortController();
  const generation = state.generation;
  state.playgroundController = controller;
  elements["send-playground"].disabled = true;
  elements["cancel-playground"].disabled = false;
  setRawText(elements["playground-output"], "");
  setTranslation(elements["playground-state"], "waitingResponse");
  const headers = {};
  const apiKey = elements["playground-api-key"].value.trim();
  if (apiKey) headers.Authorization = `Bearer ${apiKey}`;
  try {
    const data = await requestJSON("/v1/chat/completions", {
      method: "POST",
      management: false,
      controller,
      headers,
      body: { model: elements["playground-model"].value, messages: [{ role: "user", content: prompt }], stream: false }
    });
    if (!state.authenticated || generation !== state.generation) return;
    const content = data.choices?.[0]?.message?.content;
    if (typeof content === "string") setRawText(elements["playground-output"], content);
    else setTranslation(elements["playground-output"], "noResponseText");
    setTranslation(elements["playground-state"], "requestComplete");
  } catch (error) {
    if (generation !== state.generation) return;
    setTranslation(elements["playground-state"], error.name === "AbortError" ? "requestCancelled" : "requestFailed");
    if (error.name !== "AbortError") showErrorAt(elements["playground-output"], error);
  } finally {
    if (state.playgroundController === controller) {
      state.playgroundController = null;
      elements["send-playground"].disabled = false;
      elements["cancel-playground"].disabled = true;
    }
  }
});

elements["cancel-playground"].addEventListener("click", () => state.playgroundController?.abort());
elements["clear-playground"].addEventListener("click", () => {
  setRawText(elements["playground-output"], "");
  if (!state.playgroundController) setTranslation(elements["playground-state"], "noRequest");
});

const savedTheme = readPreference("theme");
setTheme(savedTheme === "dark" || (savedTheme !== "light" && Boolean(initialDark)));
document.addEventListener("invalid", event => {
  const field = event.target;
  if (field.validity.valueMissing) field.setCustomValidity(translate("fieldRequired"));
  else if (field.validity.typeMismatch && field.type === "url") field.setCustomValidity(translate("validURLRequired"));
}, true);
document.addEventListener("input", event => {
  if (typeof event.target.setCustomValidity === "function") event.target.setCustomValidity("");
});
document.addEventListener("change", event => {
  if (typeof event.target.setCustomValidity === "function") event.target.setCustomValidity("");
});
setLanguage(language);
checkSession();
