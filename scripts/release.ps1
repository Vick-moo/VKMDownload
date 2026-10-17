<#
.SYNOPSIS
    构建、打包并发布一个 GitHub Release。

.DESCRIPTION
    一条命令做完发版：构建 exe、打包扩展、压缩源码、创建 release、上传三个文件。

    token 不需要你提供：脚本从 git 的凭据管理器读取（就是你第一次 git push 时
    授权保存的那个）。这一点是本脚本存在的主要理由 —— 发 GitHub Release 必须
    用 API，而 API 必须认证，但认证信息其实早就在你机器上了。

.EXAMPLE
    .\scripts\release.ps1 -Version 0.2.3
    .\scripts\release.ps1 -Version 0.2.3 -NotesFile notes.md
    .\scripts\release.ps1 -Version 0.2.3 -Draft
#>
[CmdletBinding()]
param(
    # 版本号，不带 v。会写进 app.go 与 wails.json，并作为 tag（v0.2.3）。
    [Parameter(Mandatory)][string]$Version,

    # release 说明。省略则用一段包含下载指引的默认文本。
    [string]$NotesFile,

    # 建成草稿，先自己看一眼再发布。
    [switch]$Draft,

    # 跳过构建，用 build/bin 里已有的 exe（调试脚本时用）。
    [switch]$SkipBuild
)

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$repo = 'Vick-moo/VKMDownload'

# --------------------------------------------------------------- 预检 ---

# 缺一即停：tag 已存在 / 工作区不干净 / 版本号替换无命中。
# 只做检查，不动下面的发版主流程。
git rev-parse --verify "v$Version" 2>$null | Out-Null
if ($LASTEXITCODE -eq 0) { throw "tag v$Version 已存在，先确认版本号是否重复。" }
$dirty = git status --porcelain 2>$null
if ($dirty) { throw "工作区不干净，先提交或暂存再发版。`n$dirty" }

# The release tag lands on the REMOTE main (target_commitish), so a local
# commit that was never pushed would make the tag and the source zip point
# at different trees — silently. Fetch first so the check compares against
# reality, not the last known state.
git fetch origin 2>$null
if ($LASTEXITCODE -ne 0) { throw "git fetch origin 失败，无法确认远端状态。" }
$unpushed = git rev-list origin/main..main 2>$null
if ($unpushed) {
    $n = ($unpushed | Measure-Object).Count
    throw "main 上有 $n 个未 push 的 commit，先 git push 再发版。"
}

# --------------------------------------------------------------- 版本号 ---

$desktop = Join-Path $root 'desktop'
$appGo = Join-Path $desktop 'app.go'
$wailsJson = Join-Path $desktop 'wails.json'
$utf8 = New-Object System.Text.UTF8Encoding($false)

Write-Host "版本 -> $Version" -ForegroundColor Cyan

$hits = 0
foreach ($file in @($appGo, $wailsJson)) {
    $text = [System.IO.File]::ReadAllText($file, $utf8)
    $updated = $text -replace 'const appVersion = "[^"]*"', "const appVersion = `"$Version`""
    $updated = $updated -replace '"productVersion": "[^"]*"', "`"productVersion`": `"$Version`""
    if ($updated -ne $text) {
        $hits++
        [System.IO.File]::WriteAllText($file, $updated, $utf8)
        Write-Host "  已更新 $(Split-Path $file -Leaf)" -ForegroundColor DarkGray
    }
}
# What must hold is that both files END UP carrying the version, not that this
# run rewrote them: committing the bump before releasing (so the release's
# source zip matches the tag) is a supported flow and produces zero
# replacements.
$appText = [System.IO.File]::ReadAllText($appGo, $utf8)
$jsonText = [System.IO.File]::ReadAllText($wailsJson, $utf8)
if (-not ($appText.Contains("const appVersion = `"$Version`"") -and $jsonText.Contains("`"productVersion`": `"$Version`""))) {
    throw "版本号校验失败：app.go / wails.json 里没有 $Version。"
}

# ---------------------------------------------------------------- 构建 ---

if (-not $SkipBuild) {
    Write-Host '构建中...' -ForegroundColor Cyan
    Push-Location $desktop
    try {
        # -trimpath: strips build-machine paths out of the binary (the
        # pclntab otherwise carries C:\Users\<you>\... for every file) and
        # makes builds reproducible; costs nothing measurable.
        & wails build -trimpath | Select-String 'Built|ERROR' | ForEach-Object { Write-Host "  $_" }
        if ($LASTEXITCODE -ne 0) { throw "wails build 失败（exit $LASTEXITCODE）" }
    } finally { Pop-Location }
}

$exe = Join-Path $desktop 'build\bin\VKMDownload.exe'
if (-not (Test-Path $exe)) { throw "找不到 $exe" }

# ---------------------------------------------------------------- 打包 ---

Write-Host '打包...' -ForegroundColor Cyan
$stage = Join-Path $env:TEMP "vkm-release-$Version"
if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
New-Item -ItemType Directory -Path $stage -Force | Out-Null

Copy-Item $exe (Join-Path $stage 'VKMDownload.exe')

