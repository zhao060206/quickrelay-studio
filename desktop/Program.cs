using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Drawing;
using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Reflection;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text;
using System.Text.RegularExpressions;
using System.Threading;
using System.Threading.Tasks;
using System.Web.Script.Serialization;
using System.Windows.Forms;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

namespace QuickRelayStudioDesktop
{
    internal static class Program
    {
        // 使用完全独立的单实例互斥量，与原版 AWS-Quick-Proxy 互不干扰、支持并存运行
        private const string SingleInstanceMutexName = "Local\\QuickRelayStudio_SingleInstance";
        private const string ShowWindowEventName = "Local\\QuickRelayStudio_ShowWindow";

        [DllImport("user32.dll", SetLastError = true)]
        private static extern bool SetProcessDpiAwarenessContext(IntPtr dpiContext);

        [DllImport("user32.dll")]
        private static extern bool SetProcessDPIAware();

        private static void EnableHighDpi()
        {
            try
            {
                if (SetProcessDpiAwarenessContext(new IntPtr(-4)))
                {
                    return;
                }
            }
            catch
            {
            }

            try
            {
                SetProcessDPIAware();
            }
            catch
            {
            }
        }

        [STAThread]
        private static void Main()
        {
            EnableHighDpi();

            bool createdNew;
            using (Mutex mutex = new Mutex(true, SingleInstanceMutexName, out createdNew))
            {
                if (!createdNew)
                {
                    try
                    {
                        using (EventWaitHandle showEvent = EventWaitHandle.OpenExisting(ShowWindowEventName))
                        {
                            showEvent.Set();
                        }
                    }
                    catch
                    {
                    }
                    return;
                }

                using (EventWaitHandle showEvent = new EventWaitHandle(false, EventResetMode.AutoReset, ShowWindowEventName))
                {
                    Application.EnableVisualStyles();
                    Application.SetCompatibleTextRenderingDefault(false);

                    MainForm mainForm = new MainForm(showEvent);
                    Application.Run(mainForm);
                }
            }
        }
    }

    internal sealed class AccountViewDto
    {
        public long id;
        public string displayText;
        public string displayName;
        public string provider;
        public bool disabled;
        public bool hasError;
        public long expiresAt;
        public long lastUsedAt;
        public long callCount;
        public long successCount;
        public long failureCount;
        public long totalTokens;
        public long avgDurationMs;
        public double costUsd;
        public string creditsText;
        public string statusText;
        public string statusKind;

        public string quotaStatus;
        public string quotaUnit;
        public string subscriptionType;
        public bool hasQuota;
        public double quotaUsed;
        public double quotaLimit;
        public double quotaRemaining;
        public long quotaResetsAt;
        public long quotaSyncedAt;
    }

    /// <summary>
    /// 封装对本地 quickrelay 管理 API 的认证请求、原始 JSON 透传及 CSV 导出。
    /// </summary>
    internal sealed class AdminApiClient
    {
        private readonly JavaScriptSerializer _json;
        private CookieContainer _cookies = new CookieContainer();
        private string _csrfToken = string.Empty;
        private string _baseUrl = "http://127.0.0.1:8080";
        private string _password = string.Empty;

        public AdminApiClient()
        {
            _json = new JavaScriptSerializer();
            _json.MaxJsonLength = 16 * 1024 * 1024;
        }

        public void Configure(string baseUrl, string password)
        {
            _baseUrl = (baseUrl ?? "http://127.0.0.1:8080").TrimEnd('/');
            _password = password ?? string.Empty;
            _cookies = new CookieContainer();
            _csrfToken = string.Empty;
        }

        public string ProbeHealthServiceSync(int timeoutMs)
        {
            try
            {
                HttpWebRequest req = (HttpWebRequest)WebRequest.Create(_baseUrl + "/healthz");
                req.Method = "GET";
                req.Proxy = null;
                req.Timeout = timeoutMs;
                req.ReadWriteTimeout = timeoutMs;
                using (HttpWebResponse resp = (HttpWebResponse)req.GetResponse())
                using (StreamReader reader = new StreamReader(resp.GetResponseStream(), Encoding.UTF8))
                {
                    string body = reader.ReadToEnd();
                    Dictionary<string, object> root = _json.Deserialize<Dictionary<string, object>>(body);
                    return GetString(root, "service");
                }
            }
            catch
            {
                return string.Empty;
            }
        }

        public async Task<string> GetRawJsonAsync(string path)
        {
            return await SendAuthorizedAsync("GET", path, null);
        }

        public async Task<string> PostRawJsonAsync(string path, string jsonBody)
        {
            return await SendAuthorizedAsync("POST", path, jsonBody ?? "{}");
        }

        public async Task<string> PutRawJsonAsync(string path, string jsonBody)
        {
            return await SendAuthorizedAsync("PUT", path, jsonBody ?? "{}");
        }

        public async Task<string> DeleteRawJsonAsync(string path)
        {
            return await SendAuthorizedAsync("DELETE", path, null);
        }

        public async Task<byte[]> DownloadAuthorizedBytesAsync(string path)
        {
            await EnsureAuthenticatedAsync();
            byte[] data = await DownloadRawBytesAsync(path);
            return data;
        }

        public async Task<List<AccountViewDto>> ListAccountsAsync()
        {
            string body = await SendAuthorizedAsync("GET", "/api/accounts", null);
            Dictionary<string, object> root = _json.Deserialize<Dictionary<string, object>>(body);
            List<AccountViewDto> result = new List<AccountViewDto>();
            if (root == null || !root.ContainsKey("accounts") || root["accounts"] == null)
            {
                return result;
            }

            System.Collections.IEnumerable items = root["accounts"] as System.Collections.IEnumerable;
            if (items == null)
            {
                return result;
            }

            long nowUnix = (long)(DateTime.UtcNow - new DateTime(1970, 1, 1, 0, 0, 0, DateTimeKind.Utc)).TotalSeconds;
            foreach (object obj in items)
            {
                Dictionary<string, object> row = obj as Dictionary<string, object>;
                if (row == null)
                {
                    continue;
                }

                AccountViewDto item = new AccountViewDto();
                item.id = GetLong(row, "id");
                string email = GetString(row, "email");
                string displayName = GetString(row, "display_name");
                item.displayName = displayName;
                if (!string.IsNullOrEmpty(email))
                {
                    item.displayText = email;
                }
                else if (!string.IsNullOrEmpty(displayName))
                {
                    item.displayText = displayName;
                }
                else
                {
                    item.displayText = string.Format("账号 #{0}", item.id);
                }

                item.provider = GetString(row, "provider");
                item.disabled = GetBool(row, "disabled");
                item.hasError = GetBool(row, "has_error");
                item.expiresAt = GetLong(row, "expires_at");
                item.lastUsedAt = GetLong(row, "last_used_at");

                long callCount = GetLong(row, "use_count");
                if (row.ContainsKey("usage") && row["usage"] is Dictionary<string, object>)
                {
                    Dictionary<string, object> usage = (Dictionary<string, object>)row["usage"];
                    long realCalls = GetLong(usage, "call_count");
                    if (realCalls > 0)
                    {
                        callCount = realCalls;
                    }
                    item.successCount = GetLong(usage, "success_count");
                    item.failureCount = GetLong(usage, "failure_count");
                    item.totalTokens = GetLong(usage, "total_tokens");
                    item.costUsd = GetDouble(usage, "cost_usd");
                    long durMs = GetLong(usage, "total_duration_ms");
                    if (callCount > 0 && durMs > 0)
                    {
                        item.avgDurationMs = durMs / callCount;
                    }
                    if (usage.ContainsKey("credits") && usage["credits"] is System.Collections.IEnumerable)
                    {
                        List<string> creditParts = new List<string>();
                        foreach (object cObj in (System.Collections.IEnumerable)usage["credits"])
                        {
                            Dictionary<string, object> creditRow = cObj as Dictionary<string, object>;
                            if (creditRow == null)
                            {
                                continue;
                            }
                            double amt = GetDouble(creditRow, "amount");
                            string unit = GetString(creditRow, "unit");
                            if (amt > 0)
                            {
                                string formattedAmt = amt.ToString("0.##", System.Globalization.CultureInfo.InvariantCulture);
                                creditParts.Add(string.IsNullOrEmpty(unit) ? formattedAmt : string.Format("{0} {1}", formattedAmt, unit));
                            }
                        }
                        if (creditParts.Count > 0)
                        {
                            item.creditsText = string.Join(" · ", creditParts.ToArray());
                        }
                    }
                }
                item.callCount = callCount;

                item.quotaStatus = "unknown";
                item.quotaUnit = string.Empty;
                item.subscriptionType = string.Empty;
                if (row.ContainsKey("quota") && row["quota"] is Dictionary<string, object>)
                {
                    Dictionary<string, object> quota = (Dictionary<string, object>)row["quota"];
                    string qStatus = GetString(quota, "status");
                    if (!string.IsNullOrEmpty(qStatus))
                    {
                        item.quotaStatus = qStatus;
                    }
                    item.quotaUnit = GetString(quota, "unit");
                    item.subscriptionType = GetString(quota, "subscription_type");
                    item.quotaSyncedAt = GetLong(quota, "synced_at");

                    if (quota.ContainsKey("monthly") && quota["monthly"] is Dictionary<string, object>)
                    {
                        Dictionary<string, object> monthly = (Dictionary<string, object>)quota["monthly"];
                        double limitVal = GetDouble(monthly, "limit");
                        if (limitVal > 0)
                        {
                            item.hasQuota = true;
                            item.quotaUsed = GetDouble(monthly, "used");
                            item.quotaLimit = limitVal;
                            if (monthly.ContainsKey("remaining") && monthly["remaining"] != null)
                            {
                                item.quotaRemaining = GetDouble(monthly, "remaining");
                            }
                            else
                            {
                                item.quotaRemaining = Math.Max(0, limitVal - item.quotaUsed);
                            }
                            item.quotaResetsAt = GetLong(monthly, "resets_at");
                        }
                    }
                }

                if (item.disabled)
                {
                    item.statusText = "已停用";
                    item.statusKind = "disabled";
                }
                else if (item.hasError)
                {
                    item.statusText = "存在错误";
                    item.statusKind = "error";
                }
                else if (item.expiresAt > 0 && item.expiresAt <= nowUnix)
                {
                    item.statusText = "Token 过期";
                    item.statusKind = "expired";
                }
                else
                {
                    item.statusText = "启用中";
                    item.statusKind = "enabled";
                }

                result.Add(item);
            }
            return result;
        }

        public async Task RefreshOneTokenAsync(long id)
        {
            await SendAuthorizedAsync("POST", string.Format("/api/accounts/{0}/refresh", id), null);
        }

        public async Task SyncOneUsageAsync(long id)
        {
            await SendAuthorizedAsync("POST", string.Format("/api/accounts/{0}/usage/refresh", id), "{}");
        }

        public async Task<string> SyncAllUsageAsync()
        {
            string body = await SendAuthorizedAsync("POST", "/api/accounts/usage/refresh", "{}");
            Dictionary<string, object> root = _json.Deserialize<Dictionary<string, object>>(body);
            int okCount = 0;
            int failCount = 0;
            if (root != null && root.ContainsKey("results"))
            {
                System.Collections.IEnumerable items = root["results"] as System.Collections.IEnumerable;
                if (items != null)
                {
                    foreach (object obj in items)
                    {
                        Dictionary<string, object> row = obj as Dictionary<string, object>;
                        if (row != null && GetBool(row, "ok"))
                        {
                            okCount++;
                        }
                        else
                        {
                            failCount++;
                        }
                    }
                }
            }
            return string.Format("余额同步完成：{0} 个成功，{1} 个失败", okCount, failCount);
        }

        public async Task<string> RefreshAllTokensAsync()
        {
            string body = await SendAuthorizedAsync("POST", "/api/accounts/refresh", "{}");
            Dictionary<string, object> root = _json.Deserialize<Dictionary<string, object>>(body);
            int okCount = 0;
            int failCount = 0;
            if (root != null && root.ContainsKey("results"))
            {
                System.Collections.IEnumerable items = root["results"] as System.Collections.IEnumerable;
                if (items != null)
                {
                    foreach (object obj in items)
                    {
                        Dictionary<string, object> row = obj as Dictionary<string, object>;
                        if (row != null && GetBool(row, "ok"))
                        {
                            okCount++;
                        }
                        else
                        {
                            failCount++;
                        }
                    }
                }
            }
            return string.Format("刷新完成：{0} 个成功，{1} 个失败", okCount, failCount);
        }

