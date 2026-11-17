@echo off
rem Runs install.ps1 from the same directory, passing all arguments through.
rem
rem Scripts extracted from a downloaded zip are unsigned and marked as coming
rem from the internet, so PowerShell's default execution policy refuses to run
rem them. This wrapper lifts the policy for this one invocation only.
setlocal

rem When this is started from PowerShell 7, PSModulePath points at its modules,
rem which Windows PowerShell cannot load. Cleared, it is rebuilt from defaults.
set "PSModulePath="

rem fltmc succeeds only in an elevated process.
"%SystemRoot%\System32\fltmc.exe" >nul 2>&1
if errorlevel 1 (
    echo The Tabtender installer needs administrator rights. 1>&2
    echo Run it from an elevated Command Prompt or PowerShell window, 1>&2
    echo or right-click %~nx0 and choose "Run as administrator". 1>&2
    exit /b 1
)

"%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
exit /b %ERRORLEVEL%
