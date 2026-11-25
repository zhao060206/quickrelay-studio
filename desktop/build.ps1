param(
    [string]$OutDir = ""
)

$DesktopDir = $PSScriptRoot
$ProjectDir = Split-Path -Parent $DesktopDir
$BinDir = Join-Path $ProjectDir "bin"
if ($OutDir -ne "") {
    if (-not (Test-Path $OutDir)) {
        New-Item -ItemType Directory -Path $OutDir -Force | Out-Null
    }
    $TargetDir = $OutDir
} else {
    $TargetDir = $BinDir
}

$CscExe = "C:\Windows\Microsoft.NET\Framework64\v4.0.30319\csc.exe"
if (-not (Test-Path $CscExe)) {
    Write-Error "未找到 .NET Framework x64 编译器: $CscExe"
    exit 1
}

$CoreDll = Join-Path $BinDir "Microsoft.Web.WebView2.Core.dll"
$WinFormsDll = Join-Path $BinDir "Microsoft.Web.WebView2.WinForms.dll"
$LoaderDll = Join-Path $BinDir "WebView2Loader.dll"
foreach ($dll in @($CoreDll, $WinFormsDll, $LoaderDll)) {
    if (-not (Test-Path $dll)) {
        Write-Error "缺少 WebView2 依赖文件: $dll"
        exit 1
    }
}

$IconPath = Join-Path $DesktopDir "app.ico"
$UiHtmlPath = Join-Path $DesktopDir "ui.html"
$SourcePath = Join-Path $DesktopDir "Program.cs"
$OutputExe = Join-Path $TargetDir "QuickRelay-Studio.exe"

if (-not (Test-Path $UiHtmlPath)) {
    Write-Error "缺少前端页面文件: $UiHtmlPath"
    exit 1
}

Write-Host "正在编译 QuickRelay-Studio.exe ..." -ForegroundColor Cyan

& $CscExe `
    /nologo `
    /target:winexe `
    /platform:x64 `
    /optimize+ `
    /utf8output `
    "/win32icon:$IconPath" `
    "/resource:$UiHtmlPath,ui.html" `
    "/r:System.dll" `
    "/r:System.Core.dll" `
    "/r:System.Drawing.dll" `
    "/r:System.Windows.Forms.dll" `
    "/r:System.Web.Extensions.dll" `
    "/r:$CoreDll" `
    "/r:$WinFormsDll" `
    "/out:$OutputExe" `
    $SourcePath

if ($LASTEXITCODE -ne 0) {
    Write-Error "编译失败，退出码: $LASTEXITCODE"
    exit $LASTEXITCODE
}

$FileInfo = Get-Item $OutputExe
Write-Host ("编译成功: {0} ({1} 字节)" -f $FileInfo.FullName, $FileInfo.Length) -ForegroundColor Green