        public async Task SetAccountDisabledAsync(long id, bool disabled)
        {
            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["disabled"] = disabled;
            await SendAuthorizedAsync("PATCH", string.Format("/api/accounts/{0}", id), _json.Serialize(payload));
        }

        public async Task DeleteAccountAsync(long id)
        {
            await SendAuthorizedAsync("DELETE", string.Format("/api/accounts/{0}", id), null);
        }

        private async Task EnsureAuthenticatedAsync()
        {
            if (!string.IsNullOrEmpty(_csrfToken))
            {
                return;
            }
            await LoginAsync();
        }

        private async Task LoginAsync()
        {
            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["password"] = _password;
            HttpResponseResult res = await SendRawAsync("POST", "/api/auth/login", _json.Serialize(payload), false);
            if (res.StatusCode < 200 || res.StatusCode >= 300)
            {
                throw new InvalidOperationException(ExtractApiErrorMessage(res.Body, "管理接口自动认证失败（HTTP " + res.StatusCode + "）"));
            }

            Dictionary<string, object> data = _json.Deserialize<Dictionary<string, object>>(res.Body);
            if (data == null || !GetBool(data, "authenticated") || !data.ContainsKey("csrf_token"))
            {
                throw new InvalidOperationException("管理接口未返回有效会话令牌");
            }
            _csrfToken = data["csrf_token"] != null ? data["csrf_token"].ToString() : string.Empty;
        }

        private async Task<string> SendAuthorizedAsync(string method, string path, string jsonBody)
        {
            await EnsureAuthenticatedAsync();
            HttpResponseResult res = await SendRawAsync(method, path, jsonBody, true);
            if (res.StatusCode == 401 || res.StatusCode == 403)
            {
                _csrfToken = string.Empty;
                _cookies = new CookieContainer();
                await LoginAsync();
                res = await SendRawAsync(method, path, jsonBody, true);
            }

            if (res.StatusCode < 200 || res.StatusCode >= 300)
            {
                throw new InvalidOperationException(ExtractApiErrorMessage(res.Body, string.Format("请求失败（HTTP {0}）", res.StatusCode)));
            }
            return res.Body;
        }

        private Task<byte[]> DownloadRawBytesAsync(string path)
        {
            return Task.Run(delegate
            {
                HttpWebRequest req = (HttpWebRequest)WebRequest.Create(_baseUrl + path);
                req.Method = "GET";
                req.Proxy = null;
                req.CookieContainer = _cookies;
                req.Timeout = 45000;
                req.ReadWriteTimeout = 45000;
                using (HttpWebResponse resp = (HttpWebResponse)req.GetResponse())
                using (Stream stream = resp.GetResponseStream())
                using (MemoryStream ms = new MemoryStream())
                {
                    if (stream != null)
                    {
                        stream.CopyTo(ms);
                    }
                    return ms.ToArray();
                }
            });
        }

        private Task<HttpResponseResult> SendRawAsync(string method, string path, string jsonBody, bool includeCsrf)
        {
            return Task.Run(delegate
            {
                HttpWebRequest req = (HttpWebRequest)WebRequest.Create(_baseUrl + path);
                req.Method = method;
                req.Proxy = null;
                req.CookieContainer = _cookies;
                req.Accept = "application/json";
                req.Timeout = 45000;
                req.ReadWriteTimeout = 45000;

                if (includeCsrf && !string.IsNullOrEmpty(_csrfToken) &&
                    !string.Equals(method, "GET", StringComparison.OrdinalIgnoreCase) &&
                    !string.Equals(method, "HEAD", StringComparison.OrdinalIgnoreCase))
                {
                    req.Headers["X-CSRF-Token"] = _csrfToken;
                }

                if (jsonBody != null)
                {
                    byte[] bytes = Encoding.UTF8.GetBytes(jsonBody);
                    req.ContentType = "application/json; charset=utf-8";
                    req.ContentLength = bytes.Length;
                    using (Stream stream = req.GetRequestStream())
                    {
                        stream.Write(bytes, 0, bytes.Length);
                    }
                }
                else if (string.Equals(method, "POST", StringComparison.OrdinalIgnoreCase) ||
                         string.Equals(method, "PATCH", StringComparison.OrdinalIgnoreCase) ||
                         string.Equals(method, "PUT", StringComparison.OrdinalIgnoreCase))
                {
                    req.ContentType = "application/json; charset=utf-8";
                    req.ContentLength = 0;
                }

                try
                {
                    using (HttpWebResponse resp = (HttpWebResponse)req.GetResponse())
                    using (StreamReader reader = new StreamReader(resp.GetResponseStream(), Encoding.UTF8))
                    {
                        return new HttpResponseResult((int)resp.StatusCode, reader.ReadToEnd());
                    }
                }
                catch (WebException ex)
                {
                    HttpWebResponse errResp = ex.Response as HttpWebResponse;
                    if (errResp != null)
                    {
                        using (errResp)
                        using (Stream stream = errResp.GetResponseStream())
                        {
                            string errBody = string.Empty;
                            if (stream != null)
                            {
                                using (StreamReader reader = new StreamReader(stream, Encoding.UTF8))
                                {
                                    errBody = reader.ReadToEnd();
                                }
                            }
                            return new HttpResponseResult((int)errResp.StatusCode, errBody);
                        }
                    }
                    throw new InvalidOperationException("无法连接到本地服务: " + ex.Message, ex);
                }
            });
        }

        private string ExtractApiErrorMessage(string body, string fallback)
        {
            if (string.IsNullOrWhiteSpace(body))
            {
                return fallback;
            }
            try
            {
                Dictionary<string, object> root = _json.Deserialize<Dictionary<string, object>>(body);
                if (root != null && root.ContainsKey("error") && root["error"] is Dictionary<string, object>)
                {
                    Dictionary<string, object> errObj = (Dictionary<string, object>)root["error"];
                    string msg = GetString(errObj, "message");
                    if (!string.IsNullOrEmpty(msg))
                    {
                        return msg;
                    }
                }
            }
            catch
            {
            }
            return fallback;
        }

        private static string GetString(Dictionary<string, object> dict, string key)
        {
            if (dict != null && dict.ContainsKey(key) && dict[key] != null)
            {
                return dict[key].ToString();
            }
            return string.Empty;
        }

        private static long GetLong(Dictionary<string, object> dict, string key)
        {
            if (dict != null && dict.ContainsKey(key) && dict[key] != null)
            {
                long v;
                if (long.TryParse(dict[key].ToString(), out v))
                {
                    return v;
                }
            }
            return 0;
        }

        private static double GetDouble(Dictionary<string, object> dict, string key)
        {
            if (dict != null && dict.ContainsKey(key) && dict[key] != null)
            {
                double d;
                if (double.TryParse(dict[key].ToString(), System.Globalization.NumberStyles.Float, System.Globalization.CultureInfo.InvariantCulture, out d))
                {
                    return d;
                }
            }
            return 0;
        }

        private static bool GetBool(Dictionary<string, object> dict, string key)
        {
            if (dict != null && dict.ContainsKey(key) && dict[key] != null)
            {
                bool b;
                if (bool.TryParse(dict[key].ToString(), out b))
                {
                    return b;
                }
            }
            return false;
        }

        private sealed class HttpResponseResult
        {
            public readonly int StatusCode;
            public readonly string Body;

            public HttpResponseResult(int statusCode, string body)
            {
                StatusCode = statusCode;
                Body = body ?? string.Empty;
            }
        }
    }

    internal sealed class MainForm : Form
    {
        private const int MaxLogLines = 1500;
        private static readonly Regex UrlRegex = new Regex(@"https://\S+", RegexOptions.IgnoreCase | RegexOptions.Compiled);

        [DllImport("dwmapi.dll")]
        private static extern int DwmSetWindowAttribute(IntPtr hwnd, int attr, ref int attrValue, int attrSize);

        private readonly EventWaitHandle _showWindowEvent;
        private readonly CancellationTokenSource _shutdownCts;
        private readonly JavaScriptSerializer _json;
        private readonly object _logLock = new object();
        private readonly LinkedList<string> _logBuffer = new LinkedList<string>();
        private readonly AdminApiClient _apiClient = new AdminApiClient();

        private readonly string _binDir;
        private readonly string _projectDir;
        private readonly string _quickRelayExePath;
        private readonly string _configPath;
        private readonly string _passwordFilePath;

        private string _displayHost = "127.0.0.1";
        private string _probeHost = "127.0.0.1";
        private int _port = 8080;
        private string _webuiUrl = "http://127.0.0.1:8080/";
        private string _apiBaseUrl = "http://127.0.0.1:8080/v1";
        private string _anthropicBaseUrl = "http://127.0.0.1:8080";
        private string[] _models = new string[] { "smart", "fast", "balanced", "auto" };
        private string _defaultModel = "smart";
        private string _primaryApiKey = string.Empty;
        private string _adminPassword = string.Empty;

        private string _serviceState = "starting";
        private string _statusText = "启动中...";

        private Icon _appIcon;
        private NotifyIcon _notifyIcon;
        private WebView2 _webView;
        private bool _webViewReady;
        private readonly string _externalUiPath;
        private readonly object _uiWatchLock = new object();
        private FileSystemWatcher _uiWatcher;
        private System.Threading.Timer _uiReloadDebounce;
        private string _lastActiveTab = "dashboard";
        private string _lastTheme = "dark";

        private Process _backendProcess;
        private readonly object _procLock = new object();
        private bool _managedByUs;
        private bool _isRestarting;

        private Process _loginProcess;
        private readonly object _loginProcLock = new object();
        private bool _loginActive;
        private bool _loginCancelling;
        private string _loginStage = string.Empty;
        private string _currentLoginUrl = string.Empty;
        private string _loginMessage = string.Empty;
        private StringBuilder _loginLogBuffer = new StringBuilder();

        private bool _forceExit;
        private bool _balloonShown;
        private System.Windows.Forms.Timer _autoRefreshTimer;
        private bool _autoRefreshBusy;

        public MainForm(EventWaitHandle showWindowEvent)
        {
            _showWindowEvent = showWindowEvent;
            _shutdownCts = new CancellationTokenSource();
            _json = new JavaScriptSerializer();
            _json.MaxJsonLength = 16 * 1024 * 1024;

            _binDir = AppDomain.CurrentDomain.BaseDirectory.TrimEnd(Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar);
            _projectDir = ResolveProjectDirectory(_binDir);
            _quickRelayExePath = Path.Combine(_binDir, "quickrelay.exe");
            if (!File.Exists(_quickRelayExePath))
            {
                _quickRelayExePath = Path.Combine(_projectDir, "bin", "quickrelay.exe");
            }
            _configPath = Path.Combine(_projectDir, "config.json");
            _passwordFilePath = Path.Combine(_projectDir, "data", "admin-password.txt");
            _externalUiPath = Path.Combine(_projectDir, "ui.html");

            LoadExecutableIcon();
            InitializeFormWindow();
            InitializeTrayIcon();
            StartShowWindowListener();

            this.Load += MainForm_Load;
            this.FormClosing += MainForm_FormClosing;
            Application.ApplicationExit += Application_ApplicationExit;
        }

        protected override void OnHandleCreated(EventArgs e)
        {
            base.OnHandleCreated(e);
            EnableDarkTitleBar();
        }

        private void EnableDarkTitleBar()
        {
            try
            {
                int useDark = 1;
                if (DwmSetWindowAttribute(this.Handle, 20, ref useDark, sizeof(int)) != 0)
                {
                    DwmSetWindowAttribute(this.Handle, 19, ref useDark, sizeof(int));
                }
            }
            catch
            {
            }
        }

