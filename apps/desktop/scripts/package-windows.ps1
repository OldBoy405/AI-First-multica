# 本地 Windows 打包：一次产出安装版(NSIS .exe) + 免安装版(dist\win-unpacked\Multica.exe)
# 用法：powershell -File apps\desktop\scripts\package-windows.ps1
# 若报 esbuild「平台不匹配」：删 node_modules\.pnpm\@esbuild+win32-x64@* 后
# 在仓库根执行 pnpm install --filter @multica/desktop... 再重跑本脚本。
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")   # → apps/desktop

$env:CSC_IDENTITY_AUTO_DISCOVERY = "false"    # 本机无代码签名证书，跳过签名发现

# -w = win 平台；dir + nsis = 免安装版 + 安装版两个 target
# --config. 必须双横线（单横线 -c 会被 yargs 当成配置文件路径）
pnpm package -w dir nsis -p never --config.win.signAndEditExecutable=false
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "`n=== 产物 (apps\desktop\dist) ===" -ForegroundColor Green
Get-ChildItem dist | Select-Object Name, @{n="MB";e={[math]::Round($_.Length/1MB,1)}} | Format-Table -AutoSize
