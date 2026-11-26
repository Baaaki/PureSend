# PureSend Installer for Windows (PowerShell)
# Usage: irm https://raw.githubusercontent.com/Baaaki/PureSend/main/install.ps1 | iex

$ErrorActionPreference = "Stop"

$repo = "Baaaki/PureSend"
$binary = "puresend.exe"

# The minisign public key releases are signed with: the same key as the
# FT_UPDATE_KEY repository variable and PUBKEY in install.sh
# (docs/DEPLOYMENT.md); the SHA-256 check below runs either way. Windows
# has no built-in Ed25519 check, so, as in install.sh, the signature is
# checked when minisign is installed.
$pubKey = "RWQ2F1ZFuTGorH4GqU4qC3PzJo5Evx2OKfNfJSiLbgyoEkMFDwUV8Kts"

Write-Host "==> PureSend Windows kurulumu baslatiliyor..." -ForegroundColor Cyan

# 1. Find the latest release. The API allows 60 requests an hour per
# address, which a shared address runs out of, so the redirect behind
# /releases/latest is asked next. A guessed version is not an option: it
# would quietly install an old release.
$tag = $null
try {
    $tag = (Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest").tag_name
} catch {}
if (-not $tag) {
    try {
        $request = [System.Net.HttpWebRequest]::Create("https://github.com/$repo/releases/latest")
        $request.AllowAutoRedirect = $false
        $response = $request.GetResponse()
        $tag = ($response.Headers["Location"] -split "/releases/tag/")[-1]
        $response.Close()
    } catch {}
}
if ($tag -notmatch '^v[0-9]') {
    throw "En son surum bulunamadi (GitHub'a ulasilamadi ya da istek siniri doldu). Biraz sonra tekrar deneyin."
}

$version = $tag.TrimStart("v")
$zipName = "puresend_${version}_windows_x86_64.zip"
$downloadUrl = "https://github.com/$repo/releases/download/$tag/$zipName"

$installDir = "$env:LOCALAPPDATA\PureSend"
$zipPath = "$env:TEMP\$zipName"

Write-Host "==> En son surum indiriliyor: $tag..." -ForegroundColor Cyan
Invoke-WebRequest -Uri $downloadUrl -OutFile $zipPath

# Nothing is installed unchecked: no checksum file, no install.
Write-Host "==> Dosya butunlugu dogrulaniyor..." -ForegroundColor Cyan
$checksumsPath = "$env:TEMP\puresend_checksums_$version.txt"
try {
    Invoke-WebRequest -Uri "https://github.com/$repo/releases/download/$tag/checksums.txt" -OutFile $checksumsPath
} catch {
    Remove-Item -Path $zipPath -Force -ErrorAction SilentlyContinue
    throw "checksums.txt indirilemedi; dogrulanamayan bir dosya kurulmayacak."
}
if ($pubKey) {
    if (Get-Command minisign -ErrorAction SilentlyContinue) {
        $signaturePath = "$checksumsPath.minisig"
        try {
            Invoke-WebRequest -Uri "https://github.com/$repo/releases/download/$tag/checksums.txt.minisig" -OutFile $signaturePath
        } catch {
            Remove-Item -Path $zipPath, $checksumsPath -Force -ErrorAction SilentlyContinue
            throw "Imza dosyasi (checksums.txt.minisig) indirilemedi."
        }
        & minisign -Vq -P $pubKey -m $checksumsPath -x $signaturePath
        $signatureOk = $LASTEXITCODE -eq 0
        Remove-Item -Path $signaturePath -Force -ErrorAction SilentlyContinue
        if (-not $signatureOk) {
            Remove-Item -Path $zipPath, $checksumsPath -Force -ErrorAction SilentlyContinue
            throw "Guvenlik hatasi: checksums.txt imzasi gecersiz!"
        }
        Write-Host "==> Imza dogrulandi." -ForegroundColor Green
    } else {
        Write-Host "Not: minisign kurulu degil; imza denetimi atlaniyor, SHA-256 ozeti yine denetleniyor." -ForegroundColor Yellow
    }
}
$expected = $null
foreach ($line in Get-Content -Path $checksumsPath) {
    $fields = $line.Trim() -split '\s+'
    if ($fields.Count -eq 2 -and $fields[1].TrimStart('*') -eq $zipName) {
        $expected = $fields[0].ToLower()
    }
}
Remove-Item -Path $checksumsPath -Force
if (-not $expected) {
    Remove-Item -Path $zipPath -Force
    throw "checksums.txt icinde $zipName bulunamadi."
}
$actual = (Get-FileHash -Path $zipPath -Algorithm SHA256).Hash.ToLower()
if ($actual -ne $expected) {
    Remove-Item -Path $zipPath -Force
    throw "Guvenlik hatasi: indirilen dosyanin SHA-256 ozeti ($actual) beklenenle ($expected) eslesmiyor!"
}
Write-Host "==> SHA-256 ozeti dogrulandi." -ForegroundColor Green

if (-not (Test-Path $installDir)) {
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
}

Write-Host "==> Dosyalar cikariliyor..." -ForegroundColor Cyan
Expand-Archive -Path $zipPath -DestinationPath $installDir -Force
Remove-Item -Path $zipPath -Force

# Clear Mark-of-the-Web (Zone.Identifier) now that SHA-256 (and minisign,
# when installed) verification has succeeded.
Unblock-File -Path (Join-Path $installDir $binary) -ErrorAction SilentlyContinue

# Add to user PATH if not already present (avoiding leading semicolons on
# empty PATH and false-positive substring matches).
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$entries = @()
if ($userPath) {
    $entries = $userPath -split ';' | Where-Object { $_.Trim() -ne '' }
}
$normInstall = $installDir.TrimEnd('\')
$alreadyInPath = $false
foreach ($entry in $entries) {
    if ($entry.Trim().TrimEnd('\') -ieq $normInstall) {
        $alreadyInPath = $true
        break
    }
}
if (-not $alreadyInPath) {
    Write-Host "==> PATH ortamina ekleniyor: $installDir" -ForegroundColor Cyan
    $newUserPath = ($entries + $installDir) -join ';'
    [Environment]::SetEnvironmentVariable("Path", $newUserPath, "User")
    $env:Path = ($env:Path.TrimEnd(';') + ";" + $installDir).TrimStart(';')
}

Write-Host "==> PureSend ($tag) basariyla kuruldu!" -ForegroundColor Green
Write-Host ""
Write-Host "Kullanim:"
Write-Host "  Arayuzu acmak icin:   puresend"
Write-Host "  Dosya gondermek icin: puresend -send <dosya_veya_klasor>"
Write-Host "  Dosya almak icin:     puresend -receive <kod>"
Write-Host ""