# 扩展：解压后要直接得到 extension 目录，所以先套一层。
# ZipFile 会写反斜杠，Linux/macOS 上解压会变成一个奇怪的文件名，所以手工写条目用正斜杠。
Add-Type -AssemblyName System.IO.Compression.FileSystem
$extSrc = Join-Path $root 'extension'
$extStage = Join-Path $env:TEMP "vkm-ext-$Version"
if (Test-Path $extStage) { Remove-Item $extStage -Recurse -Force }
New-Item -ItemType Directory -Path (Join-Path $extStage 'extension') -Force | Out-Null
# Tests travel with the source zip; the extension zip users load into their
# browser should not carry them (store reviewers do look at what ships).
Get-ChildItem $extSrc | Where-Object { $_.Name -ne 'test' } |
    Copy-Item -Destination (Join-Path $extStage 'extension') -Recurse -Force

$zipPath = Join-Path $stage 'VKMDownload-extension.zip'
$zip = [System.IO.Compression.ZipFile]::Open($zipPath, 'Create')
try {
    Get-ChildItem (Join-Path $extStage 'extension') -Recurse -File | ForEach-Object {
        $rel = $_.FullName.Substring($extStage.Length + 1).Replace('\', '/')
        [void][System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip, $_.FullName, $rel)
    }
} finally { $zip.Dispose() }

Push-Location $root
try { & git archive --format=zip -o (Join-Path $stage 'VKMDownload-source.zip') HEAD }
finally { Pop-Location }

Get-ChildItem $stage | ForEach-Object {
    $size = if ($_.Length -gt 1MB) { "$([Math]::Round($_.Length/1MB,1)) MB" } else { "$([Math]::Round($_.Length/1KB,1)) KB" }
    Write-Host "  $($_.Name)  $size" -ForegroundColor DarkGray
}

# ----------------------------------------------------------------- 发布 ---

# 从 git 的凭据管理器取，而不是向用户索要。
$credOut = "protocol=https`nhost=github.com`n`n" | git credential fill 2>$null
$token = ($credOut | Where-Object { $_ -like 'password=*' }) -replace '^password=', ''
if (-not $token) {
    throw 'git 凭据管理器里没有 github.com 的凭据。先手动 git push 一次，让系统记下它。'
}

$headers = @{
    Authorization = "Bearer $token"
    'User-Agent'  = 'vkm-release'
    Accept        = 'application/vnd.github+json'
}

if ($NotesFile) {
    $body = [System.IO.File]::ReadAllText((Resolve-Path $NotesFile), $utf8)
} else {
    $body = @"
VKMDownload $Version

## 下载哪个

| 文件 | 用途 |
|---|---|
| ``VKMDownload.exe`` | 主程序，**双击即运行，无需安装** |
| ``VKMDownload-extension.zip`` | 浏览器扩展，解压后加载 |
| ``VKMDownload-source.zip`` | 源码快照 |

## 两个提示

**1. Windows 会提示「已保护你的电脑」** —— exe 没有代码签名证书。点「更多信息」->「仍要运行」。

**2. 装扩展前先关掉 IDM 等其它下载器的扩展** —— 都监听同一个下载事件，会互相抢。

## 装浏览器扩展

1. 先启动 VKMDownload
2. 打开 ``brave://extensions``（Edge 用 ``edge://extensions``，Chrome 用 ``chrome://extensions``）
3. 打开右上角「开发者模式」
4. 点「加载已解压的扩展程序」，选择解压出来的 ``extension`` 目录本身
"@
}

# 必须发 UTF-8 字节：直接把字符串交给 -Body 会让中文变成问号。
$payload = @{
    tag_name         = "v$Version"
    target_commitish = 'main'
    name             = "VKMDownload $Version"
    draft            = [bool]$Draft
    prerelease       = $false
    body             = $body
} | ConvertTo-Json -Depth 4 -Compress

Write-Host '创建 release...' -ForegroundColor Cyan
$release = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases" -Method Post `
    -Headers $headers -Body ([System.Text.Encoding]::UTF8.GetBytes($payload)) `
    -ContentType 'application/json; charset=utf-8' -TimeoutSec 60

Write-Host "  $($release.tag_name)  id=$($release.id)" -ForegroundColor Green

# 注意域名：上传走 uploads.github.com，用 api.github.com 会 404。
foreach ($file in Get-ChildItem $stage -File) {
    $url = "https://uploads.github.com/repos/$repo/releases/$($release.id)/assets?name=$([uri]::EscapeDataString($file.Name))"
    $asset = Invoke-RestMethod -Uri $url -Method Post -Headers $headers -InFile $file.FullName `
        -ContentType 'application/octet-stream' -TimeoutSec 600
    Write-Host "  上传 $($asset.name)  $($asset.state)" -ForegroundColor Green
}

Remove-Item $stage, $extStage -Recurse -Force -ErrorAction SilentlyContinue

Write-Host ''
Write-Host $release.html_url -ForegroundColor Cyan
if ($Draft) { Write-Host '（这是草稿，去网页上点发布）' -ForegroundColor Yellow }