        private static string ResolveProjectDirectory(string binDir)
        {
            try
            {
                if (File.Exists(Path.Combine(binDir, "config.json")) || File.Exists(Path.Combine(binDir, "quickrelay.exe")))
                {
                    return binDir;
                }
                string parent = Path.GetFullPath(Path.Combine(binDir, ".."));
                if (File.Exists(Path.Combine(parent, "config.json")))
                {
                    return parent;
                }
                string folderName = Path.GetFileName(binDir);
                if (string.Equals(folderName, "bin", StringComparison.OrdinalIgnoreCase) && Directory.Exists(parent))
                {
                    return parent;
                }
            }
            catch
            {
            }
            return binDir;
        }

        private void LoadExecutableIcon()
        {
            try
            {
                _appIcon = Icon.ExtractAssociatedIcon(Application.ExecutablePath);
                if (_appIcon != null)
                {
                    this.Icon = _appIcon;
                }
            }
            catch
            {
                _appIcon = SystemIcons.Application;
            }
        }

        private float GetPrimaryDpiScale()
        {
            try
            {
                using (Graphics g = Graphics.FromHwnd(IntPtr.Zero))
                {
                    float scale = g.DpiX / 96.0f;
                    return scale > 0.5f ? scale : 1.0f;
                }
            }
            catch
            {
                return 1.0f;
            }
        }

        private void InitializeFormWindow()
        {
            this.Text = "QuickRelay Studio 2.1.0";
            this.AutoScaleMode = AutoScaleMode.None;
            this.WindowState = FormWindowState.Normal;
            this.StartPosition = FormStartPosition.CenterScreen;
            this.BackColor = Color.FromArgb(20, 20, 21);

            float scale = GetPrimaryDpiScale();
            Rectangle workArea = Screen.PrimaryScreen.WorkingArea;
            int maxW = (int)(workArea.Width * 0.92);
            int maxH = (int)(workArea.Height * 0.92);

            int targetW = Math.Min((int)Math.Round(1140 * scale), maxW);
            int targetH = Math.Min((int)Math.Round(760 * scale), maxH);
            int minW = Math.Min((int)Math.Round(880 * scale), maxW);
            int minH = Math.Min((int)Math.Round(600 * scale), maxH);

            this.ClientSize = new Size(targetW, targetH);
            this.MinimumSize = new Size(minW, minH);

            _webView = new WebView2();
            _webView.Dock = DockStyle.Fill;
            _webView.DefaultBackgroundColor = Color.FromArgb(20, 20, 21);
            this.Controls.Add(_webView);
        }

        private void InitializeTrayIcon()
        {
            ContextMenuStrip menu = new ContextMenuStrip();

            ToolStripMenuItem itemShow = new ToolStripMenuItem("显示 QuickRelay Studio");
            itemShow.Font = new Font(menu.Font, FontStyle.Bold);
            itemShow.Click += delegate { RestoreFromTray(); };

            ToolStripMenuItem itemLogin = new ToolStripMenuItem("一键登录账号");
            itemLogin.Click += delegate
            {
                RestoreFromTray();
                StartOneClickLogin(false, string.Empty);
            };

            ToolStripMenuItem itemCopyOpenAI = new ToolStripMenuItem("复制 OpenAI 接口地址");
            itemCopyOpenAI.Click += delegate { CopyTextToClipboard(_apiBaseUrl, "OpenAI 接口地址已复制"); };

            ToolStripMenuItem itemCopyAnthropic = new ToolStripMenuItem("复制 Claude Code 地址");
            itemCopyAnthropic.Click += delegate { CopyTextToClipboard(_anthropicBaseUrl, "Anthropic 接口地址已复制"); };

            ToolStripMenuItem itemSyncUsage = new ToolStripMenuItem("同步全部账号余额");
            itemSyncUsage.Click += async delegate { await SyncAllUsageAndPushAsync(); };

            ToolStripMenuItem itemRestart = new ToolStripMenuItem("重启反代服务");
            itemRestart.Click += async delegate { await RestartBackendAsync(); };

            ToolStripMenuItem itemDir = new ToolStripMenuItem("打开软件目录");
            itemDir.Click += delegate { OpenProjectDirectory(); };

            ToolStripMenuItem itemExit = new ToolStripMenuItem("彻底退出（停止服务）");
            itemExit.Click += delegate { ExitApplicationCompletely(); };

            menu.Items.Add(itemShow);
            menu.Items.Add(itemLogin);
            menu.Items.Add(new ToolStripSeparator());
            menu.Items.Add(itemCopyOpenAI);
            menu.Items.Add(itemCopyAnthropic);
            menu.Items.Add(itemSyncUsage);
            menu.Items.Add(itemRestart);
            menu.Items.Add(itemDir);
            menu.Items.Add(new ToolStripSeparator());
            menu.Items.Add(itemExit);

            _notifyIcon = new NotifyIcon();
            _notifyIcon.Icon = _appIcon != null ? _appIcon : SystemIcons.Application;
            _notifyIcon.Text = "QuickRelay Studio";
            _notifyIcon.ContextMenuStrip = menu;
            _notifyIcon.Visible = false;
            _notifyIcon.DoubleClick += delegate { RestoreFromTray(); };
        }

        private void StartShowWindowListener()
        {
            Thread listenerThread = new Thread(delegate()
            {
                WaitHandle[] handles = new WaitHandle[] { _showWindowEvent, _shutdownCts.Token.WaitHandle };
                while (!_shutdownCts.IsCancellationRequested)
                {
                    int index = WaitHandle.WaitAny(handles);
                    if (index == 0)
                    {
                        try
                        {
                            if (!this.IsDisposed && this.IsHandleCreated)
                            {
                                this.BeginInvoke((MethodInvoker)delegate
                                {
                                    RestoreFromTray();
                                });
                            }
                        }
                        catch
                        {
                        }
                    }
                    else
                    {
                        break;
                    }
                }
            });
            listenerThread.IsBackground = true;
            listenerThread.Start();
        }

        private async void MainForm_Load(object sender, EventArgs e)
        {
            if (_notifyIcon != null)
            {
                _notifyIcon.Visible = true;
            }

            try
            {
                ReloadRuntimeSettings();
                await InitializeWebViewAsync();
                await EnsureBackendRunningAsync();
                if (_serviceState == "running")
                {
                    await RefreshAccountsAndPushAsync();
                }
                StartAutoRefreshTimer();
            }
            catch (Exception ex)
            {
                AppendLog("[桌面端错误] 初始化失败: " + ex.Message);
                SetServiceStatus("stopped", "启动失败");
                MessageBox.Show(
                    "启动 QuickRelay Studio 失败：\n" + ex.Message,
                    "启动失败",
                    MessageBoxButtons.OK,
                    MessageBoxIcon.Error);
            }
        }

        private async Task InitializeWebViewAsync()
        {
            string userDataFolder = Path.Combine(_projectDir, "data", "webview2");
            if (!Directory.Exists(userDataFolder))
            {
                Directory.CreateDirectory(userDataFolder);
            }

            CoreWebView2Environment env = await CoreWebView2Environment.CreateAsync(null, userDataFolder);
            await _webView.EnsureCoreWebView2Async(env);

            _webView.CoreWebView2.Settings.AreDefaultContextMenusEnabled = false;
            _webView.CoreWebView2.Settings.IsStatusBarEnabled = false;
            _webView.CoreWebView2.Settings.IsZoomControlEnabled = false;

            _webView.CoreWebView2.NewWindowRequested += CoreWebView2_NewWindowRequested;
            _webView.CoreWebView2.NavigationStarting += CoreWebView2_NavigationStarting;
            _webView.CoreWebView2.WebMessageReceived += CoreWebView2_WebMessageReceived;

            _webViewReady = true;
            string html = LoadUiHtml();
            _webView.CoreWebView2.NavigateToString(html);
            StartUiHotReloadWatcher();
        }

        /// <summary>
        /// 优先读取同目录外部 ui.html 以支持无重启热加载，缺失时回退内嵌资源。
        /// </summary>
        private string LoadUiHtml()
        {
            if (!string.IsNullOrEmpty(_externalUiPath) && File.Exists(_externalUiPath))
            {
                for (int i = 0; i < 3; i++)
                {
                    try
                    {
                        using (FileStream fs = new FileStream(_externalUiPath, FileMode.Open, FileAccess.Read, FileShare.ReadWrite))
                        using (StreamReader reader = new StreamReader(fs, Encoding.UTF8))
                        {
                            string diskHtml = reader.ReadToEnd();
                            if (!string.IsNullOrWhiteSpace(diskHtml))
                            {
                                return diskHtml;
                            }
                        }
                    }
                    catch
                    {
                        if (i < 2)
                        {
                            Thread.Sleep(80);
                        }
                    }
                }
            }
            return LoadEmbeddedUiHtml();
        }

        private void StartUiHotReloadWatcher()
        {
            if (string.IsNullOrEmpty(_projectDir) || !Directory.Exists(_projectDir))
            {
                return;
            }
            lock (_uiWatchLock)
            {
                if (_uiWatcher != null)
                {
                    return;
                }
                _uiReloadDebounce = new System.Threading.Timer(delegate
                {
                    ReloadUiHtmlFromDisk(true);
                }, null, Timeout.Infinite, Timeout.Infinite);

                FileSystemWatcher watcher = new FileSystemWatcher(_projectDir, "ui.html");
                watcher.NotifyFilter = NotifyFilters.LastWrite | NotifyFilters.Size | NotifyFilters.FileName | NotifyFilters.CreationTime;
                watcher.Changed += delegate { ScheduleUiHotReload(); };
                watcher.Created += delegate { ScheduleUiHotReload(); };
                watcher.Renamed += delegate { ScheduleUiHotReload(); };
                watcher.EnableRaisingEvents = true;
                _uiWatcher = watcher;
            }
        }

        private void ScheduleUiHotReload()
        {
            if (_shutdownCts.IsCancellationRequested)
            {
                return;
            }
            lock (_uiWatchLock)
            {
                if (_uiReloadDebounce != null)
                {
                    try
                    {
                        _uiReloadDebounce.Change(280, Timeout.Infinite);
                    }
                    catch
                    {
                    }
                }
            }
        }

        private void ReloadUiHtmlFromDisk(bool fromWatcher)
        {
            if (_shutdownCts.IsCancellationRequested || this.IsDisposed || !this.IsHandleCreated)
            {
                return;
            }
            if (this.InvokeRequired)
            {
                try
                {
                    this.BeginInvoke((MethodInvoker)delegate
                    {
                        ReloadUiHtmlFromDisk(fromWatcher);
                    });
                }
                catch
                {
                }
                return;
            }
            if (!_webViewReady || _webView == null || _webView.IsDisposed || _webView.CoreWebView2 == null)
            {
                return;
            }
            try
            {
                string html = LoadUiHtml();
                _webView.CoreWebView2.NavigateToString(html);
                AppendLog(fromWatcher
                    ? "[前端热加载] 检测到 ui.html 变更，已完成无感热重载。"
                    : "[前端热加载] 已手动重载前端页面。");
            }
            catch (Exception ex)
            {
                AppendLog("[前端热加载] 重载失败: " + ex.Message);
            }
        }

        private void PushUiPrefsToWeb()
        {
            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["type"] = "uiPrefs";
            payload["activeTab"] = _lastActiveTab;
            payload["theme"] = _lastTheme;
            payload["hotReload"] = !string.IsNullOrEmpty(_externalUiPath) && File.Exists(_externalUiPath);
            SendToWeb(payload);
        }

        private static string LoadEmbeddedUiHtml()
        {
            Assembly asm = Assembly.GetExecutingAssembly();
            using (Stream stream = asm.GetManifestResourceStream("ui.html"))
            {
                if (stream == null)
                {
                    throw new InvalidOperationException("未找到内嵌界面资源 ui.html");
                }
                using (StreamReader reader = new StreamReader(stream, Encoding.UTF8))
                {
                    return reader.ReadToEnd();
                }
            }
        }

        private static bool IsInternalNavigationUri(string uriString)
        {
            if (string.IsNullOrEmpty(uriString))
            {
                return true;
            }
            return uriString.StartsWith("about:", StringComparison.OrdinalIgnoreCase) ||
                   uriString.StartsWith("data:", StringComparison.OrdinalIgnoreCase) ||
                   uriString.StartsWith("blob:", StringComparison.OrdinalIgnoreCase);
        }

