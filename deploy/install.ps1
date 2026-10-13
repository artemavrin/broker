<#
.SYNOPSIS
  Установка брокера на Windows Server 2019+ без Docker: PostgreSQL 16,
  брокер как служба Windows, Caddy с сертификатом Let's Encrypt, файрвол.

.DESCRIPTION
  Скрипт идёт по шагам и на каждом проверяет, что уже сделано, поэтому
  повторный запуск безопасен: им же обновляется брокер до новой версии.
  Секреты (JWT_SIGNING_KEY, ADMIN_TOKEN) генерирует сам брокер командой setup
  и никогда не перегенерирует, иначе обнулились бы все выданные токены.

.EXAMPLE
  # Одна строка в PowerShell от имени администратора:
  [Net.ServicePointManager]::SecurityProtocol='Tls12'; iwr https://github.com/artemavrin/broker/releases/latest/download/install.ps1 -OutFile $env:TEMP\broker-install.ps1 -UseBasicParsing; powershell -ExecutionPolicy Bypass -File $env:TEMP\broker-install.ps1

.EXAMPLE
  # Без вопросов: домен задан, версия зафиксирована.
  powershell -ExecutionPolicy Bypass -File .\install.ps1 -Domain 203-0-113-5.sslip.io -Version v1.1.0
#>
[CmdletBinding()]
param(
    # Тег релиза брокера; latest — последний опубликованный.
    [string]$Version = 'latest',
    # Имя для HTTPS. Пусто — спросить (с подсказкой <ip>.sslip.io); '-' — без HTTPS.
    [string]$Domain,
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'broker'),
    [string]$DataDir = (Join-Path $env:ProgramData 'broker'),
    # Пароль суперпользователя postgres, если PostgreSQL уже стоит. Пусто — спросить.
    [string]$PgSuperPassword,
    [string]$PgInstallerVersion = '16.15-1',
    [string]$CaddyVersion = '2.11.7',
    [int]$Port = 8080
)

$ErrorActionPreference = 'Stop'
# Индикатор прогресса в PowerShell 5.1 замедляет Invoke-WebRequest в разы.
$ProgressPreference = 'SilentlyContinue'
# В PowerShell 5.1 по умолчанию TLS 1.0, а GitHub принимает только 1.2+.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
# Брокер пишет в консоль UTF-8; без этого кириллица в его выводе ломается.
$prevOutputEncoding = [Console]::OutputEncoding
[Console]::OutputEncoding = [Text.Encoding]::UTF8

$Repo = 'artemavrin/broker'
$ServiceName = 'broker'
$CaddyServiceName = 'broker-caddy'
# SID вместо имён групп: на русской Windows «Administrators» называется иначе.
$SidAdmins = '*S-1-5-32-544'
$SidSystem = '*S-1-5-18'

$script:stepNo = 0
$TotalSteps = 7
function Step([string]$title) {
    $script:stepNo++
    Write-Host ''
    Write-Host "Шаг $($script:stepNo) из $TotalSteps. $title" -ForegroundColor Cyan
}
function Ok([string]$msg) { Write-Host "  ок    $msg" -ForegroundColor Green }
function Info([string]$msg) { Write-Host "        $msg" }
function Warn([string]$msg) { Write-Host "  внимание: $msg" -ForegroundColor Yellow }

function New-Secret {
    $bytes = New-Object byte[] 24
    [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    # hex: без символов, которые пришлось бы экранировать в URL и в SQL.
    return -join ($bytes | ForEach-Object { $_.ToString('x2') })
}

function Get-Download([string]$url, [string]$out) {
    Info "загрузка $url"
    Invoke-WebRequest -Uri $url -OutFile $out -UseBasicParsing
}

function Assert-Checksum([string]$file, [string]$sumsFile, [string]$algorithm) {
    $name = Split-Path $file -Leaf
    $line = Get-Content $sumsFile | Where-Object { $_ -match ('\s\*?' + [regex]::Escape($name) + '$') } | Select-Object -First 1
    if (-not $line) { throw "в $(Split-Path $sumsFile -Leaf) нет контрольной суммы для $name" }
    $want = ($line -split '\s+')[0]
    $got = (Get-FileHash $file -Algorithm $algorithm).Hash
    if ($got -ne $want) { throw "контрольная сумма $name не совпала: файл повреждён или подменён" }
    Ok "контрольная сумма $name совпала"
}

# Нативные команды не бросают исключений — проверяем код выхода явно.
# Continue внутри: в PowerShell 5.1 при Stop любая строка в stderr нативной
# команды, перенаправленном через 2>, превращается в исключение.
function Invoke-Native([string]$what, [scriptblock]$block) {
    $ErrorActionPreference = 'Continue'
    & $block
    if ($LASTEXITCODE -ne 0) { throw "$what завершилось с кодом $LASTEXITCODE" }
}

function Wait-Http([string]$url, [int]$seconds) {
    $deadline = (Get-Date).AddSeconds($seconds)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 5
            if ($r.StatusCode -eq 200) { return $true }
        } catch { } # ещё не поднялся — повторяем
        Start-Sleep -Seconds 2
    }
    return $false
}

