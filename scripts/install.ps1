# name: install-windows
# summary: Install a checksum-verified hrn release on Windows.
# effects: read, exec, network
# parameters: HRN_INSTALL_DIR:path=%LOCALAPPDATA%/hrn; HRN_INSTALL_VERSION:tag=latest; HRN_INSTALL_API:test-url=github; HRN_INSTALL_ARCH:test-arch=processor
# example: out=$(HRN_INSTALL_API=ftp://example.invalid powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/install.ps1 2>&1) && exit 1; test "${out%$'\r'}" = '{"error":"HRN_INSTALL_API must use https or loopback http"}'
# hash: 5f8e92d35066d275fbbda0cc32550cfcaa569debb84fa5773b2dfeaa7459452e

& {
    Set-StrictMode -Version Latest
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'

    function Stop-Install($Message) {
        $failure = [Exception]::new($Message)
        $failure.Data['hrn-install'] = $Message
        throw $failure
    }

    function Read-InstallUrl($Text) {
        if ($Text -isnot [string] -or $Text -cnotmatch '^(https?)://([^/?#]+)' -or
            $Text -match '[\s\p{Cc}\\]') { return }
        $authority = $Matches[2]
        if ($authority -notmatch '^(?:[a-z0-9._-]+|\[[a-f0-9:.]+\])(?::[0-9]+)?$') { return }
        $uri = $null
        if (![Uri]::TryCreate($Text, [UriKind]::Absolute, [ref]$uri)) { return }
        [pscustomobject]@{ Scheme = $uri.Scheme; Authority = $authority; Host = ($authority -replace ':[0-9]+$', '') }
    }

    function Test-AssetUrl($Url, $ApiOrigin, [switch]$Redirect) {
        $assetOrigin = Read-InstallUrl $Url
        if ($null -eq $assetOrigin) { return $false }
        # Match the same origin and GitHub suffixes used by internal/update/release.go's assetURLAllowed.
        if ((!$Redirect -or $ApiOrigin.Scheme -ceq 'http') -and
            $assetOrigin.Scheme -ceq $ApiOrigin.Scheme -and $assetOrigin.Authority -ceq $ApiOrigin.Authority) {
            return $true
        }
        return $assetOrigin.Scheme -ceq 'https' -and
            $assetOrigin.Host -match '(^|\.)(github\.com|githubusercontent\.com)$'
    }

    function Test-VersionTag($Tag) {
        return $Tag -cmatch '\A[a-zA-Z0-9._+-]+\z' -and $Tag -cne 'v' -and !$Tag.Contains('..')
    }

    function Get-InstallResponse($Url, $Accept, $ApiOrigin) {
        for ($redirects = 0; $redirects -le 5; $redirects++) {
            $location = $null
            try {
                # Windows PowerShell can emit a nonterminating error before returning a redirect.
                $response = Invoke-WebRequest -Uri $Url -UseBasicParsing -MaximumRedirection 0 `
                    -Headers @{ Accept = $Accept } -ErrorAction SilentlyContinue
                $status = [int]$response.StatusCode
                if ($status -ge 200 -and $status -lt 300) { return $response }
                $location = $response.Headers['Location'] | Select-Object -First 1
            } catch {
                if (!$_.Exception.PSObject.Properties['Response'] -or !$_.Exception.Response) { throw }
                $response = $_.Exception.Response
                $status = [int]$response.StatusCode
                if ($response -is [Net.HttpWebResponse]) { $location = $response.Headers['Location'] }
                else { $location = $response.Headers.Location }
            }
            if ($status -notin @(301, 302, 303, 307, 308) -or !$location -or $redirects -eq 5 -or
                [string]$location -match '[\s\p{Cc}\\]') {
                throw 'invalid or excessive redirect'
            }
            $target = $null
            if (![Uri]::TryCreate([Uri]$Url, [string]$location, [ref]$target) -or
                !(Test-AssetUrl $target.AbsoluteUri $ApiOrigin -Redirect)) {
                throw 'redirect URL is not allowed'
            }
            $Url = $target.AbsoluteUri
        }
    }

    $temp = $null
    $errorMessage = $null
    try {
        if ($PSVersionTable.PSVersion.Major -eq 5) {
            [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor
                [Net.SecurityProtocolType]::Tls12
        }
        $api = $env:HRN_INSTALL_API
        if (!$api) { $api = 'https://api.github.com/repos/cristiangirlea/harness/releases/latest' }
        $apiOrigin = Read-InstallUrl $api
        if ($null -eq $apiOrigin -or ($apiOrigin.Scheme -eq 'http' -and
                $apiOrigin.Host -cnotin @('127.0.0.1', '[::1]'))) {
            Stop-Install 'HRN_INSTALL_API must use https or loopback http'
        }
        $pin = $env:HRN_INSTALL_VERSION
        if ($pin) {
            if (!(Test-VersionTag $pin)) { Stop-Install 'HRN_INSTALL_VERSION must be a plain version tag' }
            $apiPath = ($api -split '[?#]', 2)[0]
            if (!$apiPath.EndsWith('/releases/latest', [StringComparison]::Ordinal)) {
                Stop-Install 'HRN_INSTALL_API must end in /releases/latest to pin a version'
            }
            $api = $apiPath.Substring(0, $apiPath.Length - '/latest'.Length) + '/tags/' + $pin +
                $api.Substring($apiPath.Length)
        }

        $machine = $env:HRN_INSTALL_ARCH
        if (!$machine) { $machine = $env:PROCESSOR_ARCHITECTURE }
        switch ($machine) {
            'AMD64' { $arch = 'amd64' }
            'ARM64' { $arch = 'arm64' }
            default { Stop-Install "unsupported architecture: $machine" }
        }

        try {
            $response = Get-InstallResponse $api 'application/vnd.github+json' $apiOrigin
            $release = [Text.Encoding]::UTF8.GetString($response.RawContentStream.ToArray()) | ConvertFrom-Json
        } catch { Stop-Install 'could not fetch release' }
        if ($null -eq $release -or !$release.PSObject.Properties['tag_name'] -or
            $release.tag_name -isnot [string] -or !(Test-VersionTag $release.tag_name)) {
            Stop-Install 'release has no plain version tag'
        }
        $tag = $release.tag_name
        if ($pin -and $pin -cne $tag) { Stop-Install "release tag $tag is not the requested $pin" }
        $version = $tag -creplace '^v', ''
        $name = "hrn_$($version)_windows_$arch.exe"
        $binaryUrl = $null
        $sumsUrl = $null
        if ($release.PSObject.Properties['assets']) {
            foreach ($asset in $release.assets) {
                if ($null -eq $asset -or !$asset.PSObject.Properties['name'] -or
                    !$asset.PSObject.Properties['browser_download_url']) { continue }
                switch -CaseSensitive ($asset.name) {
                    $name { $binaryUrl = $asset.browser_download_url }
                    'SHA256SUMS' { $sumsUrl = $asset.browser_download_url }
                }
            }
        }
        if (!$binaryUrl) { Stop-Install "release $tag has no asset $name" }
        if (!$sumsUrl) { Stop-Install "release $tag has no SHA256SUMS" }
        if (!(Test-AssetUrl $binaryUrl $apiOrigin) -or !(Test-AssetUrl $sumsUrl $apiOrigin)) {
            Stop-Install 'asset URL is not allowed'
        }

        try {
            $dir = $env:HRN_INSTALL_DIR
            if (!$dir) { $dir = Join-Path $env:LOCALAPPDATA 'hrn' }
            $dir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($dir)
            $dir = [IO.Directory]::CreateDirectory($dir).FullName.TrimEnd('\', '/')
            if ($dir.EndsWith(':')) { $dir += '\' }
        } catch { Stop-Install 'could not create install directory' }
        $dest = [IO.Path]::Combine($dir, 'hrn.exe')
        $temp = [IO.Path]::Combine($dir, '.hrn-install.' + [Guid]::NewGuid().ToString('N'))
        try {
            $response = Get-InstallResponse $binaryUrl 'application/octet-stream' $apiOrigin
            [IO.File]::WriteAllBytes($temp, $response.RawContentStream.ToArray())
        } catch { Stop-Install "could not download $name" }
        try {
            $response = Get-InstallResponse $sumsUrl 'application/octet-stream' $apiOrigin
            $sums = [Text.Encoding]::UTF8.GetString($response.RawContentStream.ToArray())
        } catch { Stop-Install 'could not download SHA256SUMS' }
        $want = $null
        foreach ($line in ($sums -split '\r?\n')) {
            $fields = $line.Trim() -split '\s+'
            if ($fields.Count -eq 2 -and ($fields[1] -ceq $name -or $fields[1] -ceq "*$name")) {
                $want = $fields[0]
                break
            }
        }
        if (!$want) { Stop-Install "SHA256SUMS does not list $name" }
        if ($want -cnotmatch '\A[0-9a-fA-F]{64}\z') { Stop-Install "SHA256SUMS has no valid digest for $name" }
        try { $digest = (Get-FileHash -LiteralPath $temp -Algorithm SHA256).Hash }
        catch { Stop-Install "could not hash $name" }
        if ($want -ine $digest) { Stop-Install "checksum mismatch for $name" }
        if ([IO.Directory]::Exists($dest)) { Stop-Install 'could not replace hrn.exe' }
        try {
            if ([IO.File]::Exists($dest)) { [IO.File]::Replace($temp, $dest, [NullString]::Value) }
            else { [IO.File]::Move($temp, $dest) }
        }
        catch { Stop-Install 'could not replace hrn.exe; close hrn and run again' }

        $onPath = $false
        foreach ($entry in ($env:PATH -split ';')) {
            if (!$entry) { continue }
            try { $pathDir = [IO.Path]::GetFullPath($entry.Trim('"')).TrimEnd('\', '/') }
            catch { continue }
            if ($pathDir -ieq $dir.TrimEnd('\', '/')) { $onPath = $true; break }
        }
        if (!$onPath) {
            $quoted = "'" + $dir.Replace("'", "''") + "'"
            $hint = 'Add the folder to your user PATH in PowerShell: [Environment]::SetEnvironmentVariable(''Path'', (([Environment]::GetEnvironmentVariable(''Path'', ''User''), {0} | Where-Object {{ $_ }}) -join '';''), ''User'')'
            [Console]::Error.WriteLine(($hint -f $quoted))
        }
        [Console]::Out.WriteLine((@{ installed = $dest; version = $tag; on_path = $onPath } | ConvertTo-Json -Compress))
    } catch {
        $errorMessage = $_.Exception.Data['hrn-install']
        if (!$errorMessage) { $errorMessage = 'could not install hrn.exe' }
    } finally {
        if ($temp) { Remove-Item -LiteralPath $temp -Force -ErrorAction SilentlyContinue }
    }
    if ($errorMessage) {
        [Console]::Error.WriteLine((@{ error = $errorMessage } | ConvertTo-Json -Compress))
        if ($PSCommandPath) { exit 1 }
        $global:LASTEXITCODE = 1
        return
    }
    $global:LASTEXITCODE = 0
}