        private void CoreWebView2_NewWindowRequested(object sender, CoreWebView2NewWindowRequestedEventArgs e)
        {
            if (!IsInternalNavigationUri(e.Uri))
            {
                e.Handled = true;
                OpenInDefaultBrowser(e.Uri);
            }
        }

        private void CoreWebView2_NavigationStarting(object sender, CoreWebView2NavigationStartingEventArgs e)
        {
            if (!IsInternalNavigationUri(e.Uri))
            {
                e.Cancel = true;
                OpenInDefaultBrowser(e.Uri);
            }
        }

        private async void CoreWebView2_WebMessageReceived(object sender, CoreWebView2WebMessageReceivedEventArgs e)
        {
            string rawJson = string.Empty;
            try
            {
                rawJson = e.TryGetWebMessageAsString();
            }
            catch
            {
                return;
            }
            if (string.IsNullOrEmpty(rawJson))
            {
                return;
            }

            Dictionary<string, object> msg = null;
            try
            {
                msg = _json.Deserialize<Dictionary<string, object>>(rawJson);
            }
            catch
            {
                return;
            }
            if (msg == null || !msg.ContainsKey("action") || msg["action"] == null)
            {
                return;
            }

            string action = msg["action"].ToString();
            string reqId = GetDictString(msg, "reqId");

            switch (action)
            {
                case "ready":
                    PushUiPrefsToWeb();
                    PushStatusToWeb();
                    PushAllLogsToWeb();
                    PushLoginStateToWeb();
                    PushLocalConfigToWeb();
                    if (_serviceState == "running")
                    {
                        await RefreshAccountsAndPushAsync();
                    }
                    break;

                case "setUiPrefs":
                {
                    string tab = GetDictString(msg, "activeTab");
                    string theme = GetDictString(msg, "theme");
                    if (!string.IsNullOrEmpty(tab))
                    {
                        _lastActiveTab = tab;
                    }
                    if (!string.IsNullOrEmpty(theme))
                    {
                        _lastTheme = theme;
                    }
                    break;
                }

                case "reloadUi":
                    ReloadUiHtmlFromDisk(false);
                    break;

                case "copyText":
                    CopyTextToClipboard(
                        GetDictString(msg, "text"),
                        GetDictString(msg, "toastMessage"));
                    break;

                case "startService":
                    await StartServiceAsync();
                    break;

                case "stopService":
                    StopServiceManually();
                    break;

                case "restartService":
                    await RestartBackendAsync();
                    break;

                case "openDir":
                    OpenProjectDirectory();
                    break;

                case "openWebUI":
                    OpenAdvancedWebUI();
                    break;

                case "openExternalUrl":
                    OpenInDefaultBrowser(GetDictString(msg, "url"));
                    break;

                case "startLogin":
                    StartOneClickLogin(
                        GetDictBool(msg, "enterprise"),
                        GetDictString(msg, "region"));
                    break;

                case "cancelLogin":
                    CancelOneClickLogin();
                    break;

                case "loginManualStart":
                    await HandleManualLoginStartAsync(
                        reqId,
                        GetDictString(msg, "provider"),
                        GetDictString(msg, "region"));
                    break;

                case "loginManualSubmit":
                    await HandleManualLoginSubmitAsync(
                        reqId,
                        GetDictString(msg, "id"),
                        GetDictString(msg, "url"));
                    break;

                case "refreshAccounts":
                    await RefreshAccountsAndPushAsync();
                    PushToastToWeb("info", "已刷新账号列表");
                    break;

                case "refreshAllTokens":
                    await RefreshAllTokensAndPushAsync();
                    break;

                case "refreshOneToken":
                    await RefreshOneTokenAndPushAsync(GetDictLong(msg, "id"));
                    break;

                case "syncAllUsage":
                    await SyncAllUsageAndPushAsync();
                    break;

                case "syncOneUsage":
                    await SyncOneUsageAndPushAsync(GetDictLong(msg, "id"));
                    break;

                case "toggleAccount":
                    await ToggleAccountAndPushAsync(
                        GetDictLong(msg, "id"),
                        GetDictBool(msg, "disabled"));
                    break;

                case "deleteAccount":
                    await DeleteAccountAndPushAsync(GetDictLong(msg, "id"));
                    break;

                case "getAnalytics":
                    await HandleApiProxyAsync(reqId, "GET", "/api/analytics/summary?range=" + Uri.EscapeDataString(GetDictString(msg, "range")), null);
                    break;

                case "getStatus":
                    await HandleApiProxyAsync(reqId, "GET", "/api/status", null);
                    break;

                case "getRequestLogs":
                    await HandleApiProxyAsync(reqId, "GET", BuildLogsQueryPath(msg), null);
                    break;

                case "clearRequestLogs":
                    await HandleApiProxyAsync(reqId, "DELETE", "/api/logs/requests", null);
                    break;

                case "exportRequestLogsCsv":
                    await ExportRequestLogsCsvAsync(BuildLogsQueryPath(msg));
                    break;

                case "getConversations":
                    await HandleApiProxyAsync(reqId, "GET", "/api/conversations?limit=200", null);
                    break;

                case "deleteConversation":
                    await HandleApiProxyAsync(reqId, "DELETE", "/api/conversations/item?key=" + Uri.EscapeDataString(GetDictString(msg, "key")), null);
                    break;

                case "clearConversations":
                    await HandleApiProxyAsync(reqId, "DELETE", "/api/conversations", null);
                    break;

                case "getConfig":
                    PushLocalConfigToWeb();
                    SendApiResponse(reqId, true, LoadLocalConfigJsonString(), string.Empty);
                    break;

                case "saveConfig":
                    await SaveLocalConfigAsync(reqId, GetDictString(msg, "configJson"), GetDictBool(msg, "restartAfter"));
                    break;

                case "generateApiKey":
                    SendApiResponse(reqId, true, "{\"key\":\"" + GenerateRandomApiKey() + "\"}", string.Empty);
                    break;

                case "testLatency":
                    await TestModelLatencyAsync(reqId, GetDictString(msg, "model"));
                    break;

                case "getAgents":
                    await HandleApiProxyAsync(reqId, "GET", "/api/agents", null);
                    break;

                case "getClaudeDesktopAgent":
                    await HandleApiProxyAsync(reqId, "GET", "/api/agents/claude-desktop", null);
                    break;

                case "previewClaudeDesktopAgent":
                    await HandleApiProxyAsync(reqId, "POST", "/api/agents/claude-desktop/preview", GetDictString(msg, "payloadJson"));
                    break;

                case "applyClaudeDesktopAgent":
                    await HandleApplyClaudeDesktopAsync(reqId, GetDictString(msg, "payloadJson"));
                    break;

                case "restoreClaudeDesktopAgent":
                    await HandleRestoreClaudeDesktopAsync(reqId, GetDictString(msg, "backupId"));
                    break;

                case "disableClaudeDesktopAgent":
                    await HandleApiProxyAsync(reqId, "POST", "/api/agents/claude-desktop/disable", "{}");
                    break;

                case "launchClaudeDesktop":
                    LaunchClaudeDesktopApp(reqId);
                    break;

                case "openPath":
                    OpenLocalDirectory(GetDictString(msg, "path"));
                    break;

                case "checkUpdate":
                {
                    string mode = GetDictString(msg, "mode");
                    string path = "/api/update/check" + (string.IsNullOrEmpty(mode) ? string.Empty : "?mode=" + Uri.EscapeDataString(mode));
                    await HandleApiProxyAsync(reqId, "GET", path, null);
                    break;
                }

                case "downloadUpdate":
                    await HandleApiProxyAsync(reqId, "POST", "/api/update/download", GetDictString(msg, "payloadJson"));
                    break;

                case "getUpdateStatus":
                    await HandleApiProxyAsync(reqId, "GET", "/api/update/status", null);
                    break;

                case "applyUpdate":
                    ApplyDownloadedUpdate(reqId, GetDictString(msg, "stagingDir"));
                    break;

                case "clearLogs":
                    lock (_logLock)
                    {
                        _logBuffer.Clear();
                    }
                    break;
            }
        }

        private static string BuildLogsQueryPath(Dictionary<string, object> msg)
        {
            List<string> parts = new List<string>();
            string status = GetDictString(msg, "status");
            string protocol = GetDictString(msg, "protocol");
            string model = GetDictString(msg, "model");
            string q = GetDictString(msg, "q");
            long accountId = GetDictLong(msg, "accountId");
            long limit = GetDictLong(msg, "limit");
            long offset = GetDictLong(msg, "offset");

            if (!string.IsNullOrEmpty(status)) parts.Add("status=" + Uri.EscapeDataString(status));
            if (!string.IsNullOrEmpty(protocol)) parts.Add("protocol=" + Uri.EscapeDataString(protocol));
            if (!string.IsNullOrEmpty(model)) parts.Add("model=" + Uri.EscapeDataString(model));
            if (!string.IsNullOrEmpty(q)) parts.Add("q=" + Uri.EscapeDataString(q));
            if (accountId > 0) parts.Add("account_id=" + accountId);
            if (limit > 0) parts.Add("limit=" + limit);
            if (offset > 0) parts.Add("offset=" + offset);

            string qs = parts.Count > 0 ? "?" + string.Join("&", parts.ToArray()) : string.Empty;
            return "/api/logs/requests" + qs;
        }

        private async Task HandleApiProxyAsync(string reqId, string method, string path, string body)
        {
            try
            {
                string raw;
                if (method == "DELETE")
                {
                    raw = await _apiClient.DeleteRawJsonAsync(path);
                }
                else if (method == "POST")
                {
                    raw = await _apiClient.PostRawJsonAsync(path, body);
                }
                else if (method == "PUT")
                {
                    raw = await _apiClient.PutRawJsonAsync(path, body);
                }
                else
                {
                    raw = await _apiClient.GetRawJsonAsync(path);
                }
                SendApiResponse(reqId, true, raw, string.Empty);
            }
            catch (Exception ex)
            {
                SendApiResponse(reqId, false, "null", ex.Message);
            }
        }

        private async Task HandleManualLoginStartAsync(string reqId, string provider, string region)
        {
            try
            {
                Dictionary<string, object> payload = new Dictionary<string, object>();
                payload["provider"] = string.IsNullOrEmpty(provider) ? "social" : provider;
                payload["stage"] = "prod";
                payload["region"] = region ?? string.Empty;
                string raw = await _apiClient.PostRawJsonAsync("/api/logins", _json.Serialize(payload));
                SendApiResponse(reqId, true, raw, string.Empty);
            }
            catch (Exception ex)
            {
                SendApiResponse(reqId, false, "null", ex.Message);
            }
        }

        private async Task HandleManualLoginSubmitAsync(string reqId, string id, string callbackUrl)
        {
            try
            {
                Dictionary<string, object> payload = new Dictionary<string, object>();
                payload["url"] = callbackUrl ?? string.Empty;
                string raw = await _apiClient.PostRawJsonAsync("/api/logins/" + Uri.EscapeDataString(id) + "/callback", _json.Serialize(payload));
                SendApiResponse(reqId, true, raw, string.Empty);
                await RefreshAccountsAndPushAsync();
            }
            catch (Exception ex)
            {
                SendApiResponse(reqId, false, "null", ex.Message);
            }
        }

        private async Task HandleApplyClaudeDesktopAsync(string reqId, string payloadJson)
        {
            try
            {
                string raw = await _apiClient.PostRawJsonAsync("/api/agents/claude-desktop/apply", string.IsNullOrEmpty(payloadJson) ? "{}" : payloadJson);
                ReloadRuntimeSettings();
                PushLocalConfigToWeb();
                SendApiResponse(reqId, true, raw, string.Empty);
            }
            catch (Exception ex)
            {
                SendApiResponse(reqId, false, "null", ex.Message);
            }
        }

        private async Task HandleRestoreClaudeDesktopAsync(string reqId, string backupId)
        {
            try
            {
                Dictionary<string, object> payload = new Dictionary<string, object>();
                payload["backup_id"] = backupId ?? string.Empty;
                string raw = await _apiClient.PostRawJsonAsync("/api/agents/claude-desktop/restore", _json.Serialize(payload));
                ReloadRuntimeSettings();
                PushLocalConfigToWeb();
                SendApiResponse(reqId, true, raw, string.Empty);
            }
            catch (Exception ex)
            {
                SendApiResponse(reqId, false, "null", ex.Message);
            }
        }