function Read-EnvValue([string]$file, [string]$key) {
    if (-not (Test-Path $file)) { return $null }
    $line = Get-Content $file | Where-Object { $_ -match ('^\s*' + [regex]::Escape($key) + '\s*=') } | Select-Object -First 1
    if (-not $line) { return $null }
    return ($line -split '=', 2)[1].Trim()
}

$tmp = Join-Path $env:TEMP ('broker-install-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
    # ---------------------------------------------------------------------
    Step 'Проверка системы и параметры'

    $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'нужны права администратора: запустите PowerShell через «Запуск от имени администратора»'
    }
    Ok 'права администратора'

    if (-not [Environment]::Is64BitOperatingSystem) { throw 'нужна 64-битная Windows' }
    $os = Get-CimInstance Win32_OperatingSystem
    Ok "$($os.Caption), сборка $($os.BuildNumber), PowerShell $($PSVersionTable.PSVersion)"

    $existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($existing) {
        Info "служба $ServiceName уже установлена ($($existing.Status)) — режим обновления"
    } elseif (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) {
        throw "порт $Port уже занят другой программой; задайте другой через -Port"
    }

    # Все вопросы задаются здесь, чтобы дальше скрипт шёл без участия человека.
    if (-not $Domain) {
        $suggest = $null
        try {
            $ip = (Invoke-RestMethod -Uri 'https://api.ipify.org' -TimeoutSec 10).ToString().Trim()
            $suggest = ($ip -replace '\.', '-') + '.sslip.io'
            Info "внешний IP: $ip"
        } catch { Warn 'не удалось определить внешний IP' }
        Info 'Для HTTPS нужно имя. Без своего домена подойдёт <ip>.sslip.io — оно резолвится в ваш IP.'
        $prompt = 'Домен для HTTPS'
        if ($suggest) { $prompt += " [$suggest]" }
        $answer = Read-Host "$prompt, '-' — без HTTPS"
        if ($answer) { $Domain = $answer.Trim() } elseif ($suggest) { $Domain = $suggest } else { $Domain = '-' }
    }
    if ($Domain -eq '-') {
        Warn 'HTTPS не настраивается: брокер будет доступен только с этой машины (127.0.0.1)'
    } else {
        Ok "домен: $Domain"
    }

    $pgService = Get-Service -Name 'postgresql*' -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($pgService -and -not $PgSuperPassword -and -not (Read-EnvValue (Join-Path $DataDir 'broker.env') 'DATABASE_URL')) {
        $secure = Read-Host "PostgreSQL уже установлен ($($pgService.Name)). Пароль суперпользователя postgres" -AsSecureString
        $PgSuperPassword = [Runtime.InteropServices.Marshal]::PtrToStringAuto([Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure))
    }

    # ---------------------------------------------------------------------
    Step 'Брокер: загрузка релиза'

    if ($Version -eq 'latest') {
        $Version = (Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing).tag_name
    }
    Ok "версия $Version"

    $zipName = "broker_${Version}_windows_amd64.zip"
    $base = "https://github.com/$Repo/releases/download/$Version"
    $zip = Join-Path $tmp $zipName
    $sums = Join-Path $tmp 'checksums.txt'
    try {
        Get-Download "$base/$zipName" $zip
    } catch {
        throw "в релизе $Version нет сборки под Windows ($zipName). Выпустите версию, собранную с windows/amd64."
    }
    Get-Download "$base/checksums.txt" $sums
    Assert-Checksum $zip $sums 'SHA256'

    if ($existing -and $existing.Status -ne 'Stopped') {
        Info 'остановка службы для замены файла'
        Stop-Service -Name $ServiceName
    }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Expand-Archive -Path $zip -DestinationPath $InstallDir -Force
    $brokerExe = Join-Path $InstallDir 'broker.exe'
    Ok "установлен $brokerExe ($(& $brokerExe version))"

    # ---------------------------------------------------------------------
    Step 'PostgreSQL'

    $envFile = Join-Path $DataDir 'broker.env'
    $logDir = Join-Path $DataDir 'logs'
    New-Item -ItemType Directory -Force -Path $DataDir, $logDir | Out-Null
    # Каталог с секретами — только администраторам и SYSTEM; файлы наследуют.
    Invoke-Native 'icacls' { icacls $DataDir /inheritance:r /grant:r "${SidAdmins}:(OI)(CI)F" "${SidSystem}:(OI)(CI)F" | Out-Null }

    if (-not $pgService) {
        $PgSuperPassword = New-Secret
        $installer = Join-Path $tmp "postgresql-$PgInstallerVersion-windows-x64.exe"
        Get-Download "https://get.enterprisedb.com/postgresql/postgresql-$PgInstallerVersion-windows-x64.exe" $installer
        $sig = Get-AuthenticodeSignature $installer
        if ($sig.Status -eq 'Valid') {
            Ok "подпись установщика: $($sig.SignerCertificate.Subject)"
        } elseif ($sig.Status -eq 'NotSigned') {
            Warn 'установщик PostgreSQL без подписи; целостность обеспечена только HTTPS'
        } else {
            throw "подпись установщика PostgreSQL недействительна: $($sig.Status)"
        }
        Info 'установка PostgreSQL в фоновом режиме (несколько минут)…'
        $pgArgs = @(
            '--mode', 'unattended', '--unattendedmodeui', 'none',
            '--superpassword', $PgSuperPassword,
            '--serverport', '5432',
            '--disable-components', 'pgAdmin,stackbuilder'
        )
        $p = Start-Process -FilePath $installer -ArgumentList $pgArgs -Wait -PassThru
        if ($p.ExitCode -ne 0) { throw "установщик PostgreSQL завершился с кодом $($p.ExitCode)" }
        $pgService = Get-Service -Name 'postgresql*' | Select-Object -First 1
        $superFile = Join-Path $DataDir 'postgres-superuser.txt'
        Set-Content -Path $superFile -Value $PgSuperPassword -Encoding ascii
        Ok "PostgreSQL установлен, пароль суперпользователя сохранён в $superFile"
    } else {
        Ok "уже установлен: $($pgService.Name)"
    }
    if ($pgService.Status -ne 'Running') { Start-Service -Name $pgService.Name }
    Set-Service -Name $pgService.Name -StartupType Automatic

    $pgPath = (Get-CimInstance Win32_Service -Filter "Name='$($pgService.Name)'").PathName
    if ($pgPath -notmatch '^"?(.+?\\bin)\\pg_ctl\.exe') { throw "не удалось найти каталог PostgreSQL по пути службы: $pgPath" }
    $psql = Join-Path $Matches[1] 'psql.exe'
    Ok "psql: $psql"

    # ---------------------------------------------------------------------
    Step 'База данных и роль broker'

    # При повторном запуске пароль роли берётся из уже записанного DATABASE_URL.
    $databaseUrl = Read-EnvValue $envFile 'DATABASE_URL'
    if ($databaseUrl) {
        Ok 'DATABASE_URL уже задан в broker.env — база настроена ранее'
    } else {
        if (-not $PgSuperPassword) { throw 'нужен пароль суперпользователя postgres: -PgSuperPassword' }
        $rolePassword = New-Secret
        $env:PGPASSWORD = $PgSuperPassword
        try {
            $roleExists = & $psql -w -h 127.0.0.1 -U postgres -d postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'broker'"
            if ($LASTEXITCODE -ne 0) { throw 'не удалось подключиться к PostgreSQL как postgres: неверный пароль?' }
            if ($roleExists -eq '1') {
                Invoke-Native 'ALTER ROLE' { & $psql -w -h 127.0.0.1 -U postgres -d postgres -v ON_ERROR_STOP=1 -qc "ALTER ROLE broker LOGIN PASSWORD '$rolePassword'" }
                Ok 'роль broker уже была — пароль обновлён'
            } else {
                Invoke-Native 'CREATE ROLE' { & $psql -w -h 127.0.0.1 -U postgres -d postgres -v ON_ERROR_STOP=1 -qc "CREATE ROLE broker LOGIN PASSWORD '$rolePassword'" }
                Ok 'роль broker создана'
            }
            $dbExists = & $psql -w -h 127.0.0.1 -U postgres -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = 'broker'"
            if ($dbExists -eq '1') {
                Ok 'база broker уже существует'
            } else {
                Invoke-Native 'CREATE DATABASE' { & $psql -w -h 127.0.0.1 -U postgres -d postgres -v ON_ERROR_STOP=1 -qc 'CREATE DATABASE broker OWNER broker' }
                Ok 'база broker создана'
            }
        } finally {
            Remove-Item Env:\PGPASSWORD -ErrorAction SilentlyContinue
        }
        # База на этой же машине и наружу не смотрит, поэтому без TLS.
        $databaseUrl = "postgres://broker:$rolePassword@127.0.0.1:5432/broker?sslmode=disable"
    }

    # ---------------------------------------------------------------------
    Step 'Проверка площадки и первичная настройка брокера'

    # Брокер слушает только loopback: наружу его публикует Caddy.
    if (-not (Read-EnvValue $envFile 'HTTP_ADDR')) {
        Add-Content -Path $envFile -Value "HTTP_ADDR=127.0.0.1:$Port" -Encoding ascii
    }
    $env:DATABASE_URL = $databaseUrl
    try {
        Invoke-Native 'broker check' { & $brokerExe check }
        # setup дописывает DATABASE_URL и секреты в файл, существующие не трогает,
        # и создаёт первого инициатора, только если в базе нет ни одного.
        Invoke-Native 'broker setup' { & $brokerExe setup --env-file $envFile }
    } finally {
        Remove-Item Env:\DATABASE_URL -ErrorAction SilentlyContinue
    }

    # ---------------------------------------------------------------------
    Step 'Служба Windows'

    $logFile = Join-Path $logDir 'broker.log'
    $binPath = "`"$brokerExe`" --env-file `"$envFile`" --log-file `"$logFile`""
    if (-not (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) {
        New-Service -Name $ServiceName -BinaryPathName $binPath -DisplayName 'Broker' `
            -Description 'Брокер сообщений' -StartupType Automatic | Out-Null
        Ok "служба $ServiceName создана"
    } else {
        Invoke-Native 'sc config binPath' { sc.exe config $ServiceName binPath= $binPath | Out-Null }
        Ok "служба $ServiceName обновлена"
    }
    # Виртуальная учётная запись службы: без пароля и без прав администратора.
    Invoke-Native 'sc config obj' { sc.exe config $ServiceName obj= "NT SERVICE\$ServiceName" depend= $pgService.Name | Out-Null }
    # Перезапуск при падении, в том числе при выходе с ненулевым кодом.
    Invoke-Native 'sc failure' { sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/5000/restart/30000 | Out-Null }
    Invoke-Native 'sc failureflag' { sc.exe failureflag $ServiceName 1 | Out-Null }
    Invoke-Native 'icacls' { icacls $DataDir /grant "NT SERVICE\${ServiceName}:(OI)(CI)RX" | Out-Null }
    Invoke-Native 'icacls' { icacls $logDir /grant "NT SERVICE\${ServiceName}:(OI)(CI)M" | Out-Null }

    Start-Service -Name $ServiceName
    if (-not (Wait-Http "http://127.0.0.1:$Port/healthz" 30)) {
        Warn "брокер не ответил на /healthz за 30 секунд. Последние строки журнала ($logFile):"
        if (Test-Path $logFile) { Get-Content $logFile -Tail 20 | ForEach-Object { Info $_ } }
        throw 'служба брокера не поднялась'
    }
    Ok "служба запущена, http://127.0.0.1:$Port/healthz отвечает"

    # ---------------------------------------------------------------------
    Step 'HTTPS: Caddy и Let''s Encrypt'

    if ($Domain -eq '-') {
        Info 'пропущено'
    } else {
        $caddyDir = Join-Path $InstallDir 'caddy'
        $caddyExe = Join-Path $caddyDir 'caddy.exe'
        $caddyZipName = "caddy_${CaddyVersion}_windows_amd64.zip"
        $caddyBase = "https://github.com/caddyserver/caddy/releases/download/v$CaddyVersion"
        $caddyZip = Join-Path $tmp $caddyZipName
        $caddySums = Join-Path $tmp 'caddy_checksums.txt'
        Get-Download "$caddyBase/$caddyZipName" $caddyZip
        Get-Download "$caddyBase/caddy_${CaddyVersion}_checksums.txt" $caddySums
        Assert-Checksum $caddyZip $caddySums 'SHA512'

        $caddySvc = Get-Service -Name $CaddyServiceName -ErrorAction SilentlyContinue
        if ($caddySvc -and $caddySvc.Status -ne 'Stopped') { Stop-Service -Name $CaddyServiceName }
        New-Item -ItemType Directory -Force -Path $caddyDir | Out-Null
        Expand-Archive -Path $caddyZip -DestinationPath $caddyDir -Force

        # Caddy понимает пути со слешами — так их не нужно экранировать.
        $caddyData = (Join-Path $DataDir 'caddy') -replace '\\', '/'
        $caddyLog = (Join-Path $logDir 'caddy.log') -replace '\\', '/'
        $caddyfile = Join-Path $DataDir 'Caddyfile'
        # Дашборд /admin/ в интернет не публикуем: он доступен на сервере
        # по http://127.0.0.1:<порт>/admin/.
        $content = @"
{
	storage file_system $caddyData
	log {
		output file $caddyLog
	}
}

$Domain {
	@admin path /admin /admin/*
	respond @admin 403
	reverse_proxy 127.0.0.1:$Port
}
"@
        Set-Content -Path $caddyfile -Value $content -Encoding ascii
        Invoke-Native 'caddy validate' { & $caddyExe validate --config $caddyfile --adapter caddyfile 2>&1 | Out-Null }
        Ok "Caddyfile: $caddyfile"

        $caddyBin = "`"$caddyExe`" run --config `"$caddyfile`" --adapter caddyfile"
        if (-not $caddySvc) {
            New-Service -Name $CaddyServiceName -BinaryPathName $caddyBin -DisplayName 'Broker HTTPS (Caddy)' `
                -Description 'Обратный прокси брокера с TLS' -StartupType Automatic | Out-Null
        } else {
            Invoke-Native 'sc config binPath' { sc.exe config $CaddyServiceName binPath= $caddyBin | Out-Null }
        }
        Invoke-Native 'sc failure' { sc.exe failure $CaddyServiceName reset= 86400 actions= restart/5000/restart/5000/restart/30000 | Out-Null }

        if (-not (Get-NetFirewallRule -Name 'broker-https' -ErrorAction SilentlyContinue)) {
            # 80 нужен Let's Encrypt для проверки владения именем (HTTP-01).
            New-NetFirewallRule -Name 'broker-https' -DisplayName 'Broker HTTPS (Caddy)' -Direction Inbound `
                -Protocol TCP -LocalPort 80, 443 -Action Allow | Out-Null
            Ok 'файрвол: открыты входящие 80 и 443'
        } else {
            Ok 'файрвол: правило broker-https уже есть'
        }

        Start-Service -Name $CaddyServiceName
        Info 'получение сертификата Let''s Encrypt (до 2 минут)…'
        if (Wait-Http "https://$Domain/healthz" 120) {
            Ok "https://$Domain/healthz отвечает"
        } else {
            Warn "https://$Domain/healthz не ответил. Частые причины: порты 80/443 закрыты у хостера,"
            Warn "имя не указывает на этот сервер. Журнал Caddy: $caddyLog"
        }
    }

    # ---------------------------------------------------------------------
    Write-Host ''
    Write-Host 'Готово.' -ForegroundColor Green
    if ($Domain -ne '-') { Info "Адрес брокера:      https://$Domain" }
    Info "Дашборд (на сервере): http://127.0.0.1:$Port/admin/  (токен — ADMIN_TOKEN в $envFile)"
    Info "Настройки и секреты:  $envFile"
    Info "Журнал:               Get-Content '$logFile' -Tail 50 -Wait"
    Info "Состояние:            Get-Service $ServiceName, $CaddyServiceName"
    Info 'Обновление:           запустить этот скрипт ещё раз (можно с -Version vX.Y.Z)'
    Info 'Секрет инициатора печатается выше один раз (шаг 5) — сохраните его.'
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    [Console]::OutputEncoding = $prevOutputEncoding
}
