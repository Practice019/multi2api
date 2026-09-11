@echo off
REM ============================================================
REM start.bat - double-click to start the workbuddy2api gateway
REM
REM Why this file exists:
REM   When you double-click bin\wb2api-server.exe, Windows sets the
REM   working directory to the exe's own folder (bin\), NOT the repo
REM   root. The program then looks for config.json relative to that
REM   directory, fails, and exits. Even if config.json were found,
REM   its "./auths" and "./data" paths would point at empty folders
REM   under bin\ - which does not error out, it silently starts with
REM   an empty account pool. That is harder to notice than a crash.
REM
REM   This script switches to its own directory (%~dp0 = the folder
REM   containing this .bat) first, so the working directory is always
REM   the repo root no matter where you double-click from.
REM
REM Usage:
REM   Just double-click this file.
REM   To pass extra arguments (e.g. a different config):
REM     start.bat -config other.json
REM
REM NOTE: keep this file ASCII-only. cmd.exe parses .bat content using
REM the OEM code page (936/GBK on Chinese Windows), so a UTF-8 file with
REM non-ASCII characters gets mis-decoded and breaks parsing - it fails
REM with errors like "'json' is not recognized as an internal or
REM external command". Chinese documentation lives in README.md
REM (section "3b. Windows"), not here.
REM ============================================================

setlocal

REM /d also switches drive letters - without it, cd across drives
REM silently does nothing. This line is the whole point of the file.
cd /d "%~dp0"

REM Fail loudly if the expected files are missing, instead of letting
REM the program print one easily-missed line and disappear.
if not exist "config.json" goto no_config

REM Locate the executable. Two layouts must both work:
REM   A) Release package: wb2api-server.exe sits next to this file
REM      (what users get from the GitHub Release zip - extract and run)
REM   B) Source checkout: the binary is built into bin\ per the README
REM Without this check the release package would tell downloaders to
REM "go build it first", which is exactly what they cannot do.
set "EXE="
if exist "wb2api-server.exe" set "EXE=wb2api-server.exe"
if not defined EXE if exist "bin\wb2api-server.exe" set "EXE=bin\wb2api-server.exe"
if not defined EXE goto no_exe

echo Starting workbuddy2api...
echo   working directory: %CD%
echo   config file:       %CD%\config.json
echo   executable:        %CD%\%EXE%
echo.
echo   Close this window or press Ctrl+C to stop the gateway.
echo.

REM Pass arguments through unchanged. With no arguments this is
REM equivalent to "-config config.json", which is already the
REM program's own flag default.
"%EXE%" %*

REM Reaching here means the gateway exited. Keep the window open so
REM the reason is readable - a double-clicked window would otherwise
REM flash and close before the log can be seen.
echo.
echo Gateway exited (code %ERRORLEVEL%).
pause
exit /b %ERRORLEVEL%

:no_config
echo.
echo [ERROR] config.json not found in the working directory.
echo         working directory: %CD%
echo.
echo         Make sure start.bat sits next to config.json
echo         (i.e. in the repository root), then try again.
echo.
pause
exit /b 1

:no_exe
echo.
echo [ERROR] wb2api-server.exe not found.
echo         working directory: %CD%
echo.
echo         Looked in:
echo           %CD%\wb2api-server.exe       (Release package layout)
echo           %CD%\bin\wb2api-server.exe   (source checkout layout)
echo.
echo         If you downloaded a Release package, the .exe should sit
echo         next to this file - re-extract the zip and keep them together.
echo.
echo         If you are running from a source checkout, build it first:
echo           go build -o bin\wb2api-server.exe .\cmd\server
echo.
pause
exit /b 1