        private async Task ExportRequestLogsCsvAsync(string queryPath)
        {
            try
            {
                string exportPath = queryPath.Replace("/api/logs/requests", "/api/logs/requests/export");
                byte[] csvBytes = await _apiClient.DownloadAuthorizedBytesAsync(exportPath);

                using (SaveFileDialog dlg = new SaveFileDialog())
                {
                    dlg.Title = "导出使用记录报表";
                    dlg.Filter = "CSV 表格文件 (*.csv)|*.csv|所有文件 (*.*)|*.*";
                    dlg.FileName = string.Format("quickrelay-usage-logs-{0}.csv", DateTime.Now.ToString("yyyyMMdd-HHmm"));
                    if (dlg.ShowDialog(this) == DialogResult.OK)
                    {
                        File.WriteAllBytes(dlg.FileName, csvBytes);
                        PushToastToWeb("success", "已导出报表至 " + Path.GetFileName(dlg.FileName));
                    }
                }
            }
            catch (Exception ex)
            {
                PushToastToWeb("error", "导出 CSV 失败: " + ex.Message);
            }
        }

        private string LoadLocalConfigJsonString()
        {
            try
            {
                if (File.Exists(_configPath))
                {
                    return File.ReadAllText(_configPath, Encoding.UTF8);
                }
            }
            catch
            {
            }
            return "{}";
        }

        private void PushLocalConfigToWeb()
        {
            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["type"] = "localConfig";
            payload["configRaw"] = LoadLocalConfigJsonString();
            SendToWeb(payload);
        }

        private async Task SaveLocalConfigAsync(string reqId, string configJson, bool restartAfter)
        {
            try
            {
                Dictionary<string, object> parsed = _json.Deserialize<Dictionary<string, object>>(configJson);
                if (parsed == null || !parsed.ContainsKey("addr"))
                {
                    throw new InvalidOperationException("配置 JSON 格式无效");
                }
                string formatted = PrettyPrintConfigDict(parsed);
                File.WriteAllText(_configPath, formatted, new UTF8Encoding(false));
                AppendLog("[桌面端] 已保存配置文件: " + _configPath);

                if (restartAfter)
                {
                    await RestartBackendAsync();
                }
                else
                {
                    ReloadRuntimeSettings();
                    PushToastToWeb("success", "配置已保存，重启服务后生效");
                }
                PushLocalConfigToWeb();
                SendApiResponse(reqId, true, formatted, string.Empty);
            }
            catch (Exception ex)
            {
                SendApiResponse(reqId, false, "null", ex.Message);
                PushToastToWeb("error", "保存失败: " + ex.Message);
            }
        }

        private string PrettyPrintConfigDict(Dictionary<string, object> dict)
        {
            // 使用后端 API 格式校验或规范缩进输出
            string compact = _json.Serialize(dict);
            StringBuilder sb = new StringBuilder();
            int indent = 0;
            bool inStr = false;
            bool esc = false;
            for (int i = 0; i < compact.Length; i++)
            {
                char c = compact[i];
                if (esc)
                {
                    sb.Append(c);
                    esc = false;
                    continue;
                }
                if (inStr)
                {
                    sb.Append(c);
                    if (c == '\\') esc = true;
                    else if (c == '"') inStr = false;
                    continue;
                }
                switch (c)
                {
                    case '"':
                        inStr = true;
                        sb.Append(c);
                        break;
                    case '{':
                    case '[':
                        sb.Append(c);
                        if (i + 1 < compact.Length && compact[i + 1] != '}' && compact[i + 1] != ']')
                        {
                            indent++;
                            sb.Append('\n').Append(new string(' ', indent * 2));
                        }
                        break;
                    case '}':
                    case ']':
                        if (i > 0 && compact[i - 1] != '{' && compact[i - 1] != '[')
                        {
                            indent = Math.Max(0, indent - 1);
                            sb.Append('\n').Append(new string(' ', indent * 2));
                        }
                        sb.Append(c);
                        break;
                    case ',':
                        sb.Append(",\n").Append(new string(' ', indent * 2));
                        break;
                    case ':':
                        sb.Append(": ");
                        break;
                    default:
                        if (!char.IsWhiteSpace(c)) sb.Append(c);
                        break;
                }
            }
            sb.Append('\n');
            return sb.ToString();
        }

        private static string GenerateRandomApiKey()
        {
            const string hex = "0123456789abcdef";
            StringBuilder sb = new StringBuilder("sk-qr-", 40);
            byte[] buf = new byte[32];
            using (RNGCryptoServiceProvider rng = new RNGCryptoServiceProvider())
            {
                rng.GetBytes(buf);
            }
            for (int i = 0; i < buf.Length; i++)
            {
                sb.Append(hex[buf[i] % 16]);
            }
            return sb.ToString();
        }

        private async Task TestModelLatencyAsync(string reqId, string model)
        {
            string targetModel = string.IsNullOrEmpty(model) ? _defaultModel : model;
            string apiUrl = _apiBaseUrl + "/chat/completions";
            string apiKey = _primaryApiKey;

            try
            {
                string resultJson = await Task.Run(delegate
                {
                    Stopwatch sw = Stopwatch.StartNew();
                    Dictionary<string, object> reqPayload = new Dictionary<string, object>();
                    reqPayload["model"] = targetModel;
                    reqPayload["stream"] = false;
                    Dictionary<string, object> userMsg = new Dictionary<string, object>();
                    userMsg["role"] = "user";
                    userMsg["content"] = "连接测速测试：请仅回复「已连通」三个字。";
                    reqPayload["messages"] = new object[] { userMsg };

                    byte[] bodyBytes = Encoding.UTF8.GetBytes(_json.Serialize(reqPayload));
                    HttpWebRequest req = (HttpWebRequest)WebRequest.Create(apiUrl);
                    req.Method = "POST";
                    req.Proxy = null;
                    req.ContentType = "application/json; charset=utf-8";
                    req.ContentLength = bodyBytes.Length;
                    req.Timeout = 90000;
                    req.ReadWriteTimeout = 90000;
                    if (!string.IsNullOrEmpty(apiKey))
                    {
                        req.Headers["Authorization"] = "Bearer " + apiKey;
                    }
                    using (Stream st = req.GetRequestStream())
                    {
                        st.Write(bodyBytes, 0, bodyBytes.Length);
                    }

                    using (HttpWebResponse resp = (HttpWebResponse)req.GetResponse())
                    using (StreamReader rd = new StreamReader(resp.GetResponseStream(), Encoding.UTF8))
                    {
                        sw.Stop();
                        string respBody = rd.ReadToEnd();
                        Dictionary<string, object> respObj = _json.Deserialize<Dictionary<string, object>>(respBody);
                        string resolvedModel = respObj != null && respObj.ContainsKey("model") && respObj["model"] != null
                            ? respObj["model"].ToString()
                            : targetModel;
                        string replyText = string.Empty;
                        if (respObj != null && respObj.ContainsKey("choices") && respObj["choices"] is System.Collections.IEnumerable)
                        {
                            foreach (object c in (System.Collections.IEnumerable)respObj["choices"])
                            {
                                Dictionary<string, object> choice = c as Dictionary<string, object>;
                                if (choice != null && choice.ContainsKey("message") && choice["message"] is Dictionary<string, object>)
                                {
                                    Dictionary<string, object> msgObj = (Dictionary<string, object>)choice["message"];
                                    if (msgObj.ContainsKey("content") && msgObj["content"] != null)
                                    {
                                        replyText = msgObj["content"].ToString();
                                    }
                                }
                                break;
                            }
                        }

                        Dictionary<string, object> outObj = new Dictionary<string, object>();
                        outObj["ok"] = true;
                        outObj["elapsed_ms"] = sw.ElapsedMilliseconds;
                        outObj["resolved_model"] = resolvedModel;
                        outObj["reply"] = replyText;
                        return _json.Serialize(outObj);
                    }
                });
                SendApiResponse(reqId, true, resultJson, string.Empty);
            }
            catch (Exception ex)
            {
                string errDetail = ex.Message;
                WebException wex = ex as WebException;
                if (wex != null && wex.Response != null)
                {
                    try
                    {
                        using (StreamReader sr = new StreamReader(wex.Response.GetResponseStream(), Encoding.UTF8))
                        {
                            errDetail = sr.ReadToEnd();
                        }
                    }
                    catch
                    {
                    }
                }
                SendApiResponse(reqId, false, "null", errDetail);
            }
        }

        private void SendApiResponse(string reqId, bool ok, string rawJsonData, string error)
        {
            if (string.IsNullOrEmpty(reqId))
            {
                return;
            }
            object parsedData = null;
            if (ok && !string.IsNullOrEmpty(rawJsonData))
            {
                try
                {
                    parsedData = _json.DeserializeObject(rawJsonData);
                }
                catch
                {
                    parsedData = rawJsonData;
                }
            }
            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["type"] = "apiResponse";
            payload["reqId"] = reqId;
            payload["ok"] = ok;
            payload["data"] = parsedData;
            payload["error"] = error ?? string.Empty;
            SendToWeb(payload);
        }

        private void SendToWeb(object payload)
        {
            if (!_webViewReady || _webView == null || _webView.IsDisposed)
            {
                return;
            }

            if (this.InvokeRequired)
            {
                try
                {
                    this.BeginInvoke((MethodInvoker)delegate
                    {
                        SendToWeb(payload);
                    });
                }
                catch
                {
                }
                return;
            }

            try
            {
                if (_webView.CoreWebView2 != null)
                {
                    _webView.CoreWebView2.PostWebMessageAsJson(_json.Serialize(payload));
                }
            }
            catch
            {
            }
        }

        private void PushStatusToWeb()
        {
            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["type"] = "status";
            payload["serviceState"] = _serviceState;
            payload["statusText"] = _statusText;
            payload["apiUrl"] = _apiBaseUrl;
            payload["anthropicUrl"] = _anthropicBaseUrl;
            payload["port"] = _port;
            payload["models"] = _models;
            payload["defaultModel"] = _defaultModel;
            payload["primaryApiKey"] = _primaryApiKey;
            SendToWeb(payload);
        }

        private void PushLoginStateToWeb()
        {
            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["type"] = "loginState";
            payload["active"] = _loginActive;
            payload["stage"] = _loginStage;
            payload["url"] = _currentLoginUrl;
            payload["message"] = _loginMessage;
            SendToWeb(payload);
        }

        private void PushAllLogsToWeb()
        {
            string[] lines;
            lock (_logLock)
            {
                lines = new string[_logBuffer.Count];
                _logBuffer.CopyTo(lines, 0);
            }
            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["type"] = "logsInit";
            payload["lines"] = lines;
            SendToWeb(payload);
        }

        private void PushToastToWeb(string kind, string message)
        {
            if (string.IsNullOrEmpty(message))
            {
                return;
            }
            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["type"] = "toast";
            payload["kind"] = kind;
            payload["message"] = message;
            SendToWeb(payload);
        }

