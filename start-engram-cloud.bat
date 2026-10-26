@echo off
REM ============================================================================
REM  start-engram-cloud.bat
REM  Levanta el servidor "engram cloud serve" SIN docker.
REM  Requisitos en esta VM Windows:
REM    - Go 1.25.10 en PATH       (go version)
REM    - PostgreSQL nativo corriendo (psql / createdb en PATH)
REM  Modo: insecure no-auth (desarrollo). NO usar en produccion.
REM ============================================================================

setlocal enabledelayedexpansion
cd /d "%~dp0"

REM ---------------------------------------------------------------------------
REM  CONFIGURACION  (edita estos valores segun tu Postgres)
REM ---------------------------------------------------------------------------
set "PGHOST=127.0.0.1"
set "PGPORT=5432"
set "PGUSER=postgres"
set "PGPASSWORD=postgres"
set "PGDATABASE=engram_cloud"
REM  BD de mantenimiento para crear engram_cloud. 'postgres' existe en EDB.
set "PGMAINTDB=postgres"

REM  Binarios EDB PostgreSQL. Ajusta la version (16/17/18) a la que instalaste.
set "PGBIN=C:\Program Files\PostgreSQL\18\bin"

REM  EDB corre como SERVICIO (arranca solo). PGDATA vacio = no usar pg_ctl/initdb.
set "PGDATA="

REM Servidor cloud
set "ENGRAM_PORT=20001"
set "ENGRAM_CLOUD_HOST=0.0.0.0"
set "ENGRAM_CLOUD_ALLOWED_PROJECTS=engram,infinity-core,infinity-cache-configurator,infinity-deployer-go"
set "ENGRAM_JWT_SECRET=engram-dev-jwt-secret-for-local-smoke-1234"
set "ENGRAM_CLOUD_INSECURE_NO_AUTH=1"

REM Anteponer PGBIN al PATH si se definio
if not "%PGBIN%"=="" set "PATH=%PGBIN%;%PATH%"

REM DSN que usa el binario para conectar a Postgres.
REM  Con password vacio el userinfo es solo el usuario (sin ':').
if "%PGPASSWORD%"=="" (
    set "ENGRAM_DATABASE_URL=postgres://%PGUSER%@%PGHOST%:%PGPORT%/%PGDATABASE%?sslmode=disable"
) else (
    set "ENGRAM_DATABASE_URL=postgres://%PGUSER%:%PGPASSWORD%@%PGHOST%:%PGPORT%/%PGDATABASE%?sslmode=disable"
)

REM ---------------------------------------------------------------------------
REM  1) Verificar Go
REM ---------------------------------------------------------------------------
echo [1/5] Verificando Go...
where go >nul 2>nul
if errorlevel 1 (
    echo ERROR: 'go' no esta en PATH. Instala Go 1.25.10 y reintenta.
    goto :fail
)
go version

REM ---------------------------------------------------------------------------
REM  2) Arrancar Postgres con pg_ctl (solo si PGDATA esta definido)
REM     Postgres portable: no es servicio, hay que arrancarlo a mano.
REM ---------------------------------------------------------------------------
echo [2/5] Arrancando PostgreSQL...
if "%PGDATA%"=="" echo   PGDATA vacio. Asumo que Postgres ya esta corriendo.
if "%PGDATA%"=="" goto :checkpg

where pg_ctl >nul 2>nul
if errorlevel 1 echo ERROR: 'pg_ctl' no esta en PATH. Define PGBIN arriba.
if errorlevel 1 goto :fail

REM Inicializar el cluster si aun no existe (sin postgresql.conf = no inicializado)
if not exist "%PGDATA%\postgresql.conf" echo   Cluster no inicializado. Ejecutando initdb...
if not exist "%PGDATA%\postgresql.conf" initdb -U %PGUSER% -A trust -E UTF8 --locale=C -D "%PGDATA%"
if not exist "%PGDATA%\postgresql.conf" echo ERROR: initdb fallo.
if not exist "%PGDATA%\postgresql.conf" goto :fail

pg_ctl status -D "%PGDATA%" >nul 2>nul
if not errorlevel 1 echo   Postgres ya estaba corriendo.
if not errorlevel 1 goto :checkpg

echo   Iniciando cluster en "%PGDATA%" puerto %PGPORT% ...
pg_ctl start -D "%PGDATA%" -o "-p %PGPORT%" -w
if errorlevel 1 echo ERROR: pg_ctl no pudo arrancar Postgres.
if errorlevel 1 goto :fail

REM ---------------------------------------------------------------------------
REM  3) Verificar conexion y crear la base de datos si no existe
REM     (las TABLAS las crea engram solo al arrancar; la BD debe existir)
REM ---------------------------------------------------------------------------
:checkpg
echo [3/5] Verificando conexion y base '%PGDATABASE%'...
where psql >nul 2>nul
if errorlevel 1 echo ADVERTENCIA: 'psql' no esta en PATH. Asegurate de que la base '%PGDATABASE%' ya existe.
if errorlevel 1 goto :build

REM Comprobar conexion
psql "host=%PGHOST% port=%PGPORT% user=%PGUSER% dbname=%PGMAINTDB% sslmode=disable" -c "SELECT 1;" >nul 2>nul
if errorlevel 1 echo ERROR: No se puede conectar a Postgres. Revisa que este corriendo y las credenciales.
if errorlevel 1 goto :fail

REM Crear BD si falta (idempotente)
psql "host=%PGHOST% port=%PGPORT% user=%PGUSER% dbname=%PGMAINTDB% sslmode=disable" -tAc "SELECT 1 FROM pg_database WHERE datname='%PGDATABASE%';" | findstr "1" >nul
if not errorlevel 1 echo   Base '%PGDATABASE%' ya existe.
if not errorlevel 1 goto :ensureplpgsql
echo   Base '%PGDATABASE%' no existe. Creandola...
psql "host=%PGHOST% port=%PGPORT% user=%PGUSER% dbname=%PGMAINTDB% sslmode=disable" -c "CREATE DATABASE %PGDATABASE%;"
if errorlevel 1 echo ERROR: No se pudo crear la base '%PGDATABASE%'.
if errorlevel 1 goto :fail

REM Asegurar lenguaje plpgsql en engram_cloud (la migracion usa bloques DO $$)
:ensureplpgsql
psql "host=%PGHOST% port=%PGPORT% user=%PGUSER% dbname=%PGDATABASE% sslmode=disable" -c "CREATE EXTENSION IF NOT EXISTS plpgsql;" >nul 2>nul

REM ---------------------------------------------------------------------------
REM  4) Compilar el binario engram
REM ---------------------------------------------------------------------------
:build
echo [4/5] Compilando engram.exe ...
go build -o engram.exe ./cmd/engram
if errorlevel 1 echo ERROR: fallo el build.
if errorlevel 1 goto :fail

REM ---------------------------------------------------------------------------
REM  5) Arrancar el servidor cloud
REM ---------------------------------------------------------------------------
echo [5/5] Arrancando engram cloud serve...
echo   URL local:  http://127.0.0.1:%ENGRAM_PORT%
echo   URL red:    http://^<IP-de-esta-VM^>:%ENGRAM_PORT%
echo   Proyecto(s) permitido(s): %ENGRAM_CLOUD_ALLOWED_PROJECTS%
echo   Ctrl+C para detener.
echo ----------------------------------------------------------------------------
engram.exe cloud serve
goto :eof

:fail
echo.
echo Abortado.
exit /b 1
