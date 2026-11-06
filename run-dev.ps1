$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot
if ($env:DATABRAIN_DATABASE_MODE -eq 'existing' -and -not $env:APP_SECRET_KEY) {
    throw 'Mode existing memerlukan APP_SECRET_KEY yang sama dengan instalasi sebelumnya.'
}
function Invoke-Checked {
    param([string]$Command, [string[]]$Arguments)
    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Perintah $Command gagal (exit $LASTEXITCODE)." }
}
foreach ($entry in @{
    DATABRAIN_FIXTURE_PASSWORD = 'localsecretpassword'
    DATABRAIN_MIGRATOR_PASSWORD = 'migratorpassword'
    DATABRAIN_RUNTIME_PASSWORD = 'runtimepassword'
    DATABRAIN_TARGET_PASSWORD = 'targetpassword'
    DATABRAIN_POSTGRES_PORT = '55432'
    DATABRAIN_HTTP_PORT = '8080'
    APP_SECRET_KEY = 'kThZ7p4VzQx9mYbL2sW1fR8cE5vN0uI3jG6aD4eA7tM='
    BOOTSTRAP_ADMIN_PASSWORD = 'AdminPassword123!'
    BOOTSTRAP_ADMIN_USERNAME = 'admin'
    BOOTSTRAP_ADMIN_DISPLAY_NAME = 'Administrator'
    TARGET_ALLOWED_HOSTS = '127.0.0.1,localhost,::1'
}.GetEnumerator()) {
    if (-not [Environment]::GetEnvironmentVariable($entry.Key)) {
        [Environment]::SetEnvironmentVariable($entry.Key, $entry.Value)
    }
}
$env:APP_ENV = 'development'
if (-not $env:APP_ORIGIN) { $env:APP_ORIGIN = "http://localhost:$env:DATABRAIN_HTTP_PORT" }
$env:HTTP_ADDR = "127.0.0.1:$env:DATABRAIN_HTTP_PORT"
if (-not $env:DATABRAIN_DATABASE_MODE) { $env:DATABRAIN_DATABASE_MODE = 'docker' }
if ($env:DATABRAIN_DATABASE_MODE -notin @('docker','existing')) { throw 'DATABRAIN_DATABASE_MODE harus docker atau existing.' }
if ($env:DATABRAIN_DATABASE_MODE -eq 'existing' -and (-not $env:PLATFORM_DATABASE_URL -or -not $env:MIGRATOR_DATABASE_URL)) {
    throw 'Mode existing memerlukan PLATFORM_DATABASE_URL dan MIGRATOR_DATABASE_URL.'
}
Invoke-Checked -Command npm.cmd -Arguments @('--prefix','frontend','ci')
Invoke-Checked -Command npm.cmd -Arguments @('--prefix','frontend','run','typecheck')
Invoke-Checked -Command npm.cmd -Arguments @('--prefix','frontend','run','build')
if ($env:DATABRAIN_DATABASE_MODE -eq 'docker') {
    $env:PLATFORM_DATABASE_URL = "postgres://databrain_runtime@127.0.0.1:$env:DATABRAIN_POSTGRES_PORT/databrain_dev?sslmode=disable"
    $env:PLATFORM_DATABASE_PASSWORD = $env:DATABRAIN_RUNTIME_PASSWORD
    $env:MIGRATOR_DATABASE_URL = "postgres://databrain_migrator@127.0.0.1:$env:DATABRAIN_POSTGRES_PORT/databrain_dev?sslmode=disable"
    $env:MIGRATOR_DATABASE_PASSWORD = $env:DATABRAIN_MIGRATOR_PASSWORD
    # Compose hanya menginisialisasi volume kosong; upgrade tidak menjalankan setup.sql.
    Invoke-Checked -Command docker -Arguments @('compose','-f','infra/compose.yaml','stop','app')
    Invoke-Checked -Command docker -Arguments @('compose','-f','infra/compose.yaml','up','-d','--wait','postgres')
}
New-Item -ItemType Directory -Force -Path .tmp | Out-Null
Set-Location backend
$env:CGO_ENABLED = '0'
Invoke-Checked -Command go -Arguments @('build','-o','../.tmp/manage.exe','./cmd/manage')
Invoke-Checked -Command go -Arguments @('build','-o','../.tmp/api.exe','./cmd/api')
Invoke-Checked -Command ../.tmp/manage.exe -Arguments @('migrate','up')
$passwordFile = [IO.Path]::GetTempFileName()
try {
    [IO.File]::WriteAllText($passwordFile, $env:BOOTSTRAP_ADMIN_PASSWORD, [Text.UTF8Encoding]::new($false))
    Invoke-Checked -Command ../.tmp/manage.exe -Arguments @('bootstrap-admin','--username',$env:BOOTSTRAP_ADMIN_USERNAME,
        '--display-name',$env:BOOTSTRAP_ADMIN_DISPLAY_NAME,'--password-file',$passwordFile)
} finally {
    Remove-Item -LiteralPath $passwordFile -Force
}
Write-Output "DataBrain: $env:APP_ORIGIN (admin: $env:BOOTSTRAP_ADMIN_USERNAME)"
Invoke-Checked -Command ../.tmp/api.exe -Arguments @()