        private void ReloadRuntimeSettings()
        {
            string listenAddr = "127.0.0.1:8080";
            string defModel = "smart";
            string firstKey = string.Empty;
            List<string> parsedModels = new List<string>();
            if (File.Exists(_configPath))
            {
                try
                {
                    string rawJson = File.ReadAllText(_configPath, Encoding.UTF8);
                    Dictionary<string, object> dict = _json.Deserialize<Dictionary<string, object>>(rawJson);
                    if (dict != null && dict.ContainsKey("addr") && dict["addr"] != null)
                    {
                        string parsedAddr = dict["addr"].ToString().Trim();
                        if (!string.IsNullOrEmpty(parsedAddr))
                        {
                            listenAddr = parsedAddr;
                        }
                    }
                    if (dict != null && dict.ContainsKey("default_model") && dict["default_model"] != null)
                    {
                        string dm = dict["default_model"].ToString().Trim();
                        if (!string.IsNullOrEmpty(dm))
                        {
                            defModel = dm;
                        }
                    }
                    if (dict != null && dict.ContainsKey("api_keys") && dict["api_keys"] is System.Collections.IEnumerable)
                    {
                        foreach (object k in (System.Collections.IEnumerable)dict["api_keys"])
                        {
                            if (k != null && !string.IsNullOrWhiteSpace(k.ToString()))
                            {
                                firstKey = k.ToString().Trim();
                                break;
                            }
                        }
                    }
                    if (dict != null && dict.ContainsKey("models") && dict["models"] is System.Collections.IEnumerable)
                    {
                        foreach (object m in (System.Collections.IEnumerable)dict["models"])
                        {
                            if (m != null && !string.IsNullOrWhiteSpace(m.ToString()))
                            {
                                parsedModels.Add(m.ToString().Trim());
                            }
                        }
                    }
                }
                catch (Exception ex)
                {
                    AppendLog("[警告] 读取 config.json 失败，使用默认配置: " + ex.Message);
                }
            }

            if (parsedModels.Count > 0)
            {
                _models = parsedModels.ToArray();
            }
            _defaultModel = defModel;
            _primaryApiKey = firstKey;

            string host = "127.0.0.1";
            int port = 8080;

            Match ipv6Match = Regex.Match(listenAddr, @"^\[([^\]]+)\]:(\d+)$");
            if (ipv6Match.Success)
            {
                host = ipv6Match.Groups[1].Value;
                int.TryParse(ipv6Match.Groups[2].Value, out port);
            }
            else
            {
                Match hostPortMatch = Regex.Match(listenAddr, @"^([^:]*):(\d+)$");
                if (hostPortMatch.Success)
                {
                    string h = hostPortMatch.Groups[1].Value.Trim();
                    if (!string.IsNullOrEmpty(h))
                    {
                        host = h;
                    }
                    int.TryParse(hostPortMatch.Groups[2].Value, out port);
                }
            }

            if (port <= 0 || port > 65535)
            {
                port = 8080;
            }
            if (string.IsNullOrEmpty(host) || host == "0.0.0.0" || host == "::")
            {
                host = "127.0.0.1";
            }

            _displayHost = host;
            _probeHost = string.Equals(host, "localhost", StringComparison.OrdinalIgnoreCase) ? "127.0.0.1" : host;
            _port = port;

            string urlHost = host.Contains(":") ? string.Format("[{0}]", host) : host;
            _webuiUrl = string.Format("http://{0}:{1}/", urlHost, port);
            _apiBaseUrl = string.Format("http://{0}:{1}/v1", urlHost, port);
            _anthropicBaseUrl = string.Format("http://{0}:{1}", urlHost, port);

            _adminPassword = LoadOrGenerateAdminPassword();
            _apiClient.Configure(string.Format("http://{0}:{1}", urlHost, port), _adminPassword);
            PushStatusToWeb();
        }

        private string LoadOrGenerateAdminPassword()
        {
            string envPwd = Environment.GetEnvironmentVariable("QUICKPROXY_ADMIN_PASSWORD");
            if (!string.IsNullOrWhiteSpace(envPwd) && Encoding.UTF8.GetByteCount(envPwd.Trim()) >= 12)
            {
                return envPwd.Trim();
            }

            if (File.Exists(_passwordFilePath))
            {
                try
                {
                    string existing = File.ReadAllText(_passwordFilePath, Encoding.UTF8).Trim();
                    if (Encoding.UTF8.GetByteCount(existing) >= 12)
                    {
                        return existing;
                    }
                }
                catch
                {
                }
            }

            string dataDir = Path.GetDirectoryName(_passwordFilePath);
            if (!string.IsNullOrEmpty(dataDir) && !Directory.Exists(dataDir))
            {
                Directory.CreateDirectory(dataDir);
            }

            const string alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
            StringBuilder sb = new StringBuilder(24);
            using (RNGCryptoServiceProvider rng = new RNGCryptoServiceProvider())
            {
                byte[] buf = new byte[1];
                while (sb.Length < 24)
                {
                    rng.GetBytes(buf);
                    if (buf[0] < 248)
                    {
                        sb.Append(alphabet[buf[0] % alphabet.Length]);
                    }
                }
            }

            string generated = sb.ToString();
            File.WriteAllText(_passwordFilePath, generated, new UTF8Encoding(false));
            AppendLog("[桌面端] 已自动初始化本地管理密钥: " + _passwordFilePath);
            return generated;
        }

        private async Task EnsureBackendRunningAsync()
        {
            if (IsPortOpen(_probeHost, _port, 250))
            {
                string svc = await Task.Run(delegate { return _apiClient.ProbeHealthServiceSync(1200); });
                if (string.Equals(svc, "quickrelay-studio", StringComparison.OrdinalIgnoreCase))
                {
                    _managedByUs = false;
                    AppendLog(string.Format("[桌面端] 检测到已有 QuickRelay Studio 实例监听在 {0}:{1}，已自动接管连接。", _probeHost, _port));
                    SetServiceStatus("running", "运行中（外部实例）");
                    return;
                }

                // 端口已被其他程序（例如旧版代理）占用：绝不强连或覆盖，清晰提醒改端口即可并存运行
                _managedByUs = false;
                string conflictMsg = string.Format("端口 {0}:{1} 已被其他程序占用（如原版反代服务），请前往「系统设置」更换空闲端口后重启服务。", _probeHost, _port);
                AppendLog("[端口冲突] " + conflictMsg);
                SetServiceStatus("stopped", string.Format("端口 {0} 被占用", _port));
                PushToastToWeb("error", conflictMsg);
                return;
            }

            if (!File.Exists(_quickRelayExePath))
            {
                throw new FileNotFoundException("未找到核心代理程序: " + _quickRelayExePath);
            }

            SetServiceStatus("starting", "启动中...");
            AppendLog("[桌面端] 正在启动: " + _quickRelayExePath + " server");

            ProcessStartInfo psi = new ProcessStartInfo();
            psi.FileName = _quickRelayExePath;
            psi.Arguments = "server";
            psi.WorkingDirectory = _projectDir;
            psi.UseShellExecute = false;
            psi.CreateNoWindow = true;
            psi.RedirectStandardOutput = true;
            psi.RedirectStandardError = true;
            psi.StandardOutputEncoding = Encoding.UTF8;
            psi.StandardErrorEncoding = Encoding.UTF8;
            psi.EnvironmentVariables["QUICKPROXY_ADMIN_PASSWORD"] = _adminPassword;

            Process proc = new Process();
            proc.StartInfo = psi;
            proc.EnableRaisingEvents = true;

            proc.OutputDataReceived += delegate(object s, DataReceivedEventArgs args)
            {
                if (!string.IsNullOrEmpty(args.Data))
                {
                    AppendLog(args.Data);
                }
            };
            proc.ErrorDataReceived += delegate(object s, DataReceivedEventArgs args)
            {
                if (!string.IsNullOrEmpty(args.Data))
                {
                    AppendLog(args.Data);
                }
            };
            proc.Exited += delegate
            {
                bool expected = _forceExit || _isRestarting;
                AppendLog(expected ? "[桌面端] 反代服务已停止。" : "[桌面端] 警告：反代服务意外退出。");
                if (!expected && !this.IsDisposed && this.IsHandleCreated)
                {
                    try
                    {
                        this.BeginInvoke((MethodInvoker)delegate
                        {
                            SetServiceStatus("stopped", "已停止");
                        });
                    }
                    catch
                    {
                    }
                }
            };

            lock (_procLock)
            {
                _backendProcess = proc;
                _managedByUs = true;
            }

            proc.Start();
            proc.BeginOutputReadLine();
            proc.BeginErrorReadLine();

            bool ready = await WaitForPortOpenAsync(_probeHost, _port, 10000);
            if (!ready)
            {
                if (proc.HasExited)
                {
                    throw new InvalidOperationException("quickrelay.exe 启动后立即退出，请打开「实时日志」排查。");
                }
                SetServiceStatus("starting", "等待端口就绪...");
                return;
            }

            SetServiceStatus("running", "运行中");
        }

        private static bool IsPortOpen(string host, int port, int timeoutMs)
        {
            using (TcpClient client = new TcpClient())
            {
                try
                {
                    IAsyncResult ar = client.BeginConnect(host, port, null, null);
                    if (ar.AsyncWaitHandle.WaitOne(timeoutMs, false))
                    {
                        client.EndConnect(ar);
                        return true;
                    }
                }
                catch
                {
                }
                return false;
            }
        }

        private async Task<bool> WaitForPortOpenAsync(string host, int port, int maxWaitMs)
        {
            int elapsed = 0;
            while (elapsed < maxWaitMs)
            {
                if (_shutdownCts.IsCancellationRequested)
                {
                    return false;
                }
                lock (_procLock)
                {
                    if (_backendProcess != null && _backendProcess.HasExited)
                    {
                        return false;
                    }
                }
                if (IsPortOpen(host, port, 200))
                {
                    return true;
                }
                await Task.Delay(200);
                elapsed += 200;
            }
            return false;
        }

        private async Task StartServiceAsync()
        {
            try
            {
                ReloadRuntimeSettings();
                await EnsureBackendRunningAsync();
                if (_serviceState == "running")
                {
                    await RefreshAccountsAndPushAsync();
                    PushToastToWeb("success", "服务已启动");
                }
            }
            catch (Exception ex)
            {
                SetServiceStatus("stopped", "启动失败");
                PushToastToWeb("error", "启动失败: " + ex.Message);
            }
        }

        private void StopServiceManually()
        {
            _isRestarting = true;
            try
            {
                StopBackend();
                SetServiceStatus("stopped", "已停止");
                PushToastToWeb("info", "反代服务已停止");
            }
            finally
            {
                _isRestarting = false;
            }
        }

        private async Task RestartBackendAsync()
        {
            if (_isRestarting)
            {
                return;
            }
            _isRestarting = true;
            try
            {
                SetServiceStatus("starting", "重启中...");
                AppendLog("[桌面端] 正在重启反代服务...");

                StopBackend();
                await Task.Delay(350);

                ReloadRuntimeSettings();
                await EnsureBackendRunningAsync();
                if (_serviceState == "running")
                {
                    await RefreshAccountsAndPushAsync();
                    PushToastToWeb("success", "服务重启完成");
                }
            }
            catch (Exception ex)
            {
                AppendLog("[桌面端错误] 重启失败: " + ex.Message);
                SetServiceStatus("stopped", "重启失败");
                PushToastToWeb("error", "重启失败: " + ex.Message);
            }
            finally
            {
                _isRestarting = false;
            }
        }

        private void StopBackend()
        {
            Process proc = null;
            bool managed = false;
            lock (_procLock)
            {
                proc = _backendProcess;
                managed = _managedByUs;
                _backendProcess = null;
                _managedByUs = false;
            }

            if (managed && proc != null)
            {
                try
                {
                    if (!proc.HasExited)
                    {
                        proc.Kill();
                        proc.WaitForExit(3000);
                    }
                }
                catch
                {
                }
                finally
                {
                    try
                    {
                        proc.Dispose();
                    }
                    catch
                    {
                    }
                }
                return;
            }

            // 热更新重启后若自动接管了同目录运行中的 quickrelay.exe，退出/重启时仍精确停止该路径实例
            if (!string.IsNullOrEmpty(_quickRelayExePath))
            {
                string expectedPath = Path.GetFullPath(_quickRelayExePath);
                Process[] candidates = null;
                try
                {
                    candidates = Process.GetProcessesByName("quickrelay");
                }
                catch
                {
                }
                if (candidates != null)
                {
                    foreach (Process candidate in candidates)
                    {
                        try
                        {
                            string modPath = candidate.MainModule != null ? candidate.MainModule.FileName : string.Empty;
                            if (!string.IsNullOrEmpty(modPath) &&
                                string.Equals(Path.GetFullPath(modPath), expectedPath, StringComparison.OrdinalIgnoreCase))
                            {
                                if (!candidate.HasExited)
                                {
                                    candidate.Kill();
                                    candidate.WaitForExit(3000);
                                }
                            }
                        }
                        catch
                        {
                        }
                        finally
                        {
                            try
                            {
                                candidate.Dispose();
                            }
                            catch
                            {
                            }
                        }
                    }
                }
            }
        }

        private void StartOneClickLogin(bool enterprise, string region)
        {
            lock (_loginProcLock)
            {
                if (_loginProcess != null && !_loginProcess.HasExited)
                {
                    return;
                }
            }

            if (!File.Exists(_quickRelayExePath))
            {
                PushToastToWeb("error", "未找到核心代理程序: " + _quickRelayExePath);
                return;
            }

            string cleanRegion = (region ?? string.Empty).Trim().ToLowerInvariant();
            if (enterprise && !string.IsNullOrEmpty(cleanRegion) && !Regex.IsMatch(cleanRegion, @"^[a-z0-9-]+$"))
            {
                PushToastToWeb("error", "区域格式仅支持小写字母、数字和短横线（例如 us-east-1）");
                return;
            }

            _loginCancelling = false;
            _loginActive = true;
            _loginStage = "starting";
            _currentLoginUrl = string.Empty;
            _loginMessage = "正在开启本地回调并提取登录链接...";
            _loginLogBuffer = new StringBuilder();
            PushLoginStateToWeb();

            string args = enterprise
                ? "login -provider enterprise -stage prod"
                : "login -provider social -stage prod";
            if (enterprise && !string.IsNullOrEmpty(cleanRegion))
            {
                args = string.Format("{0} -region {1}", args, cleanRegion);
            }

            AppendLog("[一键登录] 执行: quickrelay.exe " + args);

            ProcessStartInfo psi = new ProcessStartInfo();
            psi.FileName = _quickRelayExePath;
            psi.Arguments = args;
            psi.WorkingDirectory = _projectDir;
            psi.UseShellExecute = false;
            psi.CreateNoWindow = true;
            psi.RedirectStandardOutput = true;
            psi.RedirectStandardError = true;
            psi.StandardOutputEncoding = Encoding.UTF8;
            psi.StandardErrorEncoding = Encoding.UTF8;

            Process proc = new Process();
            proc.StartInfo = psi;
            proc.EnableRaisingEvents = true;

            proc.OutputDataReceived += delegate(object s, DataReceivedEventArgs e)
            {
                HandleLoginOutputLine(e.Data);
            };
            proc.ErrorDataReceived += delegate(object s, DataReceivedEventArgs e)
            {
                HandleLoginOutputLine(e.Data);
            };
            proc.Exited += delegate
            {
                int exitCode = 1;
                try
                {
                    exitCode = proc.ExitCode;
                }
                catch
                {
                }
                if (!this.IsDisposed && this.IsHandleCreated)
                {
                    try
                    {
                        this.BeginInvoke((MethodInvoker)delegate
                        {
                            OnLoginProcessExited(exitCode);
                        });
                    }
                    catch
                    {
                    }
                }
            };

            lock (_loginProcLock)
            {
                _loginProcess = proc;
            }

            try
            {
                proc.Start();
                proc.BeginOutputReadLine();
                proc.BeginErrorReadLine();
            }
            catch (Exception ex)
            {
                AppendLog("[一键登录] 启动失败: " + ex.Message);
                OnLoginProcessExited(1);
            }
        }

        private void HandleLoginOutputLine(string line)
        {
            if (string.IsNullOrEmpty(line))
            {
                return;
            }

            AppendLog("[登录] " + line);
            lock (_loginProcLock)
            {
                _loginLogBuffer.AppendLine(line);
            }

            if (string.IsNullOrEmpty(_currentLoginUrl))
            {
                Match m = UrlRegex.Match(line);
                if (m.Success)
                {
                    string extracted = m.Value.TrimEnd('"', '\'', ')', ']', '>');
                    if (extracted.IndexOf("127.0.0.1", StringComparison.OrdinalIgnoreCase) < 0 &&
                        extracted.IndexOf("localhost", StringComparison.OrdinalIgnoreCase) < 0)
                    {
                        _currentLoginUrl = extracted;
                        _loginStage = "waiting";
                        _loginMessage = "已提取登录链接并打开浏览器，完成授权后自动入库（建议使用无痕窗口）。";
                        if (!this.IsDisposed && this.IsHandleCreated)
                        {
                            try
                            {
                                this.BeginInvoke((MethodInvoker)delegate
                                {
                                    PushLoginStateToWeb();
                                    OpenInDefaultBrowser(_currentLoginUrl);
                                });
                            }
                            catch
                            {
                            }
                        }
                    }
                }
            }
        }

        private async void OnLoginProcessExited(int exitCode)
        {
            string logSnapshot;
            lock (_loginProcLock)
            {
                if (_loginProcess != null)
                {
                    try
                    {
                        _loginProcess.Dispose();
                    }
                    catch
                    {
                    }
                    _loginProcess = null;
                }
                logSnapshot = _loginLogBuffer.ToString();
            }

            _loginActive = false;
            _currentLoginUrl = string.Empty;

            if (_loginCancelling)
            {
                _loginStage = "cancelled";
                _loginMessage = "已取消本次登录。";
                PushLoginStateToWeb();
                return;
            }

            if (exitCode == 0)
            {
                _loginStage = "success";
                _loginMessage = "✓ 登录成功！账号已保存并自动加入轮询池。";
                PushLoginStateToWeb();
                PushToastToWeb("success", "账号登录成功");
                await RefreshAccountsAndPushAsync();
                return;
            }

            string lowerLog = logSnapshot.ToLowerInvariant();
            _loginStage = "error";
            if (lowerLog.Contains("18080") || lowerLog.Contains("address already in use") || lowerLog.Contains("bind") || lowerLog.Contains("only one usage"))
            {
                _loginMessage = "18080 回调端口被占用，可在上方切换「手动回填模式」直接贴入浏览器回调 URL 完成登录。";
            }
            else
            {
                _loginMessage = "登录未完成或已失败，请点击左侧「实时日志」查看详情。";
            }
            PushLoginStateToWeb();
        }

        private void CancelOneClickLogin()
        {
            _loginCancelling = true;
            StopLoginProcess();
        }

        private void StopLoginProcess()
        {
            Process proc = null;
            lock (_loginProcLock)
            {
                proc = _loginProcess;
                _loginProcess = null;
            }

            if (proc != null)
            {
                try
                {
                    if (!proc.HasExited)
                    {
                        proc.Kill();
                        proc.WaitForExit(2000);
                    }
                }
                catch
                {
                }
                finally
                {
                    try
                    {
                        proc.Dispose();
                    }
                    catch
                    {
                    }
                }
            }
        }

        private async Task RefreshAccountsAndPushAsync()
        {
            try
            {
                List<AccountViewDto> items = await _apiClient.ListAccountsAsync();
                Dictionary<string, object> payload = new Dictionary<string, object>();
                payload["type"] = "accounts";
                payload["items"] = items;
                SendToWeb(payload);
            }
            catch (Exception ex)
            {
                AppendLog("[账号列表] 获取失败: " + ex.Message);
            }
        }

        private async Task RefreshAllTokensAndPushAsync()
        {
            try
            {
                string summary = await _apiClient.RefreshAllTokensAsync();
                await RefreshAccountsAndPushAsync();
                PushToastToWeb("success", summary);
            }
            catch (Exception ex)
            {
                PushToastToWeb("error", "刷新失败: " + ex.Message);
            }
        }

        private async Task RefreshOneTokenAndPushAsync(long id)
        {
            try
            {
                await _apiClient.RefreshOneTokenAsync(id);
                PushToastToWeb("success", string.Format("账号 #{0} Token 刷新成功", id));
            }
            catch (Exception ex)
            {
                PushToastToWeb("error", "刷新失败: " + ex.Message);
            }
            await RefreshAccountsAndPushAsync();
        }

        private void PushUsageSyncDoneToWeb()
        {
            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["type"] = "usageSyncDone";
            SendToWeb(payload);
        }

        private async Task SyncAllUsageAndPushAsync()
        {
            try
            {
                string summary = await _apiClient.SyncAllUsageAsync();
                PushToastToWeb("success", summary);
            }
            catch (Exception ex)
            {
                PushToastToWeb("error", "同步余额失败: " + ex.Message);
            }
            await RefreshAccountsAndPushAsync();
            PushUsageSyncDoneToWeb();
        }

        private async Task SyncOneUsageAndPushAsync(long id)
        {
            try
            {
                await _apiClient.SyncOneUsageAsync(id);
                PushToastToWeb("success", string.Format("账号 #{0} 余额已同步", id));
            }
            catch (Exception ex)
            {
                PushToastToWeb("error", "同步余额失败: " + ex.Message);
            }
            await RefreshAccountsAndPushAsync();
        }

        private void StartAutoRefreshTimer()
        {
            if (_autoRefreshTimer != null)
            {
                return;
            }
            _autoRefreshTimer = new System.Windows.Forms.Timer();
            _autoRefreshTimer.Interval = 30000;
            _autoRefreshTimer.Tick += async delegate
            {
                if (!this.Visible || !_webViewReady || _isRestarting || _autoRefreshBusy || _serviceState != "running")
                {
                    return;
                }
                _autoRefreshBusy = true;
                try
                {
                    await RefreshAccountsAndPushAsync();
                    Dictionary<string, object> tick = new Dictionary<string, object>();
                    tick["type"] = "autoTick";
                    SendToWeb(tick);
                }
                finally
                {
                    _autoRefreshBusy = false;
                }
            };
            _autoRefreshTimer.Start();
        }

        private async Task ToggleAccountAndPushAsync(long id, bool disabled)
        {
            try
            {
                await _apiClient.SetAccountDisabledAsync(id, disabled);
                await RefreshAccountsAndPushAsync();
                PushToastToWeb("success", disabled ? "已停用账号" : "已启用账号");
            }
            catch (Exception ex)
            {
                PushToastToWeb("error", "更新状态失败: " + ex.Message);
            }
        }

        private async Task DeleteAccountAndPushAsync(long id)
        {
            try
            {
                await _apiClient.DeleteAccountAsync(id);
                await RefreshAccountsAndPushAsync();
                PushToastToWeb("success", "已删除账号");
            }
            catch (Exception ex)
            {
                PushToastToWeb("error", "删除失败: " + ex.Message);
            }
        }

        private void SetServiceStatus(string state, string text)
        {
            _serviceState = state;
            _statusText = text;
            PushStatusToWeb();

            if (_notifyIcon != null)
            {
                string trayTip = "QuickRelay Studio - " + text;
                if (trayTip.Length > 63)
                {
                    trayTip = trayTip.Substring(0, 63);
                }
                _notifyIcon.Text = trayTip;
            }
        }

        private void CopyTextToClipboard(string text, string toastMessage)
        {
            if (string.IsNullOrEmpty(text))
            {
                return;
            }
            try
            {
                if (this.InvokeRequired)
                {
                    this.BeginInvoke((MethodInvoker)delegate
                    {
                        CopyTextToClipboard(text, toastMessage);
                    });
                    return;
                }
                Clipboard.SetText(text);
                if (!string.IsNullOrEmpty(toastMessage))
                {
                    PushToastToWeb("success", toastMessage);
                }
            }
            catch (Exception ex)
            {
                PushToastToWeb("error", "复制失败: " + ex.Message);
            }
        }

        private void OpenAdvancedWebUI()
        {
            if (!string.IsNullOrEmpty(_adminPassword))
            {
                CopyTextToClipboard(_adminPassword, "已复制管理密码到剪贴板，正在打开网页版");
            }
            OpenInDefaultBrowser(_webuiUrl);
        }

        private void OpenInDefaultBrowser(string url)
        {
            if (string.IsNullOrEmpty(url))
            {
                return;
            }
            try
            {
                ProcessStartInfo psi = new ProcessStartInfo(url);
                psi.UseShellExecute = true;
                Process.Start(psi);
            }
            catch (Exception ex)
            {
                AppendLog("[桌面端] 无法打开浏览器: " + ex.Message);
            }
        }

        private void OpenProjectDirectory()
        {
            OpenLocalDirectory(_projectDir);
        }

        private void OpenLocalDirectory(string targetPath)
        {
            try
            {
                if (string.IsNullOrEmpty(targetPath))
                {
                    PushToastToWeb("error", "目录路径为空");
                    return;
                }
                string dir = targetPath;
                if (File.Exists(dir))
                {
                    dir = Path.GetDirectoryName(dir);
                }
                if (!Directory.Exists(dir))
                {
                    string parent = Path.GetDirectoryName(dir);
                    if (!string.IsNullOrEmpty(parent) && Directory.Exists(parent))
                    {
                        dir = parent;
                    }
                    else
                    {
                        PushToastToWeb("info", "目标目录尚未生成: " + dir);
                        return;
                    }
                }
                ProcessStartInfo psi = new ProcessStartInfo(dir);
                psi.UseShellExecute = true;
                Process.Start(psi);
            }
            catch (Exception ex)
            {
                PushToastToWeb("error", "无法打开目录: " + ex.Message);
            }
        }

        private void ApplyDownloadedUpdate(string reqId, string stagingDir)
        {
            try
            {
                if (string.IsNullOrEmpty(stagingDir))
                {
                    throw new InvalidOperationException("更新暂存目录为空");
                }
                string updatesRoot = Path.GetFullPath(Path.Combine(_projectDir, "data", "updates"));
                string fullStaging = Path.GetFullPath(stagingDir);
                if (!fullStaging.StartsWith(updatesRoot + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase))
                {
                    throw new InvalidOperationException("非法的更新暂存目录路径");
                }
                if (!Directory.Exists(fullStaging) ||
                    !File.Exists(Path.Combine(fullStaging, "QuickRelay-Studio.exe")) ||
                    !File.Exists(Path.Combine(fullStaging, "quickrelay.exe")))
                {
                    throw new InvalidOperationException("更新包文件不完整，缺少核心可执行文件");
                }

                string scriptPath = Path.Combine(Path.GetTempPath(), "quickrelay-apply-" + Guid.NewGuid().ToString("N") + ".ps1");
                string selfPid = Process.GetCurrentProcess().Id.ToString();
                string backendExe = _quickRelayExePath ?? string.Empty;
                string desktopExe = Path.Combine(_projectDir, "QuickRelay-Studio.exe");

                string[] scriptLines = new string[]
                {
                    "param(",
                    "  [string]$TargetDir,",
                    "  [string]$StagingDir,",
                    "  [int]$OldPid,",
                    "  [string]$BackendExe,",
                    "  [string]$DesktopExe",
                    ")",
                    "$ErrorActionPreference = 'SilentlyContinue'",
                    "for ($i = 0; $i -lt 60; $i++) {",
                    "  $p = Get-Process -Id $OldPid -ErrorAction SilentlyContinue",
                    "  if (-not $p) { break }",
                    "  Start-Sleep -Milliseconds 250",
                    "}",
                    "$p = Get-Process -Id $OldPid -ErrorAction SilentlyContinue",
                    "if ($p) { Stop-Process -Id $OldPid -Force; Start-Sleep -Milliseconds 300 }",
                    "if ($BackendExe) {",
                    "  $normBackend = [System.IO.Path]::GetFullPath($BackendExe)",
                    "  Get-Process -Name 'quickrelay' -ErrorAction SilentlyContinue | ForEach-Object {",
                    "    try {",
                    "      if ($_.Path -and ([System.IO.Path]::GetFullPath($_.Path) -ieq $normBackend)) {",
                    "        Stop-Process -Id $_.Id -Force",
                    "      }",
                    "    } catch {}",
                    "  }",
                    "  Start-Sleep -Milliseconds 300",
                    "}",
                    "$normStaging = [System.IO.Path]::GetFullPath($StagingDir).TrimEnd('\\') + '\\'",
                    "Get-ChildItem -Path $StagingDir -Recurse -Force | ForEach-Object {",
                    "  $full = [System.IO.Path]::GetFullPath($_.FullName)",
                    "  $rel = $full.Substring($normStaging.Length)",
                    "  if (-not $rel) { return }",
                    "  if ($rel -ieq 'data' -or $rel.StartsWith('data\\', [System.StringComparison]::OrdinalIgnoreCase)) { return }",
                    "  $dest = Join-Path $TargetDir $rel",
                    "  if ($_.PSIsContainer) {",
                    "    if (-not (Test-Path $dest)) { New-Item -ItemType Directory -Path $dest -Force | Out-Null }",
                    "    return",
                    "  }",
                    "  if (($rel -ieq 'config.json') -and (Test-Path $dest)) { return }",
                    "  $destParent = Split-Path -Parent $dest",
                    "  if (-not (Test-Path $destParent)) { New-Item -ItemType Directory -Path $destParent -Force | Out-Null }",
                    "  Copy-Item -Path $full -Destination $dest -Force",
                    "}",
                    "if (Test-Path $DesktopExe) {",
                    "  Start-Process -FilePath $DesktopExe -WorkingDirectory $TargetDir",
                    "}",
                    "Remove-Item -Path $PSCommandPath -Force -ErrorAction SilentlyContinue"
                };

                File.WriteAllText(scriptPath, string.Join(Environment.NewLine, scriptLines), new UTF8Encoding(true));

                string argLine = string.Format(
                    "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File \"{0}\" -TargetDir \"{1}\" -StagingDir \"{2}\" -OldPid {3} -BackendExe \"{4}\" -DesktopExe \"{5}\"",
                    scriptPath,
                    _projectDir.TrimEnd('\\'),
                    fullStaging.TrimEnd('\\'),
                    selfPid,
                    backendExe,
                    desktopExe);

                ProcessStartInfo psi = new ProcessStartInfo("powershell.exe", argLine);
                psi.UseShellExecute = false;
                psi.CreateNoWindow = true;
                psi.WindowStyle = ProcessWindowStyle.Hidden;
                Process.Start(psi);

                SendApiResponse(reqId, true, "{\"applying\":true}", string.Empty);

                Thread exitThread = new Thread(delegate()
                {
                    Thread.Sleep(250);
                    try
                    {
                        if (!this.IsDisposed && this.IsHandleCreated)
                        {
                            this.BeginInvoke((MethodInvoker)delegate
                            {
                                ExitApplicationCompletely();
                            });
                        }
                    }
                    catch
                    {
                    }
                });
                exitThread.IsBackground = true;
                exitThread.Start();
            }
            catch (Exception ex)
            {
                SendApiResponse(reqId, false, "null", "启动自更新失败: " + ex.Message);
            }
        }

        private void LaunchClaudeDesktopApp(string reqId)
        {
            try
            {
                string localAppData = Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData);
                if (string.IsNullOrEmpty(localAppData))
                {
                    localAppData = Environment.GetEnvironmentVariable("LOCALAPPDATA") ?? string.Empty;
                }

                string[] exeCandidates = new string[]
                {
                    Path.Combine(localAppData, "AnthropicClaude", "Claude.exe"),
                    Path.Combine(localAppData, "Programs", "Claude", "Claude.exe"),
                    Path.Combine(localAppData, "Claude", "Claude.exe")
                };

                for (int i = 0; i < exeCandidates.Length; i++)
                {
                    if (File.Exists(exeCandidates[i]))
                    {
                        ProcessStartInfo exePsi = new ProcessStartInfo(exeCandidates[i]);
                        exePsi.UseShellExecute = true;
                        Process.Start(exePsi);

                        Dictionary<string, object> okExe = new Dictionary<string, object>();
                        okExe["launched"] = true;
                        okExe["mode"] = "exe";
                        okExe["target"] = exeCandidates[i];
                        SendApiResponse(reqId, true, _json.Serialize(okExe), string.Empty);
                        return;
                    }
                }

                string pkgRoot = Path.Combine(localAppData, "Packages");
                if (Directory.Exists(pkgRoot))
                {
                    string[] claudePkgs = Directory.GetDirectories(pkgRoot, "Claude_*");
                    if (claudePkgs != null && claudePkgs.Length > 0)
                    {
                        string pfn = Path.GetFileName(claudePkgs[0]);
                        string appTarget = "shell:AppsFolder\\" + pfn + "!Claude";
                        ProcessStartInfo msixPsi = new ProcessStartInfo("explorer.exe", appTarget);
                        msixPsi.UseShellExecute = true;
                        Process.Start(msixPsi);

                        Dictionary<string, object> okMsix = new Dictionary<string, object>();
                        okMsix["launched"] = true;
                        okMsix["mode"] = "msix";
                        okMsix["target"] = appTarget;
                        SendApiResponse(reqId, true, _json.Serialize(okMsix), string.Empty);
                        return;
                    }
                }

                SendApiResponse(reqId, false, "null", "未在本机检测到 Claude Desktop 安装（未找到 Claude.exe 或 MSIX 包）");
            }
            catch (Exception ex)
            {
                SendApiResponse(reqId, false, "null", "启动 Claude Desktop 失败: " + ex.Message);
            }
        }

        private void AppendLog(string line)
        {
            if (string.IsNullOrEmpty(line))
            {
                return;
            }

            lock (_logLock)
            {
                _logBuffer.AddLast(line);
                while (_logBuffer.Count > MaxLogLines)
                {
                    _logBuffer.RemoveFirst();
                }
            }

            Dictionary<string, object> payload = new Dictionary<string, object>();
            payload["type"] = "logLine";
            payload["line"] = line;
            SendToWeb(payload);
        }

        private void HideToTray()
        {
            this.Hide();
            if (!_balloonShown && _notifyIcon != null)
            {
                _balloonShown = true;
                _notifyIcon.ShowBalloonTip(
                    3000,
                    "QuickRelay Studio",
                    "程序已最小化到系统托盘并在后台运行，双击托盘图标可重新打开。",
                    ToolTipIcon.Info);
            }
        }

        private void RestoreFromTray()
        {
            if (!this.Visible)
            {
                this.Show();
            }
            if (this.WindowState == FormWindowState.Minimized)
            {
                this.WindowState = FormWindowState.Normal;
            }
            this.Activate();
        }

        private void ExitApplicationCompletely()
        {
            _forceExit = true;
            this.Close();
        }

        private void MainForm_FormClosing(object sender, FormClosingEventArgs e)
        {
            if (!_forceExit && e.CloseReason == CloseReason.UserClosing)
            {
                e.Cancel = true;
                HideToTray();
                return;
            }

            _forceExit = true;
            CleanupResources();
        }

        private void Application_ApplicationExit(object sender, EventArgs e)
        {
            _forceExit = true;
            CleanupResources();
        }

        private void CleanupResources()
        {
            try
            {
                _shutdownCts.Cancel();
            }
            catch
            {
            }

            _loginCancelling = true;
            if (_autoRefreshTimer != null)
            {
                try
                {
                    _autoRefreshTimer.Stop();
                    _autoRefreshTimer.Dispose();
                    _autoRefreshTimer = null;
                }
                catch
                {
                }
            }
            lock (_uiWatchLock)
            {
                if (_uiWatcher != null)
                {
                    try
                    {
                        _uiWatcher.EnableRaisingEvents = false;
                        _uiWatcher.Dispose();
                    }
                    catch
                    {
                    }
                    _uiWatcher = null;
                }
                if (_uiReloadDebounce != null)
                {
                    try
                    {
                        _uiReloadDebounce.Dispose();
                    }
                    catch
                    {
                    }
                    _uiReloadDebounce = null;
                }
            }
            StopLoginProcess();
            StopBackend();

            if (_notifyIcon != null)
            {
                try
                {
                    _notifyIcon.Visible = false;
                    _notifyIcon.Dispose();
                    _notifyIcon = null;
                }
                catch
                {
                }
            }
        }

        private static string GetDictString(Dictionary<string, object> dict, string key)
        {
            if (dict != null && dict.ContainsKey(key) && dict[key] != null)
            {
                return dict[key].ToString();
            }
            return string.Empty;
        }

        private static bool GetDictBool(Dictionary<string, object> dict, string key)
        {
            if (dict != null && dict.ContainsKey(key) && dict[key] != null)
            {
                bool b;
                if (bool.TryParse(dict[key].ToString(), out b))
                {
                    return b;
                }
            }
            return false;
        }

        private static long GetDictLong(Dictionary<string, object> dict, string key)
        {
            if (dict != null && dict.ContainsKey(key) && dict[key] != null)
            {
                long v;
                if (long.TryParse(dict[key].ToString(), out v))
                {
                    return v;
                }
            }
            return 0;
        }
    }
}
